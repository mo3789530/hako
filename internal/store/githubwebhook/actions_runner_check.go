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
	"github.com/mo3789530/hako/internal/idgen"
)

const actionsRunnerCheckName = "Hako Actions Runner"

type ActionsRunnerCheck struct {
	IntegrationID string
	TenantID      domain.TenantID
	Installation  int64
	Repository    int64
	WorkflowJobID int64
	WorkflowRunID int64
	RunAttempt    int64
	CommitSHA     string
	Name          string
	ExternalID    string
	CorrelationID string
	CheckRunID    int64
	Status        string
	Conclusion    string
	StartedAt     time.Time
	CompletedAt   *time.Time
	OutputTitle   string
	OutputSummary string
	Revision      int64
}

type actionsRunnerCheckOutboxEnvelope struct {
	SchemaVersion int    `json:"schema_version"`
	IntegrationID string `json:"integration_id"`
	WorkflowJobID int64  `json:"workflow_job_id"`
	RunAttempt    int64  `json:"run_attempt"`
	CorrelationID string `json:"correlation_id,omitempty"`
	Revision      int64  `json:"revision"`
}

// createActionsRunnerCheck creates the initial queued Check in the same
// transaction as the opt-in Runner admission and schedules its delivery.
func createActionsRunnerCheck(ctx context.Context, tx pgx.Tx, integrationID string, tenantID domain.TenantID, installationID, repositoryID int64,
	jobID, runID, attempt int64, headSHA, correlationID string, now time.Time) error {
	if ctx == nil || tx == nil || !validStoredIntegrationID(integrationID) || tenantID == "" || installationID <= 0 || repositoryID <= 0 ||
		jobID <= 0 || runID <= 0 || attempt <= 0 || !validRunnerCommitSHA(headSHA) ||
		correlationID != "" && !webhookCorrelationIDPattern.MatchString(correlationID) || now.IsZero() {
		return errors.New("complete Actions Runner Check identity is required")
	}
	externalID := fmt.Sprintf("hako-actions-runner/%s/%d/%d", integrationID, jobID, attempt)
	if len(externalID) > 255 {
		return errors.New("Actions Runner Check external identity is too long")
	}
	const summary = "Hako accepted this workflow job and is preparing an isolated Lambda MicroVM runner."
	_, err := tx.Exec(ctx, `INSERT INTO github_actions_runner_checks
		(integration_id,workflow_job_id,run_attempt,tenant_id,installation_id,repository_id,workflow_run_id,commit_sha,check_name,external_id,
		 status,conclusion,started_at,completed_at,output_title,output_summary,revision,updated_at,correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'queued',NULL,$11,NULL,$12,$13,1,$11,NULLIF($14,''))
		ON CONFLICT (integration_id,workflow_job_id,run_attempt) DO NOTHING`,
		integrationID, jobID, attempt, tenantID, installationID, repositoryID, runID, headSHA, actionsRunnerCheckName, externalID, now.UTC(), actionsRunnerCheckName, summary, correlationID)
	if err != nil {
		return fmt.Errorf("persist initial Actions Runner Check: %w", err)
	}
	var stored ActionsRunnerCheck
	if err := tx.QueryRow(ctx, `SELECT integration_id,tenant_id,installation_id,repository_id,workflow_job_id,workflow_run_id,run_attempt,
		commit_sha,check_name,external_id,status,COALESCE(conclusion,''),started_at,completed_at,output_title,output_summary,revision,COALESCE(correlation_id,'')
		FROM github_actions_runner_checks WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 FOR UPDATE`, integrationID, jobID, attempt).
		Scan(&stored.IntegrationID, &stored.TenantID, &stored.Installation, &stored.Repository, &stored.WorkflowJobID, &stored.WorkflowRunID, &stored.RunAttempt,
			&stored.CommitSHA, &stored.Name, &stored.ExternalID, &stored.Status, &stored.Conclusion, &stored.StartedAt, &stored.CompletedAt,
			&stored.OutputTitle, &stored.OutputSummary, &stored.Revision, &stored.CorrelationID); err != nil {
		return fmt.Errorf("verify Actions Runner Check identity: %w", err)
	}
	if stored.TenantID != tenantID || stored.Installation != installationID || stored.Repository != repositoryID || stored.WorkflowRunID != runID ||
		stored.CommitSHA != headSHA || stored.Name != actionsRunnerCheckName || stored.ExternalID != externalID {
		return errors.New("Actions Runner Check identity conflicts with existing job")
	}
	return enqueueActionsRunnerCheck(ctx, tx, stored, now)
}

