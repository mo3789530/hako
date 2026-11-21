package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/transaction"
)

const MaxStaleOperationBatch = 1000

// FailStale atomically marks old pending/running Operations failed and appends
// one timeout event to each. It deliberately leaves Workspace observed state
// unchanged because expiry does not prove what happened in the Resource Plane.
func FailStale(ctx context.Context, pool transaction.Beginner, policy transaction.Policy, cutoff, now time.Time, limit int) (int, error) {
	if pool == nil {
		return 0, errors.New("transaction pool is required")
	}
	if cutoff.IsZero() || now.IsZero() || cutoff.After(now) {
		return 0, errors.New("stale cutoff must be set and no later than current time")
	}
	if limit < 1 || limit > MaxStaleOperationBatch {
		return 0, fmt.Errorf("stale Operation batch size must be 1-%d", MaxStaleOperationBatch)
	}
	now = now.UTC()
	cutoff = cutoff.UTC()

	return transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (int, error) {
		rows, err := tx.Query(ctx, `SELECT id FROM operations
			WHERE status IN ('pending', 'running') AND updated_at <= $1
			ORDER BY updated_at, id LIMIT $2`, cutoff, limit)
		if err != nil {
			return 0, fmt.Errorf("query stale Operations: %w", err)
		}
		ids := make([]domain.OperationID, 0, limit)
		for rows.Next() {
			var id domain.OperationID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return 0, fmt.Errorf("scan stale Operation: %w", err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, fmt.Errorf("iterate stale Operations: %w", err)
		}
		rows.Close()

		payload, err := json.Marshal(struct {
			ErrorCode string    `json:"error_code"`
			CutoffAt  time.Time `json:"timeout_cutoff"`
		}{ErrorCode: "operation_timeout", CutoffAt: cutoff})
		if err != nil {
			return 0, fmt.Errorf("encode timeout event: %w", err)
		}
		expired := 0
		for _, id := range ids {
			tag, err := tx.Exec(ctx, `UPDATE operations
				SET status = 'failed', error_code = 'operation_timeout', updated_at = $1
				WHERE id = $2 AND status IN ('pending', 'running') AND updated_at <= $3`, now, id, cutoff)
			if err != nil {
				return 0, fmt.Errorf("expire stale Operation %s: %w", id, err)
			}
			if tag.RowsAffected() == 0 {
				// A result or another sweeper won the compare-and-swap.
				continue
			}
			if err := appendResultEvent(ctx, tx, id, "operation.timed_out", payload, now); err != nil {
				return 0, err
			}
			expired++
		}
		return expired, nil
	})
}
