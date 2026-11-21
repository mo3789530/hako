package githubwebhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

type ActionsRunnerJob struct {
	IntegrationID string
	DeliveryID    string
	TenantID      domain.TenantID
	Installation  int64
	Repository    int64
	JobID         int64
	WorkflowRunID int64
	RunAttempt    int64
	WorkflowName  string
	JobName       string
	HeadBranch    string
	HeadSHA       string
	Labels        []string
	State         string
	ClaimID       string
	LeaseExpires  time.Time
	ClaimAttempts int
	CreatedAt     time.Time
}

// ClaimNextActionsRunnerJob leases one durable admission to a controller.
// Expired claims can be recovered after a controller crash; a concurrent
// claim for the same row loses the guarded UPDATE and observes an empty result.
// The caller must run this in the normal OCC-retrying transaction boundary.
func ClaimNextActionsRunnerJob(ctx context.Context, tx pgx.Tx, claimID string, now time.Time, lease time.Duration) (ActionsRunnerJob, bool, error) {
	if ctx == nil || tx == nil || !validActionsRunnerClaimID(claimID) || now.IsZero() || lease < 5*time.Second || lease > 10*time.Minute {
		return ActionsRunnerJob{}, false, errors.New("transaction, claim ID, current time, and a lease between 5 seconds and 10 minutes are required")
	}
	var job ActionsRunnerJob
	var labelsJSON []byte
	err := tx.QueryRow(ctx, `WITH candidate AS (
		SELECT integration_id,workflow_job_id,run_attempt FROM github_actions_runner_jobs
		WHERE state='admitted' OR (state='claimed' AND lease_expires_at <= $1)
		   OR (state='starting' AND lease_expires_at + interval '65 minutes' <= $1)
		ORDER BY created_at,integration_id,workflow_job_id,run_attempt
		LIMIT 1
	)
	UPDATE github_actions_runner_jobs j
	SET state='claimed',claim_id=$2,lease_expires_at=$3,claim_attempts=j.claim_attempts+1,updated_at=$1
	FROM candidate c
	WHERE j.integration_id=c.integration_id AND j.workflow_job_id=c.workflow_job_id AND j.run_attempt=c.run_attempt
	  AND (j.state='admitted' OR (j.state='claimed' AND j.lease_expires_at <= $1) OR (j.state='starting' AND j.lease_expires_at + interval '65 minutes' <= $1))
	RETURNING j.integration_id,j.delivery_id,j.tenant_id,j.installation_id,j.repository_id,j.workflow_job_id,j.workflow_run_id,j.run_attempt,
	          j.workflow_name,j.job_name,j.head_branch,j.head_sha,j.labels,j.state,j.claim_id,j.lease_expires_at,j.claim_attempts,j.created_at`,
		now.UTC(), claimID, now.Add(lease).UTC()).Scan(
		&job.IntegrationID, &job.DeliveryID, &job.TenantID, &job.Installation, &job.Repository, &job.JobID, &job.WorkflowRunID, &job.RunAttempt,
		&job.WorkflowName, &job.JobName, &job.HeadBranch, &job.HeadSHA, &labelsJSON, &job.State, &job.ClaimID, &job.LeaseExpires, &job.ClaimAttempts, &job.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ActionsRunnerJob{}, false, nil
	}
	if err != nil {
		return ActionsRunnerJob{}, false, fmt.Errorf("lease admitted Actions runner job: %w", err)
	}
	if err := json.Unmarshal(labelsJSON, &job.Labels); err != nil {
		return ActionsRunnerJob{}, false, fmt.Errorf("decode admitted Actions runner labels: %w", err)
	}
	return job, true, nil
}

func validActionsRunnerClaimID(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '-') {
			return false
		}
	}
	return true
}
