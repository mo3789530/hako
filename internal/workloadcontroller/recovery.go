package workloadcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

// RecoveryJournalLister is implemented by the Resource Plane's durable
// journal index. Pages must be bounded and contain only attempts requiring
// reconciliation; callers should begin each scheduled sweep with an empty
// continuation token so failed records are naturally retried next time.
type RecoveryJournalLister interface {
	ListRecoveryPage(context.Context, string, int32) ([]workloadjournal.Snapshot, string, error)
}

type RecoveryFailure struct {
	TenantID      string
	WorkloadRunID string
	AttemptID     string
	// Err may wrap provider/store details; callers should emit a safe error
	// code rather than logging the raw value alongside frozen journal data.
	Err error
}

type RecoveryReport struct {
	Scanned int
	Handled int
	Next    string
	Failed  []RecoveryFailure
}

type RecoverySweepReport struct {
	Pages             int
	Scanned           int
	Handled           int
	FailedCount       int
	Failed            []RecoveryFailure
	Complete          bool
	LeaseAcquired     bool
	ContinuationToken string
}

type RecoveryLeaseManager interface {
	Acquire(context.Context, string, time.Time, time.Duration) (workloadjournal.RecoveryLease, error)
	Renew(context.Context, workloadjournal.RecoveryLease, time.Time, time.Duration) (workloadjournal.RecoveryLease, error)
	Release(context.Context, workloadjournal.RecoveryLease, time.Time) error
}

const maxRecoveryFailureDetails = 1000

// RecoverPage replays a bounded page through the same idempotent Poll path as
// queue delivery. It is a library primitive only: scheduling, leases, alarms,
// and production S3 permissions remain deployment responsibilities. Start a
// fresh sweep periodically rather than persisting a cursor across invocations.
func (c *Controller) RecoverPage(ctx context.Context, lister RecoveryJournalLister, continuationToken string, limit int32, publisher ResultPublisher) (RecoveryReport, error) {
	if c == nil || ctx == nil || lister == nil || publisher == nil || limit < 1 || limit > 1000 {
		return RecoveryReport{}, ErrInvalidCommand
	}
	page, next, err := lister.ListRecoveryPage(ctx, continuationToken, limit)
	if err != nil {
		return RecoveryReport{}, err
	}
	report := RecoveryReport{Scanned: len(page), Next: next, Failed: make([]RecoveryFailure, 0)}
	for _, snapshot := range page {
		j := snapshot.Journal
		if snapshot.ETag == "" || j.Validate() != nil || j.ResourcePlaneID != c.acceptor.resourcePlaneID {
			report.Failed = append(report.Failed, RecoveryFailure{TenantID: j.TenantID, WorkloadRunID: j.WorkloadRunID, AttemptID: j.AttemptID, Err: workloadjournal.ErrConflict})
			continue
		}
		command := Command{SchemaVersion: 1, TenantID: j.TenantID, WorkloadRunID: j.WorkloadRunID,
			AttemptID: j.AttemptID, Generation: j.Generation, ResourcePlaneID: j.ResourcePlaneID,
			IdempotencyToken: j.IdempotencyToken}
		data, err := json.Marshal(command)
		if err == nil {
			_, err = c.Poll(ctx, data, publisher)
		}
		if err != nil {
			report.Failed = append(report.Failed, RecoveryFailure{TenantID: j.TenantID, WorkloadRunID: j.WorkloadRunID, AttemptID: j.AttemptID, Err: err})
			continue
		}
		report.Handled++
	}
	return report, nil
}

