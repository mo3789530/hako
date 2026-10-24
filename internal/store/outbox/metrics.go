package outbox

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/store/transaction"
)

// PendingMetrics contains low-cardinality metrics about durable Outbox lag.
type PendingMetrics struct {
	OldestUnpublishedAgeSeconds float64
	HasUnpublished              bool
}

// ReadPendingMetrics finds the oldest unpublished event through the partial
// index. It deliberately avoids counting or scanning the entire Outbox table.
func ReadPendingMetrics(ctx context.Context, pool transaction.Beginner, policy transaction.Policy, now time.Time) (PendingMetrics, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (PendingMetrics, error) {
		var createdAt time.Time
		err := tx.QueryRow(ctx, `SELECT created_at FROM outbox_events
			WHERE published_at IS NULL ORDER BY created_at, id LIMIT 1`).Scan(&createdAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return PendingMetrics{}, nil
		}
		if err != nil {
			return PendingMetrics{}, err
		}
		age := now.Sub(createdAt).Seconds()
		if age < 0 {
			age = 0
		}
		return PendingMetrics{OldestUnpublishedAgeSeconds: age, HasUnpublished: true}, nil
	})
}
