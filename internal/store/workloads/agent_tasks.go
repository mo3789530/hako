package workloads

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/agenttask"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	webhookstore "github.com/mo3789530/hako/internal/store/githubwebhook"
)

var ErrAgentTaskConflict = errors.New("Agent task context conflicts with its Issue delivery")
var agentIssueClaimIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,128}$`)

type AgentTaskRecord struct {
	TenantID       domain.TenantID
	WorkloadRunID  domain.WorkloadRunID
	RequestedBy    domain.UserID `json:"-"`
	IntegrationID  string
	DeliveryID     string
	InstallationID int64
	RepositoryID   int64
	IssueNumber    int64
	CommentID      int64
	DefaultBranch  string
	CommitSHA      string
	Title          string
	Body           string
	ContextSHA256  string
	CreatedAt      time.Time
}

// CreateOrGetFromIssueComment persists one immutable Agent Run and its bounded
// Issue task context in the caller's transaction. Tenant authorization is
// reloaded from the inbox row here, not taken from supplied IDs alone.
func CreateOrGetFromIssueComment(ctx context.Context, tx pgx.Tx, integrationID, deliveryID, correlationID string,
	issue githubapp.AgentIssueContext, run domain.WorkloadRun) (domain.WorkloadRun, bool, error) {
	return createOrGetFromIssueComment(ctx, tx, integrationID, deliveryID, correlationID, "", issue, run)
}

func CreateOrGetFromClaimedIssueComment(ctx context.Context, tx pgx.Tx, integrationID, deliveryID, correlationID, claimID string,
	issue githubapp.AgentIssueContext, run domain.WorkloadRun) (domain.WorkloadRun, bool, error) {
	if !agentIssueClaimIDPattern.MatchString(claimID) {
		return domain.WorkloadRun{}, false, ErrAgentTaskConflict
	}
	return createOrGetFromIssueComment(ctx, tx, integrationID, deliveryID, correlationID, claimID, issue, run)
}

func createOrGetFromIssueComment(ctx context.Context, tx pgx.Tx, integrationID, deliveryID, correlationID, claimID string,
	issue githubapp.AgentIssueContext, run domain.WorkloadRun) (domain.WorkloadRun, bool, error) {
	if tx == nil || integrationID == "shared" || strings.TrimSpace(integrationID) == "" || deliveryID == "" ||
		issue.IssueNumber <= 0 || strings.TrimSpace(issue.Title) == "" || len(issue.Title) > 1024 || len(issue.Body) > 64<<10 ||
		len(issue.CommitSHA) != 40 && len(issue.CommitSHA) != 64 || !commitSHAPattern.MatchString(issue.CommitSHA) ||
		issue.DefaultRef == "" || len(issue.DefaultRef) > 255 || strings.ContainsAny(issue.DefaultRef, "\r\n\x00") {
		return domain.WorkloadRun{}, false, ErrAgentTaskConflict
	}
	var authorization webhookstore.IssueCommentAgentAuthorization
	var allowed bool
	var err error
	if claimID == "" {
		authorization, allowed, err = webhookstore.AuthorizeIssueCommentAgentCommand(ctx, tx, integrationID, deliveryID)
	} else {
		authorization, allowed, err = webhookstore.AuthorizeClaimedIssueCommentAgentCommand(ctx, tx, integrationID, deliveryID, claimID, run.CreatedAt)
	}
	if err != nil || !allowed || authorization.IssueNumber != issue.IssueNumber {
		return domain.WorkloadRun{}, false, ErrAgentTaskConflict
	}
	if run.TenantID != "" && run.TenantID != authorization.TenantID || run.RequestedBy != "" && string(run.RequestedBy) != authorization.UserID ||
		run.GitHubInstallation != 0 && run.GitHubInstallation != authorization.Installation ||
		run.GitHubRepository != 0 && run.GitHubRepository != authorization.Repository.GitHubID ||
		run.CommitSHA != "" && run.CommitSHA != issue.CommitSHA || run.Ref != "" && run.Ref != issue.DefaultRef {
		return domain.WorkloadRun{}, false, ErrAgentTaskConflict
	}
	run.TenantID = authorization.TenantID
	run.RequestedBy = domain.UserID(authorization.UserID)
	run.Kind = domain.WorkloadAgent
	run.GitHubInstallation = authorization.Installation
	run.GitHubRepository = authorization.Repository.GitHubID
	run.CommitSHA = issue.CommitSHA
	run.Ref = issue.DefaultRef
	run.TriggerEvent = "issue_comment"
	run.TriggerBranch = issue.DefaultRef
	run.RuntimeClass = "standard"
	run.TrustLevel = domain.WorkloadUntrusted
	run.CorrelationID = correlationID
	run.CreatedAt = run.CreatedAt.UTC()
	run.IdempotencyKey = WebhookDeliveryIdempotencyKey(integrationID, deliveryID)
	if run.ID == "" {
		return domain.WorkloadRun{}, false, ErrAgentTaskConflict
	}
	stored, created, err := CreateOrGet(ctx, tx, run)
	if err != nil {
		return domain.WorkloadRun{}, false, err
	}
	if stored.Kind != domain.WorkloadAgent || stored.TenantID != authorization.TenantID || stored.RequestedBy != domain.UserID(authorization.UserID) ||
		stored.GitHubIntegrationID != integrationID || stored.GitHubInstallation != authorization.Installation ||
		stored.GitHubRepository != authorization.Repository.GitHubID || stored.CommitSHA != issue.CommitSHA || stored.Ref != issue.DefaultRef ||
		stored.TriggerEvent != "issue_comment" || stored.TriggerBranch != issue.DefaultRef {
		return domain.WorkloadRun{}, false, ErrAgentTaskConflict
	}
	task := AgentTaskRecord{TenantID: authorization.TenantID, WorkloadRunID: stored.ID, RequestedBy: domain.UserID(authorization.UserID), IntegrationID: integrationID, DeliveryID: deliveryID,
		InstallationID: authorization.Installation, RepositoryID: authorization.Repository.GitHubID, IssueNumber: issue.IssueNumber,
		CommentID: authorization.CommentID, DefaultBranch: issue.DefaultRef, CommitSHA: issue.CommitSHA, Title: issue.Title,
		Body: issue.Body, CreatedAt: stored.CreatedAt}
	task.ContextSHA256 = agenttask.SHA256(agenttask.Context{TenantID: string(task.TenantID), WorkloadRunID: string(task.WorkloadRunID),
		IntegrationID: task.IntegrationID, DeliveryID: task.DeliveryID, InstallationID: task.InstallationID,
		RepositoryID: task.RepositoryID, IssueNumber: task.IssueNumber, CommentID: task.CommentID,
		DefaultBranch: task.DefaultBranch, CommitSHA: task.CommitSHA, Title: task.Title, Body: task.Body})
	if err := insertOrVerifyAgentTask(ctx, tx, task); err != nil {
		return domain.WorkloadRun{}, false, err
	}
	return stored, created, nil
}

func insertOrVerifyAgentTask(ctx context.Context, tx pgx.Tx, task AgentTaskRecord) error {
	if _, err := tx.Exec(ctx, `INSERT INTO workload_agent_tasks
		(tenant_id,workload_run_id,requested_by,integration_id,delivery_id,installation_id,repository_id,issue_number,comment_id,default_branch,commit_sha,issue_title,issue_body,context_sha256,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) ON CONFLICT DO NOTHING`,
		task.TenantID, task.WorkloadRunID, task.RequestedBy, task.IntegrationID, task.DeliveryID, task.InstallationID, task.RepositoryID, task.IssueNumber,
		task.CommentID, task.DefaultBranch, task.CommitSHA, task.Title, task.Body, task.ContextSHA256, task.CreatedAt.UTC()); err != nil {
		return fmt.Errorf("persist Agent task context: %w", err)
	}
	stored, err := getAgentTask(ctx, tx, task.TenantID, task.WorkloadRunID)
	if err != nil {
		return err
	}
	if stored.RequestedBy != task.RequestedBy || stored.IntegrationID != task.IntegrationID || stored.DeliveryID != task.DeliveryID || stored.InstallationID != task.InstallationID ||
		stored.RepositoryID != task.RepositoryID || stored.IssueNumber != task.IssueNumber || stored.CommentID != task.CommentID ||
		stored.DefaultBranch != task.DefaultBranch || stored.CommitSHA != task.CommitSHA || stored.Title != task.Title ||
		stored.Body != task.Body || stored.ContextSHA256 != task.ContextSHA256 {
		return ErrAgentTaskConflict
	}
	return nil
}

func getAgentTask(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, runID domain.WorkloadRunID) (AgentTaskRecord, error) {
	var result AgentTaskRecord
	err := tx.QueryRow(ctx, `SELECT tenant_id,workload_run_id,requested_by,integration_id,delivery_id,installation_id,repository_id,issue_number,comment_id,
		default_branch,commit_sha,issue_title,issue_body,context_sha256,created_at FROM workload_agent_tasks WHERE tenant_id=$1 AND workload_run_id=$2`, tenantID, runID).
		Scan(&result.TenantID, &result.WorkloadRunID, &result.RequestedBy, &result.IntegrationID, &result.DeliveryID, &result.InstallationID, &result.RepositoryID,
			&result.IssueNumber, &result.CommentID, &result.DefaultBranch, &result.CommitSHA, &result.Title, &result.Body, &result.ContextSHA256, &result.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentTaskRecord{}, ErrNotFound
	}
	if err != nil {
		return AgentTaskRecord{}, fmt.Errorf("read Agent task context: %w", err)
	}
	return result, nil
}

// GetAgentTask scopes task text to the owning Tenant and Run. A privileged
// execution resolver should be the only API caller; user-facing Run APIs must
// not expose the issue body by serializing this record.
func GetAgentTask(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, runID domain.WorkloadRunID) (AgentTaskRecord, error) {
	if tx == nil || tenantID == "" || runID == "" {
		return AgentTaskRecord{}, ErrNotFound
	}
	return getAgentTask(ctx, tx, tenantID, runID)
}
