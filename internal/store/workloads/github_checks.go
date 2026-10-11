package workloads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idgen"
)

type githubCheckState struct {
	status        string
	conclusion    string
	completedAt   *time.Time
	outputTitle   string
	outputSummary string
}

type githubCheckOutboxEnvelope struct {
	SchemaVersion int    `json:"schema_version"`
	TenantID      string `json:"tenant_id"`
	WorkloadRunID string `json:"workload_run_id"`
	Revision      int64  `json:"revision"`
}

// GitHubCheck is the durable desired state read by the trusted Check delivery
// worker. It deliberately contains no Installation token or private key data.
type GitHubCheck struct {
	TenantID             domain.TenantID
	WorkloadRunID        domain.WorkloadRunID
	GitHubInstallationID int64
	GitHubRepositoryID   int64
	CommitSHA            string
	Name                 string
	ExternalID           string
	CheckRunID           int64
	Status               string
	Conclusion           string
	StartedAt            time.Time
	CompletedAt          *time.Time
	OutputTitle          string
	OutputSummary        string
	Revision             int64
}

var ErrGitHubCheckIdentityMismatch = errors.New("GitHub Check ID does not match its immutable Workload Run identity")

func GetGitHubCheck(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, runID domain.WorkloadRunID) (GitHubCheck, error) {
	if tx == nil || tenantID == "" || runID == "" {
		return GitHubCheck{}, errors.New("transaction, Tenant ID, and Workload Run ID are required")
	}
	var check GitHubCheck
	var checkRunID *int64
	err := tx.QueryRow(ctx, `SELECT tenant_id, workload_run_id, github_installation_id, github_repository_id,
		commit_sha, check_name, external_id, check_run_id, status, COALESCE(conclusion,''), started_at,
		completed_at, output_title, output_summary, revision
		FROM workload_github_checks WHERE tenant_id = $1 AND workload_run_id = $2`, tenantID, runID).
		Scan(&check.TenantID, &check.WorkloadRunID, &check.GitHubInstallationID, &check.GitHubRepositoryID,
			&check.CommitSHA, &check.Name, &check.ExternalID, &checkRunID, &check.Status, &check.Conclusion,
			&check.StartedAt, &check.CompletedAt, &check.OutputTitle, &check.OutputSummary, &check.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return GitHubCheck{}, ErrNotFound
	}
	if err != nil {
		return GitHubCheck{}, fmt.Errorf("get durable GitHub Check: %w", err)
	}
	if checkRunID != nil {
		check.CheckRunID = *checkRunID
	}
	return check, nil
}

