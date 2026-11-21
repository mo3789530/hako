package previewcleanup

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/store/previews"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var (
	ErrRuntimeClaimFailed        = errors.New("Preview runtime cleanup claim failed")
	ErrRuntimeConfirmationFailed = errors.New("Preview runtime cleanup confirmation failed")
	ErrRuntimeRetryRecordFailed  = errors.New("Preview runtime cleanup retry could not be recorded")
)

type RuntimeExecutor interface {
	// CleanupGeneration removes exactly the bound MicroVM and Gateway route. If
	// the Control Plane has no binding but a provision attempt exists, it may
	// recover the MicroVM only from the Resource Plane journal using the
	// immutable runtime Workspace identity and pinned Plane. It must never scan
	// for or adopt an unjournaled MicroVM, and must never treat caller names as
	// provider IDs.
	CleanupGeneration(context.Context, previews.RuntimeCleanupJob) error
}

// RuntimeWorker cleans retired PR-head generations independently. Bound
// provider IDs and deterministic unbound recovery identities come from the
// store, not from user input.
type RuntimeWorker struct {
	Pool       transaction.Beginner
	Executor   RuntimeExecutor
	Now        func() time.Time
	NewClaimID func() (string, error)
	Lease      time.Duration
	Timeout    time.Duration
	BatchSize  int
}

func (w RuntimeWorker) RunOnce(ctx context.Context) (Stats, error) {
	var stats Stats
	if ctx == nil || ctx.Err() != nil || w.Pool == nil || w.Executor == nil {
		return stats, ErrWorkerConfiguration
	}
	now := w.Now
	if now == nil {
		now = time.Now
	}
	lease, timeout, batchSize := w.Lease, w.Timeout, w.BatchSize
	if lease == 0 {
		lease = 2 * time.Minute
	}
	if timeout == 0 {
		timeout = 90 * time.Second
	}
	if batchSize == 0 {
		batchSize = 1
	}
	if lease < 5*time.Second || lease > 10*time.Minute || timeout < time.Second || timeout >= lease ||
		batchSize < 1 || batchSize > 100 || time.Duration(batchSize)*timeout >= lease-time.Second {
		return stats, ErrWorkerConfiguration
	}
	newClaimID := w.NewClaimID
	if newClaimID == nil {
		newClaimID = func() (string, error) { return idgen.New("preview-runtime-cleanup-") }
	}
	claimID, err := newClaimID()
	if err != nil || len(claimID) < 8 {
		return stats, ErrWorkerConfiguration
	}
	claimedAt := now().UTC()
	jobs, err := transaction.Within(ctx, w.Pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]previews.RuntimeCleanupJob, error) {
		return previews.ClaimRuntimeCleanupDue(ctx, tx, claimID, claimedAt, lease, batchSize)
	})
	if err != nil {
		return stats, ErrRuntimeClaimFailed
	}
	stats.Claimed = len(jobs)
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		startedAt := now().UTC()
		deadline := startedAt.Add(timeout)
		if job.LeaseExpiresAt.Before(deadline) {
			deadline = job.LeaseExpiresAt
		}
		if !deadline.After(startedAt) {
			stats.StaleClaim++
			continue
		}
		var cleanupErr error
		if job.ProviderBound || job.ProvisionAttempts > 0 {
			cleanupCtx, cancel := context.WithDeadline(ctx, deadline)
			cleanupErr = w.Executor.CleanupGeneration(cleanupCtx, job)
			cancel()
		}
		completedAt := now().UTC()
		if cleanupErr == nil {
			confirmed, err := transaction.Within(ctx, w.Pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
				return previews.MarkRuntimeAllocationCleaned(ctx, tx, job, completedAt)
			})
			if err != nil {
				return stats, ErrRuntimeConfirmationFailed
			}
			if confirmed {
				stats.Confirmed++
			} else {
				stats.StaleClaim++
			}
			continue
		}
		updated, err := transaction.Within(ctx, w.Pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
			_, updated, err := previews.RecordRuntimeCleanupFailure(ctx, tx, job, completedAt, classify(cleanupErr))
			return updated, err
		})
		if err != nil {
			return stats, ErrRuntimeRetryRecordFailed
		}
		if updated {
			stats.RetryScheduled++
		} else {
			stats.StaleClaim++
		}
	}
	return stats, nil
}
