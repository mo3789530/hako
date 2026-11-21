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
	"github.com/jackc/pgx/v5/pgconn"
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
	return preflightIssueCommentAgentDelivery(ctx, tx, integrationID, deliveryID, "", time.Time{})
}

func PreflightClaimedIssueCommentAgentDelivery(ctx context.Context, tx pgx.Tx, integrationID, deliveryID, claimID string, now time.Time) (bool, error) {
	if !agentIssueClaimPattern.MatchString(claimID) || now.IsZero() {
		return false, errors.New("valid Agent Issue claim identity and timestamp are required")
	}
	return preflightIssueCommentAgentDelivery(ctx, tx, integrationID, deliveryID, claimID, now)
}

func preflightIssueCommentAgentDelivery(ctx context.Context, tx pgx.Tx, integrationID, deliveryID, claimID string, now time.Time) (bool, error) {
	if tx == nil || !validStoredIntegrationID(integrationID) || integrationID == "shared" || !coreValidDeliveryID(deliveryID) {
		return false, errors.New("tenant integration and valid GitHub Delivery ID are required")
	}
	var status, storedClaimID string
	var schemaVersion int
	var leaseExpires time.Time
	err := tx.QueryRow(ctx, `SELECT processing_status,event_schema_version,COALESCE(agent_processing_claim_id,''),COALESCE(agent_processing_lease_expires_at,'epoch'::timestamptz)
		FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2`,
		integrationID, deliveryID).Scan(&status, &schemaVersion, &storedClaimID, &leaseExpires)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, pgx.ErrNoRows
	}
	if err != nil {
		return false, fmt.Errorf("lock Issue command for authorization preflight: %w", err)
	}
	if status == "processed" || status == "failed" {
		return false, nil
	}
	if schemaVersion != githubwebhook.RepositoryEventSchemaVersion ||
		(claimID == "" && (status != "received" || storedClaimID != "")) ||
		(claimID != "" && (status != "processing" || storedClaimID != claimID || !leaseExpires.After(now.UTC()))) {
		return false, ErrUnsupportedWebhookEvent
	}
	var allowed bool
	if claimID == "" {
		_, allowed, err = AuthorizeIssueCommentAgentCommand(ctx, tx, integrationID, deliveryID)
	} else {
		_, allowed, err = AuthorizeClaimedIssueCommentAgentCommand(ctx, tx, integrationID, deliveryID, claimID, now)
	}
	if err != nil || allowed {
		return allowed, err
	}
	if claimID == "" {
		err = failStaleWebhookDelivery(ctx, tx, integrationID, deliveryID)
	} else {
		_, err = tx.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='failed',agent_processing_claim_id=NULL,agent_processing_lease_expires_at=NULL
			WHERE integration_id=$1 AND delivery_id=$2 AND processing_status='processing' AND agent_processing_claim_id=$3 AND agent_processing_lease_expires_at>$4`,
			integrationID, deliveryID, claimID, now.UTC())
	}
	if err != nil {
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
	return processIssueCommentAgentDelivery(ctx, tx, integrationID, deliveryID, "", issue, processedAt, createRun)
}

func ProcessClaimedIssueCommentAgentDelivery(ctx context.Context, tx pgx.Tx, integrationID, deliveryID, claimID string, issue AgentIssueContext,
	processedAt time.Time, createRun AgentIssueRunCreator) (bool, bool, error) {
	if !agentIssueClaimPattern.MatchString(claimID) {
		return false, false, errors.New("valid Agent Issue claim identity is required")
	}
	return processIssueCommentAgentDelivery(ctx, tx, integrationID, deliveryID, claimID, issue, processedAt, createRun)
}

func processIssueCommentAgentDelivery(ctx context.Context, tx pgx.Tx, integrationID, deliveryID, claimID string, issue AgentIssueContext,
	processedAt time.Time, createRun AgentIssueRunCreator) (bool, bool, error) {
	if tx == nil || !validStoredIntegrationID(integrationID) || integrationID == "shared" || !coreValidDeliveryID(deliveryID) || createRun == nil {
		return false, false, errors.New("tenant integration, transaction, Agent Run creator, and GitHub Delivery ID are required")
	}
	if processedAt.IsZero() {
		processedAt = time.Now().UTC()
	}
	var status, eventJSON, correlationID, storedClaimID string
	var schemaVersion int
	var receivedAt time.Time
	var leaseExpires time.Time
	err := tx.QueryRow(ctx, `SELECT processing_status,event_schema_version,normalized_event_json,received_at,COALESCE(correlation_id,''),COALESCE(agent_processing_claim_id,''),COALESCE(agent_processing_lease_expires_at,'epoch'::timestamptz)
		FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2`, integrationID, deliveryID).
		Scan(&status, &schemaVersion, &eventJSON, &receivedAt, &correlationID, &storedClaimID, &leaseExpires)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, pgx.ErrNoRows
	}
	if err != nil {
		return false, false, fmt.Errorf("lock Agent Issue command delivery: %w", err)
	}
	if status == "processed" || status == "failed" {
		return false, false, nil
	}
	if schemaVersion != githubwebhook.RepositoryEventSchemaVersion ||
		(claimID == "" && (status != "received" || storedClaimID != "")) ||
		(claimID != "" && (status != "processing" || storedClaimID != claimID || !leaseExpires.After(processedAt.UTC()))) {
		return false, false, ErrUnsupportedWebhookEvent
	}
	var event githubwebhook.RepositoryEvent
	if err := json.Unmarshal([]byte(eventJSON), &event); err != nil || event.DeliveryID != deliveryID || event.GitHubEvent != "issue_comment" ||
		event.Action != "created" || event.IssueComment == nil || event.Repository == nil {
		return false, false, ErrUnsupportedWebhookEvent
	}
	var authorization IssueCommentAgentAuthorization
	var allowed bool
	if claimID == "" {
		authorization, allowed, err = AuthorizeIssueCommentAgentCommand(ctx, tx, integrationID, deliveryID)
	} else {
		authorization, allowed, err = AuthorizeClaimedIssueCommentAgentCommand(ctx, tx, integrationID, deliveryID, claimID, processedAt)
	}
	if err != nil {
		return false, false, err
	}
	if !allowed {
		if claimID == "" {
			err = failStaleWebhookDelivery(ctx, tx, integrationID, deliveryID)
		} else {
			_, err = tx.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='failed',agent_processing_claim_id=NULL,agent_processing_lease_expires_at=NULL
				WHERE integration_id=$1 AND delivery_id=$2 AND processing_status='processing' AND agent_processing_claim_id=$3 AND agent_processing_lease_expires_at>$4`,
				integrationID, deliveryID, claimID, processedAt.UTC())
		}
		if err != nil {
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
	var tag pgconn.CommandTag
	if claimID == "" {
		tag, err = tx.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='processed'
			WHERE integration_id=$1 AND delivery_id=$2 AND processing_status='received' AND agent_processing_claim_id IS NULL AND event_schema_version=$3`, integrationID, deliveryID, schemaVersion)
	} else {
		tag, err = tx.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='processed',agent_processing_claim_id=NULL,agent_processing_lease_expires_at=NULL
			WHERE integration_id=$1 AND delivery_id=$2 AND processing_status='processing' AND agent_processing_claim_id=$3 AND agent_processing_lease_expires_at>$4 AND event_schema_version=$5`,
			integrationID, deliveryID, claimID, processedAt.UTC(), schemaVersion)
	}
	if err != nil {
		return false, false, fmt.Errorf("mark Agent Issue command processed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, false, errors.New("Agent Issue delivery processing state changed concurrently")
	}
	return true, created, nil
}
