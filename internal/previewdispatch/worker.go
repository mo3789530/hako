// Package previewdispatch claims eligible Preview runtime generations and
// commits their Resource Plane commands to the transactional Outbox.
package previewdispatch

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
	ErrWorkerConfiguration = errors.New("Preview command producer configuration is invalid")
	ErrEnqueueFailed       = errors.New("Preview runtime command enqueue failed")
)

var resourcePlaneIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)

type Stats struct {
	Claimed  int
	Enqueued int
}

// Worker does database-only work. It atomically pins/reserves the generation,
// leases it, and writes the command to Outbox; the generic Dispatcher publishes
// only after this transaction commits.
type Worker struct {
	Pool                  transaction.Beginner
	TargetResourcePlaneID domain.ResourcePlaneID
	RuntimeClass          string
	Now                   func() time.Time
	NewClaimID            func() (string, error)
	NewEventID            func() (string, error)
	Lease                 time.Duration
	BatchSize             int
}

func (w Worker) RunOnce(ctx context.Context) (Stats, error) {
	var stats Stats
	if ctx == nil || ctx.Err() != nil || w.Pool == nil ||
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
	batchSize := w.BatchSize
	if batchSize == 0 {
		batchSize = 1
	}
	if lease < 5*time.Second || lease > 10*time.Minute || batchSize < 1 || batchSize > 100 {
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
	newEventID := w.NewEventID
	if newEventID == nil {
		newEventID = func() (string, error) { return idgen.New("evt_") }
	}
	eventIDs := make([]string, batchSize)
	for i := range eventIDs {
		eventIDs[i], err = newEventID()
		if err != nil || eventIDs[i] == "" {
			return stats, ErrWorkerConfiguration
		}
	}
	claimedAt := now().UTC()
	jobs, err := transaction.Within(ctx, w.Pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]previews.ProvisionJob, error) {
		return previews.ClaimRuntimeAllocationsAndEnqueue(ctx, tx, w.TargetResourcePlaneID, runtimeClass, claimID, eventIDs, claimedAt, lease, batchSize)
	})
	if err != nil {
		return stats, ErrEnqueueFailed
	}
	stats.Claimed = len(jobs)
	stats.Enqueued = len(jobs)
	return stats, nil
}
