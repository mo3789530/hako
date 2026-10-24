// Package previewcleanup executes the physical cleanup claimed by the
// Control Plane Preview lifecycle store. The executor must be idempotent and
// must return nil only after both the MicroVM and Gateway route are absent.
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
	ErrBindingMissing       = errors.New("Preview runtime binding is missing")
	ErrGatewayRouteRemoval  = errors.New("Preview Gateway route removal failed")
	ErrMicroVMTermination   = errors.New("Preview MicroVM termination failed")
	ErrProviderUnavailable  = errors.New("Preview runtime provider is unavailable")
	ErrWorkerConfiguration  = errors.New("Preview cleanup worker configuration is invalid")
	ErrClaimFailed          = errors.New("Preview cleanup claim failed")
	ErrConfirmationFailed   = errors.New("Preview cleanup confirmation failed")
	ErrRetryRecordingFailed = errors.New("Preview cleanup retry recording failed")
)

type Executor interface {
	// Cleanup must dispatch through the authorized Resource Plane boundary,
	// derive runtime identity from durable binding, and be safe to repeat for the
	// same immutable Preview generation. It returns nil only after the Resource
	// Plane confirms that the route and MicroVM are absent; the Control Plane
	// worker must not call provider APIs directly.
	Cleanup(context.Context, previews.CleanupJob) error
}

type Stats struct {
	Claimed        int
	Confirmed      int
	RetryScheduled int
	StaleClaim     int
}

type Worker struct {
	Pool       transaction.Beginner
	Executor   Executor
	Now        func() time.Time
	NewClaimID func() (string, error)
	Lease      time.Duration
	Timeout    time.Duration
	BatchSize  int
}

func (w Worker) RunOnce(ctx context.Context) (Stats, error) {
	var stats Stats
	if ctx == nil || ctx.Err() != nil || w.Pool == nil || w.Executor == nil {
		return stats, ErrWorkerConfiguration
	}
	now := w.Now
	if now == nil {
		now = time.Now
	}
	lease := w.Lease
	if lease == 0 {
		lease = 2 * time.Minute
	}
	timeout := w.Timeout
	if timeout == 0 {
		timeout = 90 * time.Second
	}
	batchSize := w.BatchSize
	if batchSize == 0 {
		batchSize = 1
	}
	if lease < 5*time.Second || lease > 10*time.Minute || timeout < time.Second || timeout >= lease ||
		batchSize < 1 || batchSize > 100 || time.Duration(batchSize)*timeout >= lease-time.Second {
		return stats, ErrWorkerConfiguration
	}
	newClaimID := w.NewClaimID
	if newClaimID == nil {
		newClaimID = func() (string, error) { return idgen.New("preview-cleanup-") }
	}
	claimID, err := newClaimID()
	if err != nil || len(claimID) < 8 {
		return stats, ErrWorkerConfiguration
	}
	claimedAt := now().UTC()
	jobs, err := transaction.Within(ctx, w.Pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]previews.CleanupJob, error) {
		return previews.ClaimDue(ctx, tx, claimID, claimedAt, lease, batchSize)
	})
	if err != nil {
		return stats, ErrClaimFailed
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
		cleanupCtx, cancel := context.WithDeadline(ctx, deadline)
		cleanupErr := w.Executor.Cleanup(cleanupCtx, job)
		cancel()
		completedAt := now().UTC()
		if cleanupErr == nil {
			confirmed, err := transaction.Within(ctx, w.Pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
				return previews.MarkCleaned(ctx, tx, job, completedAt)
			})
			if err != nil {
				return stats, ErrConfirmationFailed
			}
			if confirmed {
				stats.Confirmed++
			} else {
				stats.StaleClaim++
			}
			continue
		}
		code := classify(cleanupErr)
		_, updated, err := recordFailure(ctx, w.Pool, job, completedAt, code)
		if err != nil {
			return stats, ErrRetryRecordingFailed
		}
		if updated {
			stats.RetryScheduled++
		} else {
			stats.StaleClaim++
		}
	}
	return stats, nil
}

func recordFailure(ctx context.Context, pool transaction.Beginner, job previews.CleanupJob, now time.Time, code string) (time.Time, bool, error) {
	result, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct {
		retry time.Time
		ok    bool
	}, error) {
		retryAt, updated, err := previews.RecordCleanupFailure(ctx, tx, job, now, code)
		return struct {
			retry time.Time
			ok    bool
		}{retryAt, updated}, err
	})
	return result.retry, result.ok, err
}

func classify(err error) string {
	switch {
	case errors.Is(err, ErrBindingMissing):
		return "preview_binding_missing"
	case errors.Is(err, ErrGatewayRouteRemoval):
		return "gateway_route_remove_failed"
	case errors.Is(err, ErrMicroVMTermination):
		return "microvm_terminate_failed"
	default:
		return "provider_unavailable"
	}
}
