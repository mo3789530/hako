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
	Claimed                   int
	Processed                 int
	WorkloadRunsCreated       int
	RepositoryPreviewsUpdated int
	ActionsRunnerJobsAdmitted int
	Deferred                  int
	RawPayloadsPurged         int
}

// ProcessBatch drains a bounded set of pending Installation repository events
// whose Installation currently has an active Tenant binding. Each delivery is
// applied and marked processed in its own retryable database transaction.
func ProcessBatch(ctx context.Context, pool BatchPool, policy transaction.Policy, batchSize int, processedAt time.Time) (BatchStats, error) {
	return ProcessBatchWithRepositoryTriggers(ctx, pool, policy, batchSize, processedAt, nil)
}

// ProcessBatchWithRepositoryTriggers additionally drains push/pull-request
// events. Workflow Job events are deliberately excluded: they require a
// separately authorized ephemeral-runner policy and must never be converted
// into a generic test Run. The run creator is injected to keep inbox storage
// independent of the Workload Run store while sharing one DB transaction.
func ProcessBatchWithRepositoryTriggers(ctx context.Context, pool BatchPool, policy transaction.Policy, batchSize int, processedAt time.Time, createRun RepositoryRunCreator) (BatchStats, error) {
	return processBatch(ctx, pool, policy, batchSize, processedAt, createRun, nil, false)
}

// ProcessBatchWithRepositoryPreviews additionally applies explicit opt-in PR
// Preview lifecycle state in the same transaction as webhook acknowledgement.
func ProcessBatchWithRepositoryPreviews(ctx context.Context, pool BatchPool, policy transaction.Policy, batchSize int, processedAt time.Time,
	createRun RepositoryRunCreator, processPreview RepositoryPreviewProcessor) (BatchStats, error) {
	return processBatch(ctx, pool, policy, batchSize, processedAt, createRun, processPreview, false)
}

// ProcessBatchWithActionsRunnerAdmissions additionally admits queued
// workflow_job events into the durable Actions runner job table. Admission is
// repository-opt-in gated and does not start a runner or create a MicroVM.
func ProcessBatchWithActionsRunnerAdmissions(ctx context.Context, pool BatchPool, policy transaction.Policy, batchSize int, processedAt time.Time, createRun RepositoryRunCreator) (BatchStats, error) {
	return processBatch(ctx, pool, policy, batchSize, processedAt, createRun, nil, true)
}

// ProcessBatchWithActionsRunnerAdmissionsAndPreviews combines the separately
// authorized Actions admission path with opt-in PR Preview desired state.
func ProcessBatchWithActionsRunnerAdmissionsAndPreviews(ctx context.Context, pool BatchPool, policy transaction.Policy, batchSize int, processedAt time.Time,
	createRun RepositoryRunCreator, processPreview RepositoryPreviewProcessor) (BatchStats, error) {
	return processBatch(ctx, pool, policy, batchSize, processedAt, createRun, processPreview, true)
}

