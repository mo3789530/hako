package githubwebhook

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
)

const MaxRepositoryEventBatchSize = 100

type BatchPool interface {
	transaction.Beginner
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type BatchStats struct {
	Claimed   int
	Processed int
	Deferred  int
}

// ProcessBatch drains a bounded set of pending Installation repository events
// whose Installation currently has an active Tenant binding. Each delivery is
// applied and marked processed in its own retryable database transaction.
func ProcessBatch(ctx context.Context, pool BatchPool, policy transaction.Policy, batchSize int, processedAt time.Time) (BatchStats, error) {
	var stats BatchStats
	if pool == nil || batchSize < 1 || batchSize > MaxRepositoryEventBatchSize {
		return stats, fmt.Errorf("database pool and batch size between 1 and %d are required", MaxRepositoryEventBatchSize)
	}
	rows, err := pool.Query(ctx, `SELECT d.delivery_id
		FROM github_webhook_deliveries d
		JOIN github_app_installation_bindings b ON b.installation_id = d.installation_id
		JOIN tenant_github_installations i ON i.tenant_id = b.tenant_id AND i.installation_id = b.installation_id
		WHERE d.processing_status = 'received'
		  AND d.event_type = 'installation_repositories'
		  AND d.event_schema_version = $2
		  AND d.installation_id IS NOT NULL
		  AND i.status = 'active'
		ORDER BY d.received_at, d.delivery_id
		LIMIT $1`, batchSize, githubwebhook.RepositoryEventSchemaVersion)
	if err != nil {
		return stats, fmt.Errorf("select pending GitHub repository events: %w", err)
	}
	var deliveryIDs []string
	for rows.Next() {
		var deliveryID string
		if err := rows.Scan(&deliveryID); err != nil {
			rows.Close()
			return stats, fmt.Errorf("scan pending GitHub repository event: %w", err)
		}
		deliveryIDs = append(deliveryIDs, deliveryID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return stats, fmt.Errorf("read pending GitHub repository events: %w", err)
	}
	rows.Close()

	for _, deliveryID := range deliveryIDs {
		stats.Claimed++
		processed, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (bool, error) {
			return ProcessInstallationRepositoryDelivery(ctx, tx, deliveryID, processedAt)
		})
		if errors.Is(err, githubregistry.ErrInstallationNotActive) {
			stats.Deferred++
			continue
		}
		if err != nil {
			return stats, fmt.Errorf("process GitHub repository delivery %s: %w", deliveryID, err)
		}
		if processed {
			stats.Processed++
		}
	}
	return stats, nil
}
