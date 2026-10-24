package githubwebhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	hakowebhook "github.com/mo3789530/hako/internal/githubwebhook"
)

var ErrWorkflowJobIdentityConflict = errors.New("GitHub workflow job identity conflicts with prior admission")

// ProcessActionsRunnerDelivery admits exactly one queued Actions job after
// revalidating the current Tenant integration, Installation binding, and the
// Repository's separate Actions Runner opt-in. It only creates a durable job
// admission record; it does not mint a runner token or create a runtime.
func ProcessActionsRunnerDelivery(ctx context.Context, tx pgx.Tx, integrationID, deliveryID string, admittedAt time.Time) (bool, bool, error) {
	if ctx == nil || tx == nil || !validStoredIntegrationID(integrationID) || !coreValidDeliveryID(deliveryID) {
		return false, false, errors.New("transaction and valid GitHub Delivery ID are required")
	}
	if admittedAt.IsZero() {
		admittedAt = time.Now().UTC()
	}
	var status, eventType, action, eventJSON, correlationID string
	var schemaVersion int
	var generation int64
	var receivedAt time.Time
	err := tx.QueryRow(ctx, `SELECT processing_status,event_type,action,event_schema_version,normalized_event_json,credential_generation,received_at,COALESCE(correlation_id,'')
		FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2 FOR UPDATE`, integrationID, deliveryID).
		Scan(&status, &eventType, &action, &schemaVersion, &eventJSON, &generation, &receivedAt, &correlationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, pgx.ErrNoRows
	}
	if err != nil {
		return false, false, fmt.Errorf("lock GitHub Actions workflow job delivery: %w", err)
	}
	if status == "processed" || status == "failed" {
		return false, false, nil
	}
	if status != "received" || eventType != "workflow_job" || action != "queued" || schemaVersion != hakowebhook.RepositoryEventSchemaVersion {
		return false, false, ErrUnsupportedWebhookEvent
	}
	var event hakowebhook.RepositoryEvent
	if err := json.Unmarshal([]byte(eventJSON), &event); err != nil || event.DeliveryID != deliveryID || event.GitHubEvent != "workflow_job" ||
		event.Action != "queued" || event.WorkflowJob == nil || event.WorkflowJob.Status != "queued" || event.Repository == nil {
		return false, false, ErrUnsupportedWebhookEvent
	}
	allowed, err := authorizeStoredTenantEvent(ctx, tx, integrationID, generation, event)
	if err != nil {
		return false, false, err
	}
	if !allowed {
		if err := failStaleWebhookDelivery(ctx, tx, integrationID, deliveryID); err != nil {
			return false, false, err
		}
		return true, false, nil
	}
	tenantID, err := resolveRepositoryEventTenant(ctx, tx, integrationID, event.InstallationID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := failStaleWebhookDelivery(ctx, tx, integrationID, deliveryID); err != nil {
			return false, false, err
		}
		return true, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("resolve Tenant for Actions workflow job: %w", err)
	}
	var enabled bool
	var enabledAt *time.Time
	var ownerLogin, repositoryName string
	err = tx.QueryRow(ctx, `SELECT actions_runner_enabled,actions_runner_enabled_at,owner_login,repository_name
		FROM tenant_github_repositories WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3`,
		tenantID, event.InstallationID, event.Repository.GitHubID).Scan(&enabled, &enabledAt, &ownerLogin, &repositoryName)
	if errors.Is(err, pgx.ErrNoRows) || !enabled || enabledAt == nil || receivedAt.Before(*enabledAt) ||
		ownerLogin != event.Repository.Owner || repositoryName != event.Repository.Name {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, false, fmt.Errorf("load Tenant Actions runner policy: %w", err)
		}
		if err := failStaleWebhookDelivery(ctx, tx, integrationID, deliveryID); err != nil {
			return false, false, err
		}
		return true, false, nil
	}
	job := event.WorkflowJob
	if job.RunAttempt <= 0 {
		// A runner admission must correlate to later in_progress/completed
		// events using the exact workflow attempt. Retain older normalized
		// deliveries, but do not create an uncorrelatable runtime job.
		if err := failStaleWebhookDelivery(ctx, tx, integrationID, deliveryID); err != nil {
			return false, false, err
		}
		return true, false, nil
	}
	labels, err := json.Marshal(job.Labels)
	if err != nil {
		return false, false, fmt.Errorf("encode Actions runner labels: %w", err)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO github_actions_runner_jobs
		(integration_id,delivery_id,tenant_id,installation_id,repository_id,workflow_job_id,workflow_run_id,run_attempt,workflow_name,job_name,head_branch,head_sha,labels,state,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'admitted',$14,$14)
		ON CONFLICT (integration_id,workflow_job_id,run_attempt) DO NOTHING`,
		integrationID, deliveryID, tenantID, event.InstallationID, event.Repository.GitHubID, job.ID, job.RunID, job.RunAttempt,
		job.WorkflowName, job.Name, job.HeadBranch, job.HeadSHA, labels, admittedAt.UTC())
	if err != nil {
		return false, false, fmt.Errorf("persist Actions runner admission: %w", err)
	}
	created := tag.RowsAffected() == 1
	if !created {
		var existingTenant string
		var existingInstallation, existingRepository, existingRun, existingJob int64
		var existingBranch, existingSHA string
		if err := tx.QueryRow(ctx, `SELECT tenant_id,installation_id,repository_id,workflow_run_id,workflow_job_id,head_branch,head_sha
			FROM github_actions_runner_jobs WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3`, integrationID, job.ID, job.RunAttempt).
			Scan(&existingTenant, &existingInstallation, &existingRepository, &existingRun, &existingJob, &existingBranch, &existingSHA); err != nil {
			return false, false, fmt.Errorf("verify idempotent Actions runner admission: %w", err)
		}
		if domain.TenantID(existingTenant) != domain.TenantID(tenantID) || existingInstallation != event.InstallationID || existingRepository != event.Repository.GitHubID ||
			existingRun != job.RunID || existingJob != job.ID || existingBranch != job.HeadBranch || existingSHA != job.HeadSHA {
			return false, false, ErrWorkflowJobIdentityConflict
		}
	} else if err := createActionsRunnerCheck(ctx, tx, integrationID, domain.TenantID(tenantID), event.InstallationID, event.Repository.GitHubID,
		job.ID, job.RunID, job.RunAttempt, job.HeadSHA, correlationID, admittedAt); err != nil {
		return false, false, fmt.Errorf("create Actions Runner GitHub Check: %w", err)
	}
	updated, err := tx.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='processed'
		WHERE integration_id=$1 AND delivery_id=$2 AND processing_status='received' AND event_schema_version=$3`, integrationID, deliveryID, schemaVersion)
	if err != nil {
		return false, false, fmt.Errorf("mark Actions workflow job delivery processed: %w", err)
	}
	if updated.RowsAffected() != 1 {
		return false, false, errors.New("GitHub Actions workflow job delivery processing state changed concurrently")
	}
	return true, created, nil
}