func processBatch(ctx context.Context, pool BatchPool, policy transaction.Policy, batchSize int, processedAt time.Time, createRun RepositoryRunCreator,
	processPreview RepositoryPreviewProcessor, actionsRunnerAdmissions bool) (BatchStats, error) {
	var stats BatchStats
	if pool == nil || batchSize < 1 || batchSize > MaxRepositoryEventBatchSize {
		return stats, fmt.Errorf("database pool and batch size between 1 and %d are required", MaxRepositoryEventBatchSize)
	}
	purged, err := PurgeExpiredRawPayloads(ctx, pool, policy, MaxRawPayloadCleanupBatch, processedAt)
	if err != nil {
		return stats, err
	}
	stats.RawPayloadsPurged = purged
	rows, err := pool.Query(ctx, `SELECT d.integration_id, d.delivery_id, d.event_type
		FROM github_webhook_deliveries d
		WHERE d.processing_status = 'received'
		  AND d.event_schema_version = $2
		  AND (
		    (d.event_type = 'installation' AND d.action IN ('created', 'deleted', 'suspend', 'unsuspend'))
		    OR (d.event_type = 'installation_repositories' AND d.installation_id IS NOT NULL
		      AND EXISTS (
		        SELECT 1 FROM github_app_installation_bindings b
		        JOIN tenant_github_installations i ON i.tenant_id = b.tenant_id AND i.installation_id = b.installation_id
		        WHERE b.installation_id = d.installation_id AND i.status = 'active' AND i.suspended_at IS NULL
		      ))
			OR ($3 AND d.event_type IN ('push', 'pull_request'))
			OR ($4 AND d.event_type='workflow_job' AND d.action IN ('queued','in_progress','completed'))
			OR ($5 AND d.event_type='pull_request')
		  )
		ORDER BY CASE WHEN d.event_type='workflow_job' AND d.action='queued' THEN 0 ELSE 1 END,
		         d.received_at, d.delivery_id
		LIMIT $1`, batchSize, githubwebhook.RepositoryEventSchemaVersion, createRun != nil, actionsRunnerAdmissions, processPreview != nil)
	if err != nil {
		return stats, fmt.Errorf("select pending GitHub repository events: %w", err)
	}
	type pendingDelivery struct{ integrationID, id, eventType string }
	var pending []pendingDelivery
	for rows.Next() {
		var delivery pendingDelivery
		if err := rows.Scan(&delivery.integrationID, &delivery.id, &delivery.eventType); err != nil {
			rows.Close()
			return stats, fmt.Errorf("scan pending GitHub repository event: %w", err)
		}
		pending = append(pending, delivery)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return stats, fmt.Errorf("read pending GitHub repository events: %w", err)
	}
	rows.Close()

	for _, delivery := range pending {
		stats.Claimed++
		type deliveryResult struct{ processed, runCreated, previewUpdated, runnerAdmitted bool }
		result, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (deliveryResult, error) {
			if delivery.eventType == "workflow_job" {
				if !actionsRunnerAdmissions {
					return deliveryResult{}, nil
				}
				var done, admitted bool
				var err error
				var action string
				if err := tx.QueryRow(ctx, `SELECT action FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2`, delivery.integrationID, delivery.id).Scan(&action); err != nil {
					return deliveryResult{}, err
				}
				if action == "queued" {
					done, admitted, err = ProcessActionsRunnerDelivery(ctx, tx, delivery.integrationID, delivery.id, processedAt)
				} else {
					done, err = ProcessActionsRunnerStatusDelivery(ctx, tx, delivery.integrationID, delivery.id, processedAt)
				}
				if err != nil {
					return deliveryResult{}, err
				}
				return deliveryResult{processed: done, runnerAdmitted: admitted}, nil
			}
			if delivery.eventType == "push" || delivery.eventType == "pull_request" {
				if createRun == nil && (delivery.eventType != "pull_request" || processPreview == nil) {
					return deliveryResult{}, nil
				}
				done, created, previewUpdated, err := ProcessRepositoryTriggerDeliveryWithPreview(ctx, tx, delivery.integrationID, delivery.id, processedAt, createRun, processPreview)
				if err != nil {
					return deliveryResult{}, err
				}
				return deliveryResult{processed: done, runCreated: created, previewUpdated: previewUpdated}, nil
			}
			var processed bool
			var err error
			if delivery.eventType == "installation" {
				processed, err = ProcessInstallationLifecycleDeliveryForIntegration(ctx, tx, delivery.integrationID, delivery.id, processedAt)
			} else {
				processed, err = ProcessInstallationRepositoryDeliveryForIntegration(ctx, tx, delivery.integrationID, delivery.id, processedAt)
			}
			return deliveryResult{processed: processed}, err
		})
		if errors.Is(err, githubregistry.ErrInstallationNotActive) {
			stats.Deferred++
			continue
		}
		if err != nil {
			return stats, fmt.Errorf("process GitHub webhook delivery %s: %w", delivery.id, err)
		}
		if result.runCreated {
			stats.WorkloadRunsCreated++
		}
		if result.previewUpdated {
			stats.RepositoryPreviewsUpdated++
		}
		if result.runnerAdmitted {
			stats.ActionsRunnerJobsAdmitted++
		}
		if result.processed {
			stats.Processed++
		}
	}
	return stats, nil
}
