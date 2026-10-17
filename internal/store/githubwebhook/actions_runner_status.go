package githubwebhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/githubwebhook"
)

// ProcessActionsRunnerStatusDelivery binds GitHub's later workflow_job events
// to the admission created from the queued event. It never creates a job or a
// runtime; mismatched or out-of-order events are acknowledged without changing
// the durable job.
func ProcessActionsRunnerStatusDelivery(ctx context.Context, tx pgx.Tx, integrationID, deliveryID string, updatedAt time.Time) (bool, error) {
	if ctx == nil || tx == nil || !validStoredIntegrationID(integrationID) || !coreValidDeliveryID(deliveryID) {
		return false, errors.New("transaction and valid GitHub Delivery ID are required")
	}
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	var deliveryStatus, eventType, action, eventJSON string
	var schemaVersion int
	var generation int64
	var deliveryReceivedAt time.Time
	err := tx.QueryRow(ctx, `SELECT processing_status,event_type,action,event_schema_version,normalized_event_json,credential_generation,received_at
		FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2 FOR UPDATE`, integrationID, deliveryID).
		Scan(&deliveryStatus, &eventType, &action, &schemaVersion, &eventJSON, &generation, &deliveryReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, pgx.ErrNoRows
	}
	if err != nil {
		return false, fmt.Errorf("lock Actions runner status delivery: %w", err)
	}
	if deliveryStatus == "processed" || deliveryStatus == "failed" {
		return false, nil
	}
	if deliveryStatus != "received" || eventType != "workflow_job" || (action != "in_progress" && action != "completed") || schemaVersion != githubwebhook.RepositoryEventSchemaVersion {
		return false, ErrUnsupportedWebhookEvent
	}
	var event githubwebhook.RepositoryEvent
	if err := json.Unmarshal([]byte(eventJSON), &event); err != nil || event.DeliveryID != deliveryID || event.GitHubEvent != "workflow_job" ||
		event.Action != action || event.WorkflowJob == nil || event.WorkflowJob.ID <= 0 || event.WorkflowJob.RunID <= 0 || event.Repository == nil {
		return false, ErrUnsupportedWebhookEvent
	}
	job := event.WorkflowJob
	wantStatus := "in_progress"
	if action == "completed" {
		wantStatus = "completed"
	}
	if job.Status != wantStatus {
		return false, ErrUnsupportedWebhookEvent
	}
	if action == "completed" && !oneOfActionsRunnerConclusion(job.Conclusion) {
		return false, ErrUnsupportedWebhookEvent
	}
	allowed, err := authorizeStoredTenantEvent(ctx, tx, integrationID, generation, event)
	if err != nil {
		return false, err
	}
	if !allowed {
		if err := failStaleWebhookDelivery(ctx, tx, integrationID, deliveryID); err != nil {
			return false, err
		}
		return true, nil
	}
	tenantID, err := resolveRepositoryEventTenant(ctx, tx, integrationID, event.InstallationID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := failStaleWebhookDelivery(ctx, tx, integrationID, deliveryID); err != nil {
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("resolve Tenant for Actions runner status: %w", err)
	}
	var owner, name string
	if err := tx.QueryRow(ctx, `SELECT owner_login,repository_name FROM tenant_github_repositories
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3`, tenantID, event.InstallationID, event.Repository.GitHubID).Scan(&owner, &name); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if err := failStaleWebhookDelivery(ctx, tx, integrationID, deliveryID); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, fmt.Errorf("revalidate Actions runner repository: %w", err)
	}
	if owner != event.Repository.Owner || name != event.Repository.Name {
		if err := failStaleWebhookDelivery(ctx, tx, integrationID, deliveryID); err != nil {
			return false, err
		}
		return true, nil
	}
	if job.RunAttempt <= 0 {
		// Status events without an exact attempt cannot safely update an
		// admitted runner. Quarantine them rather than matching the legacy
		// zero-attempt compatibility bucket.
		if err := failStaleWebhookDelivery(ctx, tx, integrationID, deliveryID); err != nil {
			return false, err
		}
		return true, nil
	}

	var tenant, state, headBranch, headSHA string
	var installationID, repositoryID, workflowRunID, storedJobID, runAttempt int64
	var runnerID *int64
	err = tx.QueryRow(ctx, `SELECT tenant_id,state,installation_id,repository_id,workflow_run_id,workflow_job_id,run_attempt,head_branch,head_sha,runner_id
		FROM github_actions_runner_jobs WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 FOR UPDATE`,
		integrationID, job.ID, job.RunAttempt).
		Scan(&tenant, &state, &installationID, &repositoryID, &workflowRunID, &storedJobID, &runAttempt, &headBranch, &headSHA, &runnerID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("lock admitted Actions runner job: %w", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// An in_progress/completed delivery can race ahead of its queued
		// delivery. Keep it pending while the matching queued event is still
		// being processed, and prioritize queued admissions in the batch. If
		// there is no admission after the bounded correlation window, quarantine
		// the status delivery rather than retrying it forever.
		var queuedStatus string
		queuedErr := tx.QueryRow(ctx, `SELECT processing_status FROM github_webhook_deliveries
			WHERE integration_id=$1 AND event_type='workflow_job' AND action='queued'
			AND normalized_event_json::jsonb #>> '{workflow_job,id}'=$2
			AND COALESCE(NULLIF(normalized_event_json::jsonb #>> '{workflow_job,run_attempt}','')::bigint,0)=$3
			ORDER BY received_at LIMIT 1`, integrationID, fmt.Sprint(job.ID), job.RunAttempt).Scan(&queuedStatus)
		if queuedErr != nil && !errors.Is(queuedErr, pgx.ErrNoRows) {
			return false, fmt.Errorf("check queued Actions runner event ordering: %w", queuedErr)
		}
		if queuedErr == nil && queuedStatus == "received" {
			return false, nil
		}
		if queuedErr == nil || updatedAt.Sub(deliveryReceivedAt) >= 24*time.Hour {
			if err := failStaleWebhookDelivery(ctx, tx, integrationID, deliveryID); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, nil
	}
	if err == nil && tenant == tenantID && installationID == event.InstallationID && repositoryID == event.Repository.GitHubID &&
		workflowRunID == job.RunID && storedJobID == job.ID && runAttempt == job.RunAttempt && headBranch == job.HeadBranch && headSHA == job.HeadSHA {
		if action == "in_progress" {
			if job.RunnerID != nil && (state == "starting" || state == "assigned" || state == "running") &&
				(runnerID == nil || *runnerID == *job.RunnerID) {
				_, err = tx.Exec(ctx, `UPDATE github_actions_runner_jobs SET state='running',runner_id=$4,runner_name=$5,
					claim_id=NULL,lease_expires_at=NULL,updated_at=$6
					WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 AND state IN ('starting','assigned','running')`,
					integrationID, job.ID, job.RunAttempt, *job.RunnerID, job.RunnerName, updatedAt.UTC())
			} else if (state == "admitted" || state == "claimed") && updatedAt.Sub(deliveryReceivedAt) < 24*time.Hour {
				// A runner assignment may be observed before the Controller has
				// committed its `starting` checkpoint. Retry after that checkpoint
				// instead of dropping this event as out-of-order.
				return false, nil
			}
		} else {
			conclusion := job.Conclusion
			terminalState := "failed"
			if conclusion == "success" || conclusion == "neutral" || conclusion == "skipped" {
				terminalState = "completed"
			} else if conclusion == "cancelled" {
				terminalState = "cancelled"
			}
			// GitHub may deliver the completed event before an in_progress
			// delivery after retries/reordering. In that case the completion's
			// authoritative Runner ID may pin the still-unassigned job. Once a
			// Runner ID is recorded, it must match exactly.
			runnerMatches := (runnerID == nil && (job.RunnerID == nil || state == "starting" || state == "assigned" || state == "running")) ||
				(runnerID != nil && job.RunnerID != nil && *runnerID == *job.RunnerID)
			if runnerMatches && (state == "admitted" || state == "claimed" || state == "starting" || state == "assigned" || state == "running") {
				var eventRunnerID any
				if job.RunnerID != nil {
					eventRunnerID = *job.RunnerID
				}
				_, err = tx.Exec(ctx, `UPDATE github_actions_runner_jobs SET state=$4,runner_id=COALESCE(runner_id,$5),
					runner_name=COALESCE(NULLIF($6,''),runner_name),conclusion=$7,completed_at=$8,
					claim_id=NULL,lease_expires_at=NULL,updated_at=$8
					WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 AND state IN ('admitted','claimed','starting','assigned','running')`,
					integrationID, job.ID, job.RunAttempt, terminalState, eventRunnerID, job.RunnerName, job.Conclusion, updatedAt.UTC())
			}
		}
		if err != nil {
			return false, fmt.Errorf("record Actions runner status: %w", err)
		}
	}
	updated, err := tx.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='processed'
		WHERE integration_id=$1 AND delivery_id=$2 AND processing_status='received' AND event_schema_version=$3`, integrationID, deliveryID, schemaVersion)
	if err != nil {
		return false, fmt.Errorf("mark Actions runner status processed: %w", err)
	}
	if updated.RowsAffected() != 1 {
		return false, errors.New("GitHub Actions runner status delivery changed concurrently")
	}
	return true, nil
}

func oneOfActionsRunnerConclusion(value string) bool {
	switch value {
	case "action_required", "cancelled", "failure", "neutral", "success", "skipped", "stale", "timed_out", "startup_failure":
		return true
	default:
		return false
	}
}
