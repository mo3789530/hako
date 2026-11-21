package workspaceactivity

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/store/transaction"
)

type SweepStats struct {
	Checked   int
	Suspended int
}

// RunIdleSweep finds recently disconnected running allocations and rechecks
// each candidate transactionally. Tenant policy decides whether an operation
// is created; disabled/missing policies are a no-op.
func RunIdleSweep(ctx context.Context, pool transaction.Beginner, now time.Time, limit int) (SweepStats, error) {
	if ctx == nil || pool == nil || now.IsZero() || limit < 1 || limit > 1000 {
		return SweepStats{}, ErrInvalidLease
	}
	candidates, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]IdleCandidate, error) {
		return ListIdleCandidates(ctx, tx, now.UTC().Add(-MinimumIdleGracePeriod), limit)
	})
	if err != nil {
		return SweepStats{}, fmt.Errorf("list idle Workspace candidates: %w", err)
	}
	stats := SweepStats{Checked: len(candidates)}
	for _, candidate := range candidates {
		suspended, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
			return SuspendIdle(ctx, tx, candidate, now.UTC())
		})
		if err != nil {
			return stats, fmt.Errorf("recheck idle Workspace %s: %w", candidate.WorkspaceID, err)
		}
		if suspended {
			stats.Suspended++
		}
	}
	return stats, nil
}