// updateActionsRunnerCheck follows the GitHub workflow_job event, which is the
// authoritative source of job completion (the local Runner exit code is not).
func updateActionsRunnerCheck(ctx context.Context, tx pgx.Tx, integrationID string, event hakowebhook.RepositoryEvent, tenantID domain.TenantID, now time.Time) error {
	if ctx == nil || tx == nil || event.WorkflowJob == nil || tenantID == "" || now.IsZero() {
		return errors.New("valid Actions Runner Check update is required")
	}
	job := event.WorkflowJob
	var check ActionsRunnerCheck
	err := tx.QueryRow(ctx, `SELECT integration_id,tenant_id,installation_id,repository_id,workflow_job_id,workflow_run_id,run_attempt,
		commit_sha,check_name,external_id,COALESCE(check_run_id,0),status,COALESCE(conclusion,''),started_at,completed_at,output_title,output_summary,revision,COALESCE(correlation_id,'')
		FROM github_actions_runner_checks WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 FOR UPDATE`, integrationID, job.ID, job.RunAttempt).
		Scan(&check.IntegrationID, &check.TenantID, &check.Installation, &check.Repository, &check.WorkflowJobID, &check.WorkflowRunID, &check.RunAttempt,
			&check.CommitSHA, &check.Name, &check.ExternalID, &check.CheckRunID, &check.Status, &check.Conclusion, &check.StartedAt, &check.CompletedAt,
			&check.OutputTitle, &check.OutputSummary, &check.Revision, &check.CorrelationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("lock Actions Runner Check state: %w", err)
	}
	if check.TenantID != tenantID || check.Installation != event.InstallationID || event.Repository == nil || check.Repository != event.Repository.GitHubID ||
		check.WorkflowRunID != job.RunID || check.CommitSHA != job.HeadSHA {
		return errors.New("Actions Runner Check event identity mismatch")
	}
	status, conclusion := "in_progress", ""
	completedAt := (*time.Time)(nil)
	if event.Action == "completed" {
		status = "completed"
		conclusion = actionsRunnerCheckConclusion(job.Conclusion)
		if conclusion == "" {
			return errors.New("unsupported Actions Runner Check conclusion")
		}
		completed := now.UTC()
		if completed.Before(check.StartedAt) {
			completed = check.StartedAt
		}
		completedAt = &completed
	} else if event.Action != "in_progress" {
		return errors.New("unsupported Actions Runner Check transition")
	}
	if check.Status == "completed" {
		return nil
	}
	if check.Status == status && check.Conclusion == conclusion {
		return nil
	}
	if status == "completed" {
		check.OutputSummary = "GitHub reported this workflow job completed with conclusion: " + conclusion + "."
	} else {
		check.OutputSummary = "GitHub reported this workflow job is running on an isolated Lambda MicroVM runner."
	}
	tag, err := tx.Exec(ctx, `UPDATE github_actions_runner_checks SET status=$1,conclusion=NULLIF($2,''),completed_at=$3,
		output_summary=$4,revision=revision+1,updated_at=$5
		WHERE integration_id=$6 AND workflow_job_id=$7 AND run_attempt=$8 AND revision=$9`,
		status, conclusion, completedAt, check.OutputSummary, now.UTC(), integrationID, job.ID, job.RunAttempt, check.Revision)
	if err != nil || tag.RowsAffected() != 1 {
		if err != nil {
			return fmt.Errorf("update Actions Runner Check state: %w", err)
		}
		return errors.New("Actions Runner Check revision changed concurrently")
	}
	check.Status, check.Conclusion, check.CompletedAt, check.Revision = status, conclusion, completedAt, check.Revision+1
	return enqueueActionsRunnerCheck(ctx, tx, check, now)
}

