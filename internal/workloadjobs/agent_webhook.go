package workloadjobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/idgen"
	webhookstore "github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
)

type AgentIssueContextProvider interface {
	Resolve(context.Context, string, string) (githubapp.AgentIssueContext, error)
}

const agentIssueCommandLease = 5 * time.Minute

// ProcessAgentIssueCommandBatch drains only the separately authorized
// Issue-comment queue. It is deliberately separate from generic push/PR
// triggers and must remain disabled until the Agent runtime consumer is ready.
func ProcessAgentIssueCommandBatch(ctx context.Context, pool webhookstore.BatchPool, policy transaction.Policy, limit int,
	provider AgentIssueContextProvider, now time.Time) (webhookstore.BatchStats, error) {
	var stats webhookstore.BatchStats
	if pool == nil || limit < 1 || limit > webhookstore.MaxRepositoryEventBatchSize || provider == nil {
		return stats, fmt.Errorf("database pool, broker provider and batch size between 1 and %d are required", webhookstore.MaxRepositoryEventBatchSize)
	}
	claimID, err := idgen.New("agent-issue-")
	if err != nil {
		return stats, fmt.Errorf("generate Agent Issue command claim: %w", err)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	pending, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) ([]webhookstore.AgentIssueCommandDelivery, error) {
		return webhookstore.ClaimAgentIssueCommands(ctx, tx, claimID, now, agentIssueCommandLease, limit)
	})
	if err != nil {
		return stats, fmt.Errorf("claim pending Agent Issue commands: %w", err)
	}
	stats.Claimed = len(pending)
	for _, item := range pending {
		processed, created, err := processClaimedAgentIssueComment(ctx, pool, policy, provider, item.IntegrationID, item.DeliveryID, claimID, now)
		if err != nil {
			return stats, fmt.Errorf("process Agent Issue command %s: %w", item.DeliveryID, err)
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
	return processAgentIssueComment(ctx, pool, policy, provider, integrationID, deliveryID, "", now)
}

func processClaimedAgentIssueComment(ctx context.Context, pool transaction.Beginner, policy transaction.Policy, provider AgentIssueContextProvider,
	integrationID, deliveryID, claimID string, now time.Time) (bool, bool, error) {
	return processAgentIssueComment(ctx, pool, policy, provider, integrationID, deliveryID, claimID, now)
}

func processAgentIssueComment(ctx context.Context, pool transaction.Beginner, policy transaction.Policy, provider AgentIssueContextProvider,
	integrationID, deliveryID, claimID string, now time.Time) (bool, bool, error) {
	if ctx == nil || pool == nil || provider == nil {
		return false, false, webhookstore.ErrUnsupportedWebhookEvent
	}
	allowed, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		if claimID == "" {
			return webhookstore.PreflightIssueCommentAgentDelivery(ctx, tx, integrationID, deliveryID)
		}
		return webhookstore.PreflightClaimedIssueCommentAgentDelivery(ctx, tx, integrationID, deliveryID, claimID, now)
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
		agentContext := webhookstore.AgentIssueContext{IssueNumber: issue.IssueNumber, Title: issue.Title, Body: issue.Body,
			CommitSHA: issue.CommitSHA, DefaultRef: issue.DefaultRef}
		creator := func(ctx context.Context, tx pgx.Tx, authorization webhookstore.IssueCommentAgentAuthorization, integrationID, deliveryID, correlationID string,
			issueContext webhookstore.AgentIssueContext, createdAt time.Time) (bool, error) {
			runID, err := idgen.New("run_")
			if err != nil {
				return false, err
			}
			issue := githubapp.AgentIssueContext{IssueNumber: issueContext.IssueNumber, Title: issueContext.Title, Body: issueContext.Body,
				CommitSHA: issueContext.CommitSHA, DefaultRef: issueContext.DefaultRef}
			run := domain.WorkloadRun{ID: domain.WorkloadRunID(runID), CreatedAt: createdAt}
			var created bool
			if claimID == "" {
				_, created, err = workloads.CreateOrGetFromIssueComment(ctx, tx, integrationID, deliveryID, correlationID, issue, run)
			} else {
				_, created, err = workloads.CreateOrGetFromClaimedIssueComment(ctx, tx, integrationID, deliveryID, correlationID, claimID, issue, run)
			}
			return created, err
		}
		var processed, created bool
		var err error
		if claimID == "" {
			processed, created, err = webhookstore.ProcessIssueCommentAgentDelivery(ctx, tx, integrationID, deliveryID, agentContext, now, creator)
		} else {
			processed, created, err = webhookstore.ProcessClaimedIssueCommentAgentDelivery(ctx, tx, integrationID, deliveryID, claimID, agentContext, now, creator)
		}
		return result{processed: processed, created: created}, err
	})
	return value.processed, value.created, err
}
