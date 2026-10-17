package githubwebhook

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

type ActionsRunnerCleanupJob struct {
	IntegrationID   string
	TenantID        domain.TenantID
	ResourcePlaneID *string
	Installation    int64
	Repository      int64
	WorkflowJobID   int64
	RunAttempt      int64
	RunnerID        *int64
	LambdaMicroVMID *string
	Reason          string
	State           string
	ClaimID         string
	LeaseExpires    time.Time
	AttemptCount    int
	CreatedAt       time.Time
}

func enqueueActionsRunnerCleanup(ctx context.Context, tx pgx.Tx, integrationID string, workflowJobID, runAttempt int64, reason string, now time.Time) error {
	if ctx == nil || tx == nil || !validStoredIntegrationID(integrationID) || workflowJobID <= 0 || runAttempt <= 0 ||
		(reason != "completed" && reason != "failed" && reason != "cancelled") || now.IsZero() {
		return errors.New("valid terminal Actions runner identity and time are required for cleanup")
	}
	_, err := tx.Exec(ctx, `INSERT INTO github_actions_runner_cleanup_outbox
		(integration_id,workflow_job_id,run_attempt,tenant_id,installation_id,repository_id,runner_id,resource_plane_id,lambda_microvm_id,reason,state,available_at,created_at,updated_at)
		SELECT integration_id,workflow_job_id,run_attempt,tenant_id,installation_id,repository_id,runner_id,resource_plane_id,lambda_microvm_id,$4,'pending',$5,$5,$5
		FROM github_actions_runner_jobs WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 AND state=$4
		ON CONFLICT (integration_id,workflow_job_id,run_attempt) DO NOTHING`, integrationID, workflowJobID, runAttempt, reason, now.UTC())
	if err != nil {
		return fmt.Errorf("enqueue Actions runner cleanup: %w", err)
	}
	return nil
}

// ClaimNextActionsRunnerCleanup leases one transactional cleanup request.
// Expired leases are recoverable; callers must execute cleanup idempotently.
func ClaimNextActionsRunnerCleanup(ctx context.Context, tx pgx.Tx, resourcePlaneID, claimID string, now time.Time, lease time.Duration) (ActionsRunnerCleanupJob, bool, error) {
	if ctx == nil || tx == nil || !actionsRunnerResourcePlaneIDPattern.MatchString(resourcePlaneID) || !validActionsRunnerClaimID(claimID) || now.IsZero() || lease < 5*time.Second || lease > 10*time.Minute {
		return ActionsRunnerCleanupJob{}, false, errors.New("transaction, Resource Plane, claim ID, current time, and a cleanup lease between 5 seconds and 10 minutes are required")
	}
	var job ActionsRunnerCleanupJob
	err := tx.QueryRow(ctx, `WITH candidate AS (
		SELECT integration_id,workflow_job_id,run_attempt FROM github_actions_runner_cleanup_outbox
		WHERE (resource_plane_id=$1 OR (resource_plane_id IS NULL AND lambda_microvm_id IS NULL))
		  AND ((state='pending' AND available_at<=$2) OR (state='claimed' AND lease_expires_at<=$2))
		ORDER BY available_at,created_at,integration_id,workflow_job_id,run_attempt LIMIT 1
	)
	UPDATE github_actions_runner_cleanup_outbox o
	SET state='claimed',claim_id=$3,lease_expires_at=$4,attempt_count=o.attempt_count+1,updated_at=$2
	FROM candidate c
	WHERE o.integration_id=c.integration_id AND o.workflow_job_id=c.workflow_job_id AND o.run_attempt=c.run_attempt
	  AND (o.resource_plane_id=$1 OR (o.resource_plane_id IS NULL AND o.lambda_microvm_id IS NULL))
	  AND ((o.state='pending' AND o.available_at<=$2) OR (o.state='claimed' AND o.lease_expires_at<=$2))
	RETURNING o.integration_id,o.tenant_id,o.resource_plane_id,o.installation_id,o.repository_id,o.workflow_job_id,o.run_attempt,o.runner_id,o.lambda_microvm_id,
	          o.reason,o.state,o.claim_id,o.lease_expires_at,o.attempt_count,o.created_at`,
		resourcePlaneID, now.UTC(), claimID, now.Add(lease).UTC()).Scan(
		&job.IntegrationID, &job.TenantID, &job.ResourcePlaneID, &job.Installation, &job.Repository, &job.WorkflowJobID, &job.RunAttempt, &job.RunnerID,
		&job.LambdaMicroVMID, &job.Reason, &job.State, &job.ClaimID, &job.LeaseExpires, &job.AttemptCount, &job.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ActionsRunnerCleanupJob{}, false, nil
	}
	if err != nil {
		return ActionsRunnerCleanupJob{}, false, fmt.Errorf("lease Actions runner cleanup: %w", err)
	}
	return job, true, nil
}