// FailActionsRunnerCheck terminalizes only the exact durable job attempt when
// Resource Plane startup fails before GitHub can emit workflow_job completion.
func FailActionsRunnerCheck(ctx context.Context, tx pgx.Tx, integrationID string, workflowJobID, runAttempt int64, now time.Time) error {
	if ctx == nil || tx == nil || !validStoredIntegrationID(integrationID) || workflowJobID <= 0 || runAttempt <= 0 || now.IsZero() {
		return errors.New("valid Actions Runner Check failure identity and time are required")
	}
	var check ActionsRunnerCheck
	err := tx.QueryRow(ctx, `SELECT integration_id,tenant_id,installation_id,repository_id,workflow_job_id,workflow_run_id,run_attempt,
		commit_sha,check_name,external_id,COALESCE(check_run_id,0),status,COALESCE(conclusion,''),started_at,completed_at,output_title,output_summary,revision,COALESCE(correlation_id,'')
		FROM github_actions_runner_checks WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 FOR UPDATE`, integrationID, workflowJobID, runAttempt).
		Scan(&check.IntegrationID, &check.TenantID, &check.Installation, &check.Repository, &check.WorkflowJobID, &check.WorkflowRunID, &check.RunAttempt,
			&check.CommitSHA, &check.Name, &check.ExternalID, &check.CheckRunID, &check.Status, &check.Conclusion, &check.StartedAt, &check.CompletedAt,
			&check.OutputTitle, &check.OutputSummary, &check.Revision, &check.CorrelationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock Actions Runner Check for startup failure: %w", err)
	}
	if check.Status == "completed" {
		return nil
	}
	completed := now.UTC()
	if completed.Before(check.StartedAt) {
		completed = check.StartedAt
	}
	check.Status, check.Conclusion, check.CompletedAt = "completed", "failure", &completed
	check.OutputSummary = "Hako could not start an isolated Lambda MicroVM runner for this workflow job."
	tag, err := tx.Exec(ctx, `UPDATE github_actions_runner_checks SET status='completed',conclusion='failure',completed_at=$1,
		output_summary=$2,revision=revision+1,updated_at=$1 WHERE integration_id=$3 AND workflow_job_id=$4 AND run_attempt=$5 AND revision=$6`,
		completed, check.OutputSummary, integrationID, workflowJobID, runAttempt, check.Revision)
	if err != nil {
		return fmt.Errorf("persist Actions Runner Check startup failure: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("Actions Runner Check revision changed during startup failure")
	}
	check.Revision++
	return enqueueActionsRunnerCheck(ctx, tx, check, completed)
}

func enqueueActionsRunnerCheck(ctx context.Context, tx pgx.Tx, check ActionsRunnerCheck, now time.Time) error {
	payload, err := json.Marshal(actionsRunnerCheckOutboxEnvelope{SchemaVersion: 1, IntegrationID: check.IntegrationID,
		WorkflowJobID: check.WorkflowJobID, RunAttempt: check.RunAttempt, CorrelationID: check.CorrelationID, Revision: check.Revision})
	if err != nil {
		return fmt.Errorf("encode Actions Runner Check Outbox event: %w", err)
	}
	eventID, err := idgen.New("evt_")
	if err != nil {
		return fmt.Errorf("generate Actions Runner Check Outbox ID: %w", err)
	}
	aggregateID := fmt.Sprintf("%s:%d:%d", check.IntegrationID, check.WorkflowJobID, check.RunAttempt)
	if _, err := tx.Exec(ctx, `INSERT INTO outbox_events
		(id,aggregate_type,aggregate_id,event_type,payload_json,created_at,published_at,attempt)
		VALUES ($1,'github_actions_runner_job',$2,'github_actions_runner_job.github_check_requested',$3,$4,NULL,0)`,
		eventID, aggregateID, string(payload), now.UTC()); err != nil {
		return fmt.Errorf("enqueue Actions Runner Check delivery: %w", err)
	}
	return nil
}

func actionsRunnerCheckConclusion(value string) string {
	switch value {
	case "success", "neutral", "cancelled", "timed_out", "action_required", "stale", "skipped":
		return value
	case "failure", "startup_failure":
		return "failure"
	default:
		return ""
	}
}

func validRunnerCommitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

// GetActionsRunnerCheck loads only the immutable integration/job/attempt Check.
func GetActionsRunnerCheck(ctx context.Context, tx pgx.Tx, integrationID string, workflowJobID, runAttempt int64) (ActionsRunnerCheck, error) {
	if ctx == nil || tx == nil || !validStoredIntegrationID(integrationID) || workflowJobID <= 0 || runAttempt <= 0 {
		return ActionsRunnerCheck{}, errors.New("valid Actions Runner Check identity is required")
	}
	var check ActionsRunnerCheck
	err := tx.QueryRow(ctx, `SELECT integration_id,tenant_id,installation_id,repository_id,workflow_job_id,workflow_run_id,run_attempt,
		commit_sha,check_name,external_id,COALESCE(check_run_id,0),status,COALESCE(conclusion,''),started_at,completed_at,output_title,output_summary,revision,COALESCE(correlation_id,'')
		FROM github_actions_runner_checks WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3`, integrationID, workflowJobID, runAttempt).
		Scan(&check.IntegrationID, &check.TenantID, &check.Installation, &check.Repository, &check.WorkflowJobID, &check.WorkflowRunID, &check.RunAttempt,
			&check.CommitSHA, &check.Name, &check.ExternalID, &check.CheckRunID, &check.Status, &check.Conclusion, &check.StartedAt, &check.CompletedAt,
			&check.OutputTitle, &check.OutputSummary, &check.Revision, &check.CorrelationID)
	return check, err
}

// SaveActionsRunnerCheckRunID pins the remote Check ID to the exact immutable
// repository/commit and leaves current desired state untouched.
func SaveActionsRunnerCheckRunID(ctx context.Context, tx pgx.Tx, check ActionsRunnerCheck, checkRunID int64, now time.Time) error {
	if ctx == nil || tx == nil || checkRunID <= 0 || now.IsZero() || check.IntegrationID == "" || check.WorkflowJobID <= 0 || check.RunAttempt <= 0 ||
		check.TenantID == "" || check.Installation <= 0 || check.Repository <= 0 || !validRunnerCommitSHA(check.CommitSHA) || check.Revision <= 0 {
		return errors.New("valid Actions Runner Check and GitHub Check ID are required")
	}
	tag, err := tx.Exec(ctx, `UPDATE github_actions_runner_checks SET check_run_id=$1,updated_at=$2
		WHERE integration_id=$3 AND workflow_job_id=$4 AND run_attempt=$5 AND tenant_id=$6 AND installation_id=$7 AND repository_id=$8
		AND commit_sha=$9 AND revision>=$10 AND (check_run_id IS NULL OR check_run_id=$1)`,
		checkRunID, now.UTC(), check.IntegrationID, check.WorkflowJobID, check.RunAttempt, check.TenantID, check.Installation, check.Repository, check.CommitSHA, check.Revision)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("Actions Runner GitHub Check identity changed before Check ID persistence")
	}
	return nil
}
