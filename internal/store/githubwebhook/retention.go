package githubwebhook

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/store/transaction"
)

const MaxRawPayloadCleanupBatch = 1000

// PurgeExpiredRawPayloads erases only the original provider body after its
// fixed retention deadline. The normalized event, payload hash, actor and
// delivery metadata remain available for idempotency, audit and processing.
func PurgeExpiredRawPayloads(ctx context.Context, pool transaction.Beginner, policy transaction.Policy, limit int, now time.Time) (int, error) {
	if pool == nil || limit < 1 || limit > MaxRawPayloadCleanupBatch || now.IsZero() {
		return 0, errors.New("database, bounded cleanup limit, and current time are required")
	}
	now = now.UTC()
	return transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (int, error) {
		tag, err := tx.Exec(ctx, `WITH expired AS (
			SELECT integration_id, delivery_id FROM github_webhook_deliveries
			WHERE payload_purged_at IS NULL AND payload_expires_at <= $1
			ORDER BY payload_expires_at, integration_id, delivery_id
			LIMIT $2
		)
		UPDATE github_webhook_deliveries d
		SET payload_json = '', payload_purged_at = $1
		FROM expired e
		WHERE d.integration_id = e.integration_id AND d.delivery_id = e.delivery_id
		  AND d.payload_purged_at IS NULL`, now, limit)
		if err != nil {
			return 0, fmt.Errorf("purge expired GitHub webhook payloads: %w", err)
		}
		return int(tag.RowsAffected()), nil
	})
}
