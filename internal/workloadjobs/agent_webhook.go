package workloadjobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/idgen"
	webhookstore "github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
)

type AgentIssueContextProvider interface {
	Resolve(context.Context, string, string) (githubapp.AgentIssueContext, error)
}

// ProcessAgentIssueCommandBatch drains only the separately authorized
// Issue-comment queue. It is deliberately separate from generic push/PR
// triggers and must remain disabled until the Agent runtime consumer is ready.
func ProcessAgentIssueCommandBatch(ctx context.Context, pool webhookstore.BatchPool, policy transaction.Policy, limit int,
	provider AgentIssueContextProvider, now time.Time) (webhookstore.BatchStats, error) {
	var stats webhookstore.BatchStats
	if pool == nil || limit < 1 || limit > webhookstore.MaxRepositoryEventBatchSize || provider == nil {
		return stats, fmt.Errorf("database pool, broker provider and batch size between 1 and %d are required", webhookstore.MaxRepositoryEventBatchSize)
	}
	rows, err := pool.Query(ctx, `SELECT integration_id,delivery_id FROM github_webhook_deliveries
		WHERE processing_status='received' AND event_schema_version=$2 AND event_type='issue_comment' AND action='created'
		ORDER BY received_at,delivery_id LIMIT $1`, limit, githubwebhook.RepositoryEventSchemaVersion)
	if err != nil {
		return stats, fmt.Errorf("select pending Agent Issue commands: %w", err)
	}
	type delivery struct{ integrationID, deliveryID string }
	pending := make([]delivery, 0, limit)
	for rows.Next() {
		var item delivery
		if err := rows.Scan(&item.integrationID, &item.deliveryID); err != nil {
			rows.Close()
			return stats, fmt.Errorf("scan pending Agent Issue command: %w", err)
		}
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return stats, fmt.Errorf("read pending Agent Issue commands: %w", err)
	}
	rows.Close()
	for _, item := range pending {
		stats.Claimed++
		processed, created, err := ProcessAgentIssueComment(ctx, pool, policy, provider, item.integrationID, item.deliveryID, now)
		if err != nil {
			return stats, fmt.Errorf("process Agent Issue command %s: %w", item.deliveryID, err)
		}
		if processed {
			stats.Processed++
		}
		if created {
			stats.WorkloadRunsCreated++
		}
	}
	return stats, nil
}

// ProcessAgentIssueComment resolves remote context before opening a database
// transaction, then atomically reauthorizes, creates/reuses the Agent Run and
// stores its task context while marking the inbox delivery processed. The
// processor is intentionally separate from generic repository triggers until
// a Resource Plane Agent executor is enabled.
func ProcessAgentIssueComment(ctx context.Context, pool transaction.Beginner, policy transaction.Policy, provider AgentIssueContextProvider,
	integrationID, deliveryID string, now time.Time) (bool, bool, error) {
	if ctx == nil || pool == nil || provider == nil {
		return false, false, webhookstore.ErrUnsupportedWebhookEvent
	}
	allowed, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return webhookstore.PreflightIssueCommentAgentDelivery(ctx, tx, integrationID, deliveryID)
	})
	if err != nil || !allowed {
		return false, false, err
	}
	issue, err := provider.Resolve(ctx, integrationID, deliveryID)
	if err != nil {
		return false, false, err
	}
	type result struct{ processed, created bool }
	value, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (result, error) {
		processed, created, err := webhookstore.ProcessIssueCommentAgentDelivery(ctx, tx, integrationID, deliveryID,
			webhookstore.AgentIssueContext{IssueNumber: issue.IssueNumber, Title: issue.Title, Body: issue.Body,
				CommitSHA: issue.CommitSHA, DefaultRef: issue.DefaultRef}, now,
			func(ctx context.Context, tx pgx.Tx, authorization webhookstore.IssueCommentAgentAuthorization, integrationID, deliveryID, correlationID string,
				issueContext webhookstore.AgentIssueContext, createdAt time.Time) (bool, error) {
				runID, err := idgen.New("run_")
				if err != nil {
					return false, err
				}
				_, created, err := workloads.CreateOrGetFromIssueComment(ctx, tx, integrationID, deliveryID, correlationID,
					githubapp.AgentIssueContext{IssueNumber: issueContext.IssueNumber, Title: issueContext.Title, Body: issueContext.Body,
						CommitSHA: issueContext.CommitSHA, DefaultRef: issueContext.DefaultRef},
					domain.WorkloadRun{ID: domain.WorkloadRunID(runID), CreatedAt: createdAt})
				return created, err
			})
		return result{processed: processed, created: created}, err
	})
	return value.processed, value.created, err
}
