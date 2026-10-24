package githubwebhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/githubwebhook"
)

var agentIssueCommitSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

type AgentIssueContext struct {
	IssueNumber int64
	Title       string
	Body        string
	CommitSHA   string
	DefaultRef  string
}

type AgentIssueRunCreator func(context.Context, pgx.Tx, IssueCommentAgentAuthorization, string, string, string, AgentIssueContext, time.Time) (bool, error)

// PreflightIssueCommentAgentDelivery rejects stale or unauthorized commands
// before an external broker call and avoids re-fetching context for completed
// deliveries. Final authorization is still repeated at commit time.
func PreflightIssueCommentAgentDelivery(ctx context.Context, tx pgx.Tx, integrationID, deliveryID string) (bool, error) {
	if tx == nil || !validStoredIntegrationID(integrationID) || integrationID == "shared" || !coreValidDeliveryID(deliveryID) {
		return false, errors.New("tenant integration and valid GitHub Delivery ID are required")
	}
	var status string
	var schemaVersion int
	err := tx.QueryRow(ctx, `SELECT processing_status,event_schema_version FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2 FOR UPDATE`,
		integrationID, deliveryID).Scan(&status, &schemaVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, pgx.ErrNoRows
	}
	if err != nil {
		return false, fmt.Errorf("lock Issue command for authorization preflight: %w", err)
	}
	if status == "processed" || status == "failed" {
		return false, nil
	}
	if status != "received" || schemaVersion != githubwebhook.RepositoryEventSchemaVersion {
		return false, ErrUnsupportedWebhookEvent
	}
	_, allowed, err := AuthorizeIssueCommentAgentCommand(ctx, tx, integrationID, deliveryID)
	if err != nil || allowed {
		return allowed, err
	}
	if err := failStaleWebhookDelivery(ctx, tx, integrationID, deliveryID); err != nil {
		return false, err
	}
	return false, nil
}

// ProcessIssueCommentAgentDelivery atomically reauthorizes one resolved Issue
// command, creates or reuses its Agent Run, and acknowledges the inbox row.
// GitHub API reads must happen before this transaction; the supplied context
// is only accepted after its immutable SHA and Issue number are validated.
func ProcessIssueCommentAgentDelivery(ctx context.Context, tx pgx.Tx, integrationID, deliveryID string, issue AgentIssueContext,
	processedAt time.Time, createRun AgentIssueRunCreator) (bool, bool, error) {
	if tx == nil || !validStoredIntegrationID(integrationID) || integrationID == "shared" || !coreValidDeliveryID(deliveryID) || createRun == nil {
		return false, false, errors.New("tenant integration, transaction, Agent Run creator, and GitHub Delivery ID are required")
	}
	if processedAt.IsZero() {
		processedAt = time.Now().UTC()
	}
	var status, eventJSON, correlationID string
	var schemaVersion int
	var receivedAt time.Time
	err := tx.QueryRow(ctx, `SELECT processing_status,event_schema_version,normalized_event_json,received_at,COALESCE(correlation_id,'')
		FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2 FOR UPDATE`, integrationID, deliveryID).
		Scan(&status, &schemaVersion, &eventJSON, &receivedAt, &correlationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, pgx.ErrNoRows
	}
	if err != nil {
		return false, false, fmt.Errorf("lock Agent Issue command delivery: %w", err)
	}
	if status == "processed" || status == "failed" {
		return false, false, nil
	}
	if status != "received" || schemaVersion != githubwebhook.RepositoryEventSchemaVersion {
		return false, false, ErrUnsupportedWebhookEvent
	}
	var event githubwebhook.RepositoryEvent
	if err := json.Unmarshal([]byte(eventJSON), &event); err != nil || event.DeliveryID != deliveryID || event.GitHubEvent != "issue_comment" ||
		event.Action != "created" || event.IssueComment == nil || event.Repository == nil {
		return false, false, ErrUnsupportedWebhookEvent
	}
	authorization, allowed, err := AuthorizeIssueCommentAgentCommand(ctx, tx, integrationID, deliveryID)
	if err != nil {
		return false, false, err
	}
	if !allowed {
		if err := failStaleWebhookDelivery(ctx, tx, integrationID, deliveryID); err != nil {
			return false, false, err
		}
		return true, false, nil
	}
	if issue.IssueNumber != authorization.IssueNumber || strings.TrimSpace(issue.Title) == "" || len(issue.Title) > 1024 || len(issue.Body) > 64<<10 ||
		!agentIssueCommitSHA.MatchString(issue.CommitSHA) || issue.DefaultRef == "" || len(issue.DefaultRef) > 255 || strings.ContainsAny(issue.DefaultRef, "\r\n\x00") {
		return false, false, ErrUnsupportedWebhookEvent
	}
	created, err := createRun(ctx, tx, authorization, integrationID, deliveryID, correlationID, issue, processedAt.UTC())
	if err != nil {
		return false, false, fmt.Errorf("create Agent Run from Issue command: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='processed'
		WHERE integration_id=$1 AND delivery_id=$2 AND processing_status='received' AND event_schema_version=$3`, integrationID, deliveryID, schemaVersion)
	if err != nil {
		return false, false, fmt.Errorf("mark Agent Issue command processed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, false, errors.New("Agent Issue delivery processing state changed concurrently")
	}
	return true, created, nil
}