// SaveGitHubCheckRunID persists the remote ID only for the exact Tenant/Run/SHA
// and refuses to replace a different ID during concurrent delivery retries.
func SaveGitHubCheckRunID(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, runID domain.WorkloadRunID, commitSHA string, checkRunID int64, now time.Time) error {
	if tx == nil || tenantID == "" || runID == "" || !commitSHAPattern.MatchString(commitSHA) || checkRunID <= 0 || now.IsZero() {
		return errors.New("Tenant, Workload Run, commit SHA, Check ID, and timestamp are required")
	}
	tag, err := tx.Exec(ctx, `UPDATE workload_github_checks SET check_run_id = $1, updated_at = $2
		WHERE tenant_id = $3 AND workload_run_id = $4 AND commit_sha = $5 AND (check_run_id IS NULL OR check_run_id = $1)`,
		checkRunID, now.UTC(), tenantID, runID, commitSHA)
	if err != nil {
		return fmt.Errorf("persist GitHub Check Run ID: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrGitHubCheckIdentityMismatch
	}
	return nil
}

// syncGitHubCheck updates only durable desired Check metadata and emits an
// Outbox event in the same transaction as the Run transition. Network delivery
// is always performed later by a trusted result worker.
func syncGitHubCheck(ctx context.Context, tx pgx.Tx, run domain.WorkloadRun, occurredAt time.Time) error {
	if run.GitHubInstallation <= 0 && run.GitHubRepository <= 0 {
		return nil
	}
	if tx == nil || run.TenantID == "" || run.ID == "" || run.GitHubInstallation <= 0 || run.GitHubRepository <= 0 ||
		len(run.ID) > 255 || strings.ContainsAny(string(run.ID), "\r\n\x00") || !commitSHAPattern.MatchString(run.CommitSHA) || occurredAt.IsZero() {
		return errors.New("GitHub Check requires a complete immutable Workload Run identity")
	}
	state, err := githubCheckStateForRun(run)
	if err != nil {
		return err
	}
	name := "hako/" + string(run.Kind)
	if len(name) > 100 {
		return errors.New("GitHub Check name exceeds its size limit")
	}
	startedAt := run.CreatedAt.UTC()
	if run.StartedAt != nil {
		startedAt = run.StartedAt.UTC()
	}
	var revision int64
	var existingInstallation, existingRepository int64
	var existingSHA, existingName, existingExternalID, existingStatus, existingConclusion, existingTitle, existingSummary string
	var existingStarted time.Time
	var existingCompleted *time.Time
	err = tx.QueryRow(ctx, `SELECT github_installation_id, github_repository_id, commit_sha, check_name, external_id,
		status, COALESCE(conclusion, ''), started_at, completed_at, output_title, output_summary, revision
		FROM workload_github_checks WHERE tenant_id = $1 AND workload_run_id = $2 FOR UPDATE`, run.TenantID, run.ID).
		Scan(&existingInstallation, &existingRepository, &existingSHA, &existingName, &existingExternalID,
			&existingStatus, &existingConclusion, &existingStarted, &existingCompleted,
			&existingTitle, &existingSummary, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		revision = 1
		if _, err := tx.Exec(ctx, `INSERT INTO workload_github_checks
			(tenant_id, workload_run_id, github_installation_id, github_repository_id, commit_sha, check_name, external_id,
			 status, conclusion, started_at, completed_at, output_title, output_summary, revision, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,'')::text,$10,$11,$12,$13,$14,$15)`,
			run.TenantID, run.ID, run.GitHubInstallation, run.GitHubRepository, run.CommitSHA, name, string(run.ID),
			state.status, state.conclusion, startedAt, state.completedAt, state.outputTitle, state.outputSummary, revision, occurredAt.UTC()); err != nil {
			return fmt.Errorf("create durable GitHub Check state: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("load durable GitHub Check state: %w", err)
	} else {
		if existingInstallation != run.GitHubInstallation || existingRepository != run.GitHubRepository || existingSHA != run.CommitSHA ||
			existingName != name || existingExternalID != string(run.ID) {
			return errors.New("durable GitHub Check identity does not match its Workload Run")
		}
		if existingStatus == state.status && existingConclusion == state.conclusion && existingStarted.Equal(startedAt) &&
			sameOptionalTime(existingCompleted, state.completedAt) && existingTitle == state.outputTitle && existingSummary == state.outputSummary {
			return nil
		}
		tag, err := tx.Exec(ctx, `UPDATE workload_github_checks SET status = $1, conclusion = NULLIF($2,''), started_at = $3,
			completed_at = $4, output_title = $5, output_summary = $6, revision = revision + 1, updated_at = $7
			WHERE tenant_id = $8 AND workload_run_id = $9 AND revision = $10`,
			state.status, state.conclusion, startedAt, state.completedAt, state.outputTitle, state.outputSummary,
			occurredAt.UTC(), run.TenantID, run.ID, revision)
		if err != nil {
			return fmt.Errorf("update durable GitHub Check state: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return errors.New("durable GitHub Check revision changed concurrently")
		}
		revision++
	}

	eventID, err := idgen.New("evt_")
	if err != nil {
		return fmt.Errorf("generate GitHub Check Outbox ID: %w", err)
	}
	payload, err := json.Marshal(githubCheckOutboxEnvelope{SchemaVersion: 1, TenantID: string(run.TenantID), WorkloadRunID: string(run.ID), Revision: revision})
	if err != nil {
		return fmt.Errorf("encode GitHub Check Outbox event: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outbox_events
		(id, aggregate_type, aggregate_id, event_type, payload_json, created_at, published_at, attempt)
		VALUES ($1, 'workload_run', $2, 'workload_run.github_check_requested', $3, $4, NULL, 0)`,
		eventID, run.ID, string(payload), occurredAt.UTC()); err != nil {
		return fmt.Errorf("enqueue GitHub Check delivery: %w", err)
	}
	return nil
}

func githubCheckStateForRun(run domain.WorkloadRun) (githubCheckState, error) {
	state := githubCheckState{outputTitle: "Hako " + string(run.Kind), outputSummary: "Hako Workload Run is " + string(run.State) + "."}
	switch run.State {
	case domain.WorkloadPending, domain.WorkloadQueued:
		state.status = "queued"
	case domain.WorkloadProvisioning, domain.WorkloadRunning:
		state.status = "in_progress"
	case domain.WorkloadSucceeded:
		state.status, state.conclusion = "completed", "success"
	case domain.WorkloadFailed:
		state.status, state.conclusion = "completed", "failure"
	case domain.WorkloadCancelled:
		state.status, state.conclusion = "completed", "cancelled"
	case domain.WorkloadTimedOut:
		state.status, state.conclusion = "completed", "timed_out"
	default:
		return githubCheckState{}, errors.New("unsupported Workload state for GitHub Check")
	}
	if state.status == "completed" {
		if run.CompletedAt == nil {
			return githubCheckState{}, errors.New("terminal Workload Run is missing completion time for GitHub Check")
		}
		completed := run.CompletedAt.UTC()
		state.completedAt = &completed
	}
	if run.ErrorCode != "" {
		state.outputSummary += " Error code: " + run.ErrorCode + "."
	}
	return state, nil
}

func sameOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}
