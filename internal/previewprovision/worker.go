// Package previewprovision executes claimed Preview runtime allocations via an
// injected Resource Plane provisioner. It never calls provider APIs directly.
package previewprovision

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/store/previews"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var (
	ErrRuntimeProfileMissing = errors.New("Preview runtime profile is missing")
	ErrMicroVMLaunch         = errors.New("Preview MicroVM launch failed")
	ErrGatewayRoutePublish   = errors.New("Preview Gateway route publish failed")
	ErrProviderUnavailable   = errors.New("Preview Resource Plane is unavailable")
	ErrWorkerConfiguration   = errors.New("Preview provision worker configuration is invalid")
	ErrClaimFailed           = errors.New("Preview runtime claim failed")
	ErrFailureRecording      = errors.New("Preview runtime failure could not be recorded")
	ErrBindingRecording      = errors.New("Preview runtime binding could not be recorded")
	ErrCompensationFailed    = errors.New("stale Preview runtime could not be compensated")
)

var resourcePlaneIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)

// Provisioner routes all mutations through the authorized Resource Plane. It
// must be idempotent for a Preview generation. Compensate runs after every
// failed Provision, including when no provider IDs were returned; it must
// rediscover unbound resources using the immutable RuntimeWorkspaceID and Hako
// tags. It must accept partial bindings and be safe if resources are already
// gone.
type Provisioner interface {
	Provision(context.Context, previews.ProvisionJob) (previews.RuntimeBinding, error)
	Compensate(context.Context, previews.ProvisionJob, previews.RuntimeBinding) error
}

type Stats struct {
	Claimed        int
	Bound          int
	RetryScheduled int
	StaleClaim     int
	Compensated    int
}

type Worker struct {
	Pool                  transaction.Beginner
	Provisioner           Provisioner
	TargetResourcePlaneID domain.ResourcePlaneID
	RuntimeClass          string
	Now                   func() time.Time
	NewClaimID            func() (string, error)
	Lease                 time.Duration
	Timeout               time.Duration
	BatchSize             int
}

func (w Worker) RunOnce(ctx context.Context) (Stats, error) {
	var stats Stats
	if ctx == nil || ctx.Err() != nil || w.Pool == nil || w.Provisioner == nil ||
		(w.TargetResourcePlaneID == "" && w.RuntimeClass == "") ||
		(w.TargetResourcePlaneID != "" && !resourcePlaneIDPattern.MatchString(string(w.TargetResourcePlaneID))) {
		return stats, ErrWorkerConfiguration
	}
	runtimeClass := w.RuntimeClass
	if runtimeClass == "" {
		runtimeClass = "standard"
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
		newClaimID = func() (string, error) { return idgen.New("preview-provision-") }
	}
	claimID, err := newClaimID()
	if err != nil || len(claimID) < 8 {
		return stats, ErrWorkerConfiguration
	}
	claimedAt := now().UTC()
	jobs, err := transaction.Within(ctx, w.Pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]previews.ProvisionJob, error) {
		if w.TargetResourcePlaneID != "" {
			return previews.ClaimRuntimeAllocationsOnPlane(ctx, tx, w.TargetResourcePlaneID, runtimeClass, claimID, claimedAt, lease, batchSize)
		}
		return previews.ClaimRuntimeAllocationsScheduled(ctx, tx, claimID, claimedAt, lease, batchSize, runtimeClass)
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
		provisionCtx, cancel := context.WithDeadline(ctx, deadline)
		binding, provisionErr := w.Provisioner.Provision(provisionCtx, job)
		cancel()
		completedAt := now().UTC()
		if provisionErr != nil {
			compensationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
			compensationErr := w.Provisioner.Compensate(compensationCtx, job, binding)
			cancel()
			if compensationErr != nil {
				return stats, ErrCompensationFailed
			}
			stats.Compensated++
			_, updated, err := recordFailure(ctx, w.Pool, job, completedAt, classify(provisionErr))
			if err != nil {
				return stats, ErrFailureRecording
			}
			if updated {
				stats.RetryScheduled++
			} else {
				stats.StaleClaim++
			}
			continue
		}
		bound, err := transaction.Within(ctx, w.Pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
			return previews.BindRuntimeAllocation(ctx, tx, job, binding, completedAt)
		})
		if err != nil || !bound {
			compensationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
			compensationErr := w.Provisioner.Compensate(compensationCtx, job, binding)
			cancel()
			if compensationErr != nil {
				return stats, ErrCompensationFailed
			}
			stats.Compensated++
			if err != nil {
				return stats, ErrBindingRecording
			}
			stats.StaleClaim++
			continue
		}
		stats.Bound++
	}
	return stats, nil
}

func recordFailure(ctx context.Context, pool transaction.Beginner, job previews.ProvisionJob, now time.Time, code string) (time.Time, bool, error) {
	result, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct {
		retry time.Time
		ok    bool
	}, error) {
		retryAt, updated, err := previews.RecordRuntimeAllocationFailure(ctx, tx, job, now, code)
		return struct {
			retry time.Time
			ok    bool
		}{retryAt, updated}, err
	})
	return result.retry, result.ok, err
}

func classify(err error) string {
	switch {
	case errors.Is(err, ErrRuntimeProfileMissing):
		return "runtime_profile_missing"
	case errors.Is(err, ErrMicroVMLaunch):
		return "microvm_launch_failed"
	case errors.Is(err, ErrGatewayRoutePublish):
		return "gateway_route_publish_failed"
	default:
		return "provider_unavailable"
	}
}