// RecoverSweep walks a bounded number of S3 pages and isolates per-attempt
// failures so one malformed/stuck record cannot block later journals. The
// continuation token is an optimization for continuing this sweep only; it
// is not a durable work claim. Periodic sweeps must restart with an empty
// token to revisit transiently failed attempts.
func (c *Controller) RecoverSweep(ctx context.Context, lister RecoveryJournalLister, pageSize int32, maxPages int, publisher ResultPublisher) (RecoverySweepReport, error) {
	if c == nil || ctx == nil || lister == nil || publisher == nil || pageSize < 1 || pageSize > 1000 || maxPages < 1 || maxPages > 1000 {
		return RecoverySweepReport{}, ErrInvalidCommand
	}
	report := RecoverySweepReport{Failed: make([]RecoveryFailure, 0)}
	continuation := ""
	seenTokens := make(map[string]struct{}, maxPages)
	for report.Pages < maxPages {
		if err := ctx.Err(); err != nil {
			report.ContinuationToken = continuation
			return report, err
		}
		page, err := c.RecoverPage(ctx, lister, continuation, pageSize, publisher)
		if err != nil {
			report.ContinuationToken = continuation
			return report, err
		}
		report.Pages++
		report.Scanned += page.Scanned
		report.Handled += page.Handled
		report.FailedCount += len(page.Failed)
		remainingDetails := maxRecoveryFailureDetails - len(report.Failed)
		if remainingDetails > 0 {
			if remainingDetails > len(page.Failed) {
				remainingDetails = len(page.Failed)
			}
			report.Failed = append(report.Failed, page.Failed[:remainingDetails]...)
		}
		if page.Next == "" {
			report.Complete = true
			return report, nil
		}
		if _, exists := seenTokens[page.Next]; exists || page.Next == continuation {
			report.ContinuationToken = continuation
			return report, ErrInvalidCommand
		}
		seenTokens[page.Next] = struct{}{}
		continuation = page.Next
	}
	report.ContinuationToken = continuation
	return report, nil
}

// RecoverSweepWithLease prevents overlapping scheduled scans. The lease is
// renewed while pages are processed; losing it cancels the scan so a second
// worker can take over after expiry. The caller should use a unique owner ID
// per invocation and a lease key isolated from the journal listing prefix.
func (c *Controller) RecoverSweepWithLease(ctx context.Context, lister RecoveryJournalLister, leases RecoveryLeaseManager,
	pageSize int32, maxPages int, leaseTTL time.Duration, publisher ResultPublisher) (RecoverySweepReport, error) {
	if c == nil || ctx == nil || lister == nil || leases == nil || publisher == nil ||
		pageSize < 1 || pageSize > 1000 || maxPages < 1 || maxPages > 1000 || leaseTTL < time.Second || leaseTTL > 30*time.Minute {
		return RecoverySweepReport{}, ErrInvalidCommand
	}
	owner, err := idgen.New("recovery_")
	if err != nil {
		return RecoverySweepReport{}, err
	}
	lease, err := leases.Acquire(ctx, owner, time.Now().UTC(), leaseTTL)
	if errors.Is(err, workloadjournal.ErrRecoveryLeaseHeld) {
		return RecoverySweepReport{}, nil
	}
	if err != nil {
		return RecoverySweepReport{}, err
	}
	childCtx, cancel := context.WithCancel(ctx)
	stopRenewal := make(chan struct{})
	renewalDone := make(chan struct{})
	renewalErr := make(chan error, 1)
	interval := leaseTTL / 3
	go func() {
		defer close(renewalDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopRenewal:
				return
			case <-childCtx.Done():
				return
			case <-ticker.C:
				next, renewErr := leases.Renew(childCtx, lease, time.Now().UTC(), leaseTTL)
				if renewErr != nil {
					renewalErr <- renewErr
					cancel()
					return
				}
				lease = next
			}
		}
	}()

	report, sweepErr := c.RecoverSweep(childCtx, lister, pageSize, maxPages, publisher)
	close(stopRenewal)
	<-renewalDone
	cancel()
	report.LeaseAcquired = true
	select {
	case err = <-renewalErr:
		sweepErr = errors.Join(sweepErr, err)
	default:
	}
	releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	releaseErr := leases.Release(releaseCtx, lease, time.Now().UTC())
	releaseCancel()
	if releaseErr != nil {
		sweepErr = errors.Join(sweepErr, releaseErr)
	}
	return report, sweepErr
}
