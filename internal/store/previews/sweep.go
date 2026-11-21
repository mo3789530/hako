package previews

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/store/transaction"
)

// ExpireBatch performs one OCC-retried, bounded expiration transaction.
func ExpireBatch(ctx context.Context, pool transaction.Beginner, now time.Time, limit int) (int64, error) {
	if ctx == nil || pool == nil || now.IsZero() || limit < 1 || limit > 500 {
		return 0, fmt.Errorf("invalid Preview expiry sweep")
	}
	expired, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (int64, error) {
		if _, err := tx.Exec(ctx, `WITH due AS (
			SELECT id FROM repository_preview_sessions WHERE expires_at <= $1
			ORDER BY expires_at,id LIMIT $2
		)
		DELETE FROM repository_preview_sessions s USING due WHERE s.id=due.id`, now.UTC(), limit); err != nil {
			return 0, fmt.Errorf("delete expired Preview sessions: %w", err)
		}
		return ExpireDue(ctx, tx, now.UTC(), limit)
	})
	if err != nil {
		return expired, fmt.Errorf("run Preview expiry batch: %w", err)
	}
	return expired, nil
}