// CompleteActionsRunnerCleanup marks a cleanup request complete only for its
// current unexpired lease owner. A stale worker cannot acknowledge a recovered
// request.
func CompleteActionsRunnerCleanup(ctx context.Context, tx pgx.Tx, job ActionsRunnerCleanupJob, now time.Time) (bool, error) {
	if ctx == nil || tx == nil || !validActionsRunnerCleanupJob(job) || now.IsZero() {
		return false, errors.New("transaction, valid cleanup job, and current time are required")
	}
	tag, err := tx.Exec(ctx, `UPDATE github_actions_runner_cleanup_outbox
		SET state='completed',claim_id=NULL,lease_expires_at=NULL,last_error='',updated_at=$5
		WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 AND state='claimed' AND claim_id=$4 AND lease_expires_at>$5`,
		job.IntegrationID, job.WorkflowJobID, job.RunAttempt, job.ClaimID, now.UTC())
	if err != nil {
		return false, fmt.Errorf("complete Actions runner cleanup: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RetryActionsRunnerCleanup releases a live lease and schedules a retry. The
// error parameter is a bounded operator-safe category, never raw tool output.
func RetryActionsRunnerCleanup(ctx context.Context, tx pgx.Tx, job ActionsRunnerCleanupJob, now, retryAt time.Time, safeErrorCode string) (bool, error) {
	if ctx == nil || tx == nil || !validActionsRunnerCleanupJob(job) || now.IsZero() || !retryAt.After(now) || retryAt.Sub(now) > 24*time.Hour || !validCleanupErrorCode(safeErrorCode) {
		return false, errors.New("transaction, valid cleanup job, bounded future retry, and safe error code are required")
	}
	tag, err := tx.Exec(ctx, `UPDATE github_actions_runner_cleanup_outbox
		SET state='pending',claim_id=NULL,lease_expires_at=NULL,available_at=$6,last_error=$7,updated_at=$5
		WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 AND state='claimed' AND claim_id=$4 AND lease_expires_at>$5`,
		job.IntegrationID, job.WorkflowJobID, job.RunAttempt, job.ClaimID, now.UTC(), retryAt.UTC(), safeErrorCode)
	if err != nil {
		return false, fmt.Errorf("retry Actions runner cleanup: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func validActionsRunnerCleanupJob(job ActionsRunnerCleanupJob) bool {
	return validStoredIntegrationID(job.IntegrationID) && validRunnerCleanupResourcePlane(job) && job.WorkflowJobID > 0 && job.RunAttempt > 0 &&
		job.ClaimID != "" && job.State == "claimed" && job.TenantID != "" && job.Installation > 0 && job.Repository > 0
}

func validRunnerCleanupResourcePlane(job ActionsRunnerCleanupJob) bool {
	if job.LambdaMicroVMID == nil {
		return job.ResourcePlaneID == nil || actionsRunnerResourcePlaneIDPattern.MatchString(*job.ResourcePlaneID)
	}
	return job.ResourcePlaneID != nil && actionsRunnerResourcePlaneIDPattern.MatchString(*job.ResourcePlaneID)
}

func validCleanupErrorCode(value string) bool {
	switch value {
	case "runner_deregister_failed", "microvm_terminate_failed", "provider_unavailable":
		return true
	default:
		return false
	}
}
