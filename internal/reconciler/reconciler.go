// Package reconciler compares Workspace Desired and Observed State and
// records corrective Operations through the Transactional Outbox.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

type Candidate struct {
	WorkspaceID domain.WorkspaceID
	Desired     domain.DesiredWorkspaceState
	Observed    domain.ObservedWorkspaceState
	UpdatedAt   time.Time
}

type Store interface {
	FailStaleOperations(context.Context, time.Time, time.Time, int) (int, error)
	Candidates(context.Context, int, time.Time, time.Duration) ([]Candidate, error)
	EnsureOperation(context.Context, Candidate, domain.OperationType, time.Time, time.Duration) (bool, error)
}

type Config struct {
	BatchSize        int
	FailureDelay     time.Duration
	OperationTimeout time.Duration
	Now              func() time.Time
}

type Reconciler struct {
	store  Store
	config Config
}

type Stats struct {
	Checked  int
	Created  int
	Skipped  int
	Failed   int
	TimedOut int
}

func New(store Store, config Config) (*Reconciler, error) {
	if store == nil {
		return nil, errors.New("reconciler store is required")
	}
	if config.BatchSize == 0 {
		config.BatchSize = 100
	}
	if config.BatchSize < 1 || config.BatchSize > 1000 {
		return nil, errors.New("reconciler batch size must be 1-1000")
	}
	if config.FailureDelay == 0 {
		config.FailureDelay = time.Minute
	}
	if config.FailureDelay < time.Second {
		return nil, errors.New("reconciler failure delay must be at least one second")
	}
	if config.OperationTimeout == 0 {
		config.OperationTimeout = 30 * time.Minute
	}
	if config.OperationTimeout < time.Second {
		return nil, errors.New("reconciler Operation timeout must be at least one second")
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Reconciler{store: store, config: config}, nil
}

// RunOnce first expires stale active Operations, then examines a bounded set
// of state mismatches. A recent terminal Operation applies a cooldown; active
// Operations and concurrent reconciler claims are guarded by the store.
func (r *Reconciler) RunOnce(ctx context.Context) (Stats, error) {
	var stats Stats
	now := r.config.Now().UTC()
	expired, err := r.store.FailStaleOperations(ctx, now.Add(-r.config.OperationTimeout), now, r.config.BatchSize)
	if err != nil {
		return stats, fmt.Errorf("expire stale Operations: %w", err)
	}
	stats.TimedOut = expired
	candidates, err := r.store.Candidates(ctx, r.config.BatchSize, now, r.config.FailureDelay)
	if err != nil {
		return stats, fmt.Errorf("list Workspace reconciliation candidates: %w", err)
	}
	stats.Checked = len(candidates)
	var failures []error
	for _, candidate := range candidates {
		action, needed := operationFor(candidate.Desired, candidate.Observed)
		if !needed {
			stats.Skipped++
			continue
		}
		created, err := r.store.EnsureOperation(ctx, candidate, action, now, r.config.FailureDelay)
		if err != nil {
			stats.Failed++
			failures = append(failures, fmt.Errorf("reconcile Workspace %s: %w", candidate.WorkspaceID, err))
			continue
		}
		if created {
			stats.Created++
		} else {
			stats.Skipped++
		}
	}
	return stats, errors.Join(failures...)
}

func operationFor(desired domain.DesiredWorkspaceState, observed domain.ObservedWorkspaceState) (domain.OperationType, bool) {
	if desired == "" || observed == "" || string(desired) == string(observed) {
		return "", false
	}
	switch desired {
	case domain.DesiredWorkspaceRunning:
		if observed == domain.ObservedWorkspaceSuspended {
			return domain.OperationResume, true
		}
		return domain.OperationEnsureRunning, true
	case domain.DesiredWorkspaceSuspended:
		return domain.OperationSuspend, true
	case domain.DesiredWorkspaceDeleted:
		return domain.OperationDelete, true
	default:
		return "", false
	}
}
