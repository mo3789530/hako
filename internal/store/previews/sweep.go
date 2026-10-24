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
		return ExpireDue(ctx, tx, now.UTC(), limit)
	})
	if err != nil {
		return expired, fmt.Errorf("run Preview expiry batch: %w", err)
	}
	return expired, nil
}
