package githubregistry

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/store/transaction"
)

const TenantWebhookRateWindow = time.Minute

// AllowWebhookAttempt stores one bounded counter row per active integration.
// Unknown or disabled route IDs create no rows. The UPSERT is atomic across API
// instances; OCC conflicts are retried by transaction.Within.
func AllowWebhookAttempt(ctx context.Context, pool transaction.Beginner, policy transaction.Policy, integrationID string, now time.Time, limit int) (bool, error) {
	if pool == nil || !validOpaqueMetadata(integrationID, 128) || now.IsZero() || limit < 1 || limit > 100000 {
		return false, ErrAppBindingDenied
	}
	cutoff := now.UTC().Add(-TenantWebhookRateWindow)
	return transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		var attempts int
		err := tx.QueryRow(ctx, `INSERT INTO github_webhook_rate_limits (integration_id,window_started_at,attempts)
			SELECT id,$2,1 FROM tenant_github_integrations WHERE id=$1 AND status='active'
			ON CONFLICT (integration_id) DO UPDATE SET
			window_started_at=CASE WHEN github_webhook_rate_limits.window_started_at <= $3 THEN EXCLUDED.window_started_at ELSE github_webhook_rate_limits.window_started_at END,
			attempts=CASE WHEN github_webhook_rate_limits.window_started_at <= $3 THEN 1 ELSE github_webhook_rate_limits.attempts + 1 END
			RETURNING attempts`, integrationID, now.UTC(), cutoff).Scan(&attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return attempts <= limit, nil
	})
}
