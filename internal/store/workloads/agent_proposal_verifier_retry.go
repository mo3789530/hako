package workloads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idgen"
)

var (
	ErrAgentProposalVerifierNotRetryable = errors.New("Agent proposal verifier is not eligible for retry")
	ErrAgentProposalVerifierRetryLimit   = errors.New("Agent proposal verifier retry limit reached")
)

const maxAgentProposalVerifierAttempts = 3 // initial attempt plus two retries

type AgentProposalVerifierRetry struct {
	VerifierRunID     domain.WorkloadRunID
	VerifierAttemptID string
	State             string
	Queued            bool
}

// RetryFailedAgentProposalVerifier re-dispatches only a failed, cleaned
// verifier attempt while preserving its Agent proposal and repository pins.
// Active retries and successful verification are idempotent observations.
func RetryFailedAgentProposalVerifier(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID,
	agentRunID domain.WorkloadRunID, agentAttemptID string, now time.Time) (AgentProposalVerifierRetry, error) {
	if ctx == nil || tx == nil || tenantID == "" || agentRunID == "" || agentAttemptID == "" || now.IsZero() {
		return AgentProposalVerifierRetry{}, ErrAgentProposalVerifierNotRetryable
	}
	now = now.UTC()
	binding, err := GetAgentProposalVerification(ctx, tx, tenantID, agentRunID, agentAttemptID)
	if err != nil {
		return AgentProposalVerifierRetry{}, ErrAgentProposalVerifierNotRetryable
	}
	run, err := Get(ctx, tx, tenantID, binding.VerifierRunID)
	if err != nil || run.Kind != domain.WorkloadJob || run.DesiredState != domain.WorkloadDesiredRunning ||
		run.CommitSHA != binding.BaseCommitSHA || run.ResourcePlaneID == "" {
		return AgentProposalVerifierRetry{}, ErrAgentProposalVerifierNotRetryable
	}
	var eligible bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM workload_runs agent
		JOIN workload_run_attempts agent_attempt ON agent_attempt.tenant_id=agent.tenant_id AND agent_attempt.workload_run_id=agent.id
		JOIN workload_artifacts artifact ON artifact.tenant_id=agent.tenant_id AND artifact.workload_run_id=agent.id
		JOIN tenant_github_repositories repo ON repo.tenant_id=agent.tenant_id AND repo.installation_id=agent.github_installation_id
		  AND repo.github_repository_id=agent.github_repository_id
		WHERE agent.tenant_id=$1 AND agent.id=$2 AND agent.kind='agent' AND agent.state='succeeded'
		  AND agent.desired_state='running' AND agent_attempt.id=$3 AND agent_attempt.state='succeeded'
		  AND agent_attempt.cleanup_confirmed_at IS NOT NULL AND agent_attempt.cleanup_confirmed_at<=$4
		  AND NOT EXISTS (SELECT 1 FROM workload_run_attempts newer WHERE newer.tenant_id=agent_attempt.tenant_id
		    AND newer.workload_run_id=agent_attempt.workload_run_id AND newer.generation>agent_attempt.generation)
		  AND artifact.id=$5 AND artifact.kind='agent_changes' AND artifact.sha256=$6
		  AND (artifact.expires_at IS NULL OR artifact.expires_at>$4)
		  AND agent.commit_sha=$7 AND agent.resource_plane_id=$8
		  AND agent.github_installation_id=$9 AND agent.github_repository_id=$10
		  AND agent.github_integration_id IS NOT DISTINCT FROM $11
		  AND agent.github_config_revision IS NOT DISTINCT FROM $12
		  AND agent.github_credential_generation IS NOT DISTINCT FROM $13
		  AND repo.factory_enabled=true AND repo.factory_enabled_at IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM workload_agent_proposal_publications publication
		    WHERE publication.tenant_id=$1 AND publication.agent_run_id=$2 AND publication.agent_attempt_id=$3)
	)`, tenantID, agentRunID, agentAttemptID, now, binding.ProposalArtifactID, binding.ProposalSHA256,
		binding.BaseCommitSHA, run.ResourcePlaneID, run.GitHubInstallation, run.GitHubRepository,
		nullableString(run.GitHubIntegrationID), nullableInt64(run.GitHubConfigRevision), nullableInt64(run.GitHubCredentialGeneration)).Scan(&eligible)
	if err != nil {
		return AgentProposalVerifierRetry{}, fmt.Errorf("validate Agent proposal verifier retry: %w", err)
	}
	if !eligible {
		return AgentProposalVerifierRetry{}, ErrAgentProposalVerifierNotRetryable
	}
	var latestAttemptID, latestAttemptState string
	var generation, attemptNumber int64
	err = tx.QueryRow(ctx, `SELECT id,state,generation,attempt_number FROM workload_run_attempts
		WHERE tenant_id=$1 AND workload_run_id=$2 ORDER BY generation DESC LIMIT 1`, tenantID, run.ID).
		Scan(&latestAttemptID, &latestAttemptState, &generation, &attemptNumber)
	if err != nil {
		return AgentProposalVerifierRetry{}, fmt.Errorf("read current proposal verifier attempt: %w", err)
	}
	if latestAttemptID != binding.VerifierAttemptID {
		return AgentProposalVerifierRetry{}, ErrAgentProposalVerifierNotRetryable
	}
	switch run.State {
	case domain.WorkloadPending, domain.WorkloadQueued, domain.WorkloadProvisioning, domain.WorkloadRunning:
		if latestAttemptState != "dispatch_pending" && latestAttemptState != "accepted" && latestAttemptState != "running" {
			return AgentProposalVerifierRetry{}, ErrAgentProposalVerifierNotRetryable
		}
		return AgentProposalVerifierRetry{VerifierRunID: run.ID, VerifierAttemptID: latestAttemptID, State: string(run.State), Queued: true}, nil
	case domain.WorkloadSucceeded:
		if latestAttemptState != "succeeded" {
			return AgentProposalVerifierRetry{}, ErrAgentProposalVerifierNotRetryable
		}
		return AgentProposalVerifierRetry{VerifierRunID: run.ID, VerifierAttemptID: latestAttemptID, State: string(run.State)}, nil
	case domain.WorkloadFailed:
		if latestAttemptState != "failed" {
			return AgentProposalVerifierRetry{}, ErrAgentProposalVerifierNotRetryable
		}
		var cleanupConfirmed bool
		if err := tx.QueryRow(ctx, `SELECT cleanup_confirmed_at IS NOT NULL FROM workload_run_attempts WHERE id=$1`, latestAttemptID).Scan(&cleanupConfirmed); err != nil {
			return AgentProposalVerifierRetry{}, fmt.Errorf("check failed verifier cleanup: %w", err)
		}
		if !cleanupConfirmed {
			return AgentProposalVerifierRetry{}, ErrAgentProposalVerifierNotRetryable
		}
	default:
		return AgentProposalVerifierRetry{}, ErrAgentProposalVerifierNotRetryable
	}
	if attemptNumber >= maxAgentProposalVerifierAttempts {
		return AgentProposalVerifierRetry{}, ErrAgentProposalVerifierRetryLimit
	}
	newAttemptID, err := idgen.New("attempt_")
	if err != nil {
		return AgentProposalVerifierRetry{}, fmt.Errorf("generate proposal verifier retry attempt: %w", err)
	}
	token, err := idgen.New("token_")
	if err != nil {
		return AgentProposalVerifierRetry{}, fmt.Errorf("generate proposal verifier retry token: %w", err)
	}
	eventID, err := idgen.New("evt_")
	if err != nil {
		return AgentProposalVerifierRetry{}, fmt.Errorf("generate proposal verifier retry event: %w", err)
	}
	// Reopening and quota reservation are one transaction. A competing retry
	// loses the revision/state compare and transaction retry observes the active
	// attempt above, so it cannot dispatch a second copy.
	if _, err := tx.Exec(ctx, `DELETE FROM workload_quota_slots WHERE tenant_id=$1 AND workload_run_id=$2`, tenantID, run.ID); err != nil {
		return AgentProposalVerifierRetry{}, fmt.Errorf("clear terminal verifier quota slot before retry: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workload_artifact_quota_reservations WHERE tenant_id=$1 AND workload_run_id=$2`, tenantID, run.ID); err != nil {
		return AgentProposalVerifierRetry{}, fmt.Errorf("clear terminal verifier artifact quota before retry: %w", err)
	}
	run, err = scanRun(tx.QueryRow(ctx, `UPDATE workload_runs SET state='provisioning',desired_state='running',
		completed_at=NULL,error_code=NULL,timeout_at=$1,revision=revision+1,updated_at=$2
		WHERE tenant_id=$3 AND id=$4 AND revision=$5 AND state='failed' AND desired_state='running'
		RETURNING `+runColumns, now.Add(time.Duration(run.TimeoutSeconds)*time.Second), now, tenantID, run.ID, run.Revision))
	if errors.Is(err, pgx.ErrNoRows) {
		var currentAttemptID, attemptState string
		var currentState domain.WorkloadRunState
		reconcileErr := tx.QueryRow(ctx, `SELECT verification.verifier_attempt_id,run.state,attempt.state
			FROM workload_agent_proposal_verifications verification
			JOIN workload_runs run ON run.tenant_id=verification.tenant_id AND run.id=verification.verifier_run_id
			JOIN workload_run_attempts attempt ON attempt.tenant_id=run.tenant_id AND attempt.workload_run_id=run.id
			  AND attempt.id=verification.verifier_attempt_id
			WHERE verification.tenant_id=$1 AND verification.agent_run_id=$2 AND verification.agent_attempt_id=$3`,
			tenantID, agentRunID, agentAttemptID).Scan(&currentAttemptID, &currentState, &attemptState)
		if reconcileErr == nil && currentAttemptID != binding.VerifierAttemptID {
			switch currentState {
			case domain.WorkloadPending, domain.WorkloadQueued, domain.WorkloadProvisioning, domain.WorkloadRunning:
				if attemptState == "dispatch_pending" || attemptState == "accepted" || attemptState == "running" {
					return AgentProposalVerifierRetry{VerifierRunID: run.ID, VerifierAttemptID: currentAttemptID,
						State: string(currentState), Queued: true}, nil
				}
			case domain.WorkloadSucceeded:
				if attemptState == "succeeded" {
					return AgentProposalVerifierRetry{VerifierRunID: run.ID, VerifierAttemptID: currentAttemptID,
						State: string(currentState)}, nil
				}
			}
		}
		return AgentProposalVerifierRetry{}, ErrTransitionConflict
	}
	if err != nil {
		return AgentProposalVerifierRetry{}, fmt.Errorf("reopen failed proposal verifier: %w", err)
	}
	quotaRun := run
	quotaRun.CreatedAt = now
	if err := reserveConcurrentRunSlot(ctx, tx, quotaRun); err != nil {
		return AgentProposalVerifierRetry{}, err
	}
	if err := reserveArtifactQuota(ctx, tx, quotaRun); err != nil {
		return AgentProposalVerifierRetry{}, err
	}
	if err := appendEvent(ctx, tx, run, "workload.provisioning", now); err != nil {
		return AgentProposalVerifierRetry{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workload_run_attempts
		(id,tenant_id,workload_run_id,generation,attempt_number,resource_plane_id,idempotency_token,state,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'dispatch_pending',$8,$8)`, newAttemptID, tenantID, run.ID,
		generation+1, attemptNumber+1, run.ResourcePlaneID, token, now); err != nil {
		return AgentProposalVerifierRetry{}, fmt.Errorf("create proposal verifier retry attempt: %w", err)
	}
	command := struct {
		SchemaVersion    int                    `json:"schema_version"`
		TenantID         domain.TenantID        `json:"tenant_id"`
		WorkloadRunID    domain.WorkloadRunID   `json:"workload_run_id"`
		AttemptID        string                 `json:"attempt_id"`
		Generation       int64                  `json:"generation"`
		ResourcePlaneID  domain.ResourcePlaneID `json:"resource_plane_id"`
		IdempotencyToken string                 `json:"idempotency_token"`
		CorrelationID    string                 `json:"correlation_id,omitempty"`
	}{1, tenantID, run.ID, newAttemptID, generation + 1, run.ResourcePlaneID, token, run.CorrelationID}
	payload, err := json.Marshal(command)
	if err != nil {
		return AgentProposalVerifierRetry{}, fmt.Errorf("encode proposal verifier retry command: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outbox_events(id,aggregate_type,aggregate_id,event_type,payload_json,created_at,published_at,attempt)
		VALUES ($1,'workload_attempt',$2,'workload_attempt.dispatch',$3,$4,NULL,0)`, eventID, newAttemptID, string(payload), now); err != nil {
		return AgentProposalVerifierRetry{}, fmt.Errorf("enqueue proposal verifier retry: %w", err)
	}
	updated, err := tx.Exec(ctx, `UPDATE workload_agent_proposal_verifications SET verifier_attempt_id=$1
		WHERE tenant_id=$2 AND agent_run_id=$3 AND agent_attempt_id=$4 AND verifier_run_id=$5 AND verifier_attempt_id=$6`,
		newAttemptID, tenantID, agentRunID, agentAttemptID, run.ID, binding.VerifierAttemptID)
	if err != nil || updated.RowsAffected() != 1 {
		if err != nil {
			return AgentProposalVerifierRetry{}, fmt.Errorf("bind proposal verifier retry attempt: %w", err)
		}
		return AgentProposalVerifierRetry{}, ErrAgentProposalVerifierNotRetryable
	}
	return AgentProposalVerifierRetry{VerifierRunID: run.ID, VerifierAttemptID: newAttemptID,
		State: string(domain.WorkloadProvisioning), Queued: true}, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableInt64(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}
