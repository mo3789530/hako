package workloads

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

var (
	ErrAgentProposalVerificationConflict = errors.New("Agent proposal verification binding conflicts with trusted Run state")
	proposalSHA256Pattern                = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// AgentProposalVerification binds one immutable Agent proposal to one isolated
// Job Run. It intentionally contains no storage reference or credential.
type AgentProposalVerification struct {
	TenantID           domain.TenantID
	AgentRunID         domain.WorkloadRunID
	AgentAttemptID     string
	ProposalArtifactID string
	ProposalSHA256     string
	BaseCommitSHA      string
	VerifierRunID      domain.WorkloadRunID
	VerifierAttemptID  string
	CreatedAt          time.Time
}

// BindAgentProposalVerification accepts only a successful Agent attempt whose
// MicroVM cleanup is confirmed and whose proposal artifact digest matches. The
// verifier must be a separate, untrusted Job for the same pinned repository and
// base commit. Retrying the exact binding is idempotent; changing any pin is a
// conflict.
func BindAgentProposalVerification(ctx context.Context, tx pgx.Tx, binding AgentProposalVerification) error {
	if tx == nil || ctx == nil || binding.TenantID == "" || binding.AgentRunID == "" || binding.AgentAttemptID == "" ||
		binding.ProposalArtifactID == "" || !proposalSHA256Pattern.MatchString(binding.ProposalSHA256) ||
		!commitSHAPattern.MatchString(binding.BaseCommitSHA) || binding.VerifierRunID == "" || binding.VerifierAttemptID == "" || binding.CreatedAt.IsZero() {
		return ErrAgentProposalVerificationConflict
	}
	binding.CreatedAt = binding.CreatedAt.UTC()
	var valid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1
		FROM workload_runs agent
		JOIN workload_run_attempts agent_attempt
		  ON agent_attempt.tenant_id=agent.tenant_id AND agent_attempt.workload_run_id=agent.id
		JOIN workload_artifacts artifact
		  ON artifact.tenant_id=agent.tenant_id AND artifact.workload_run_id=agent.id
		JOIN workload_runs verifier
		  ON verifier.tenant_id=agent.tenant_id AND verifier.id=$7
		JOIN workload_run_attempts verifier_attempt
		  ON verifier_attempt.tenant_id=verifier.tenant_id AND verifier_attempt.workload_run_id=verifier.id
		WHERE agent.tenant_id=$1 AND agent.id=$2 AND agent.kind='agent' AND agent.state='succeeded'
		  AND agent_attempt.id=$3 AND agent_attempt.state='cleanup_confirmed'
		  AND NOT EXISTS (SELECT 1 FROM workload_run_attempts newer WHERE newer.tenant_id=agent.tenant_id
		      AND newer.workload_run_id=agent.id AND newer.generation>agent_attempt.generation)
		  AND artifact.id=$4 AND artifact.kind='agent_changes' AND artifact.sha256=$5
		  AND (artifact.expires_at IS NULL OR artifact.expires_at>$9)
		  AND agent.commit_sha=$6
		  AND verifier.id<>agent.id AND verifier.kind='job' AND verifier.commit_sha=agent.commit_sha
		  AND verifier.github_installation_id=agent.github_installation_id
		  AND verifier.github_repository_id=agent.github_repository_id
		  AND verifier.github_integration_id IS NOT DISTINCT FROM agent.github_integration_id
		  AND verifier.github_config_revision IS NOT DISTINCT FROM agent.github_config_revision
		  AND verifier.github_credential_generation IS NOT DISTINCT FROM agent.github_credential_generation
		  AND verifier.trust_level='untrusted'
		  AND verifier.desired_state='running' AND verifier.state IN ('pending','queued','provisioning','running')
		  AND verifier_attempt.id=$8 AND verifier_attempt.resource_plane_id=verifier.resource_plane_id
		  AND verifier_attempt.state IN ('dispatch_pending','accepted','running')
		  AND NOT EXISTS (SELECT 1 FROM workload_run_attempts newer WHERE newer.tenant_id=verifier.tenant_id
		      AND newer.workload_run_id=verifier.id AND newer.generation>verifier_attempt.generation)
	)`, binding.TenantID, binding.AgentRunID, binding.AgentAttemptID, binding.ProposalArtifactID,
		binding.ProposalSHA256, binding.BaseCommitSHA, binding.VerifierRunID, binding.VerifierAttemptID, binding.CreatedAt).Scan(&valid)
	if err != nil {
		return fmt.Errorf("validate Agent proposal verification binding: %w", err)
	}
	if !valid {
		return ErrAgentProposalVerificationConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO workload_agent_proposal_verifications
		(tenant_id,agent_run_id,agent_attempt_id,proposal_artifact_id,proposal_sha256,base_commit_sha,verifier_run_id,verifier_attempt_id,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`, binding.TenantID, binding.AgentRunID,
		binding.AgentAttemptID, binding.ProposalArtifactID, binding.ProposalSHA256, binding.BaseCommitSHA,
		binding.VerifierRunID, binding.VerifierAttemptID, binding.CreatedAt)
	if err != nil {
		return fmt.Errorf("persist Agent proposal verification binding: %w", err)
	}
	var stored AgentProposalVerification
	err = tx.QueryRow(ctx, `SELECT tenant_id,agent_run_id,agent_attempt_id,proposal_artifact_id,proposal_sha256,base_commit_sha,
		verifier_run_id,verifier_attempt_id,created_at FROM workload_agent_proposal_verifications
		WHERE tenant_id=$1 AND agent_run_id=$2 AND agent_attempt_id=$3`, binding.TenantID, binding.AgentRunID, binding.AgentAttemptID).
		Scan(&stored.TenantID, &stored.AgentRunID, &stored.AgentAttemptID, &stored.ProposalArtifactID, &stored.ProposalSHA256,
			&stored.BaseCommitSHA, &stored.VerifierRunID, &stored.VerifierAttemptID, &stored.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAgentProposalVerificationConflict
	}
	if err != nil {
		return fmt.Errorf("read Agent proposal verification binding: %w", err)
	}
	if stored.TenantID != binding.TenantID || stored.AgentRunID != binding.AgentRunID || stored.AgentAttemptID != binding.AgentAttemptID ||
		stored.ProposalArtifactID != binding.ProposalArtifactID || stored.ProposalSHA256 != binding.ProposalSHA256 ||
		stored.BaseCommitSHA != binding.BaseCommitSHA || stored.VerifierRunID != binding.VerifierRunID ||
		stored.VerifierAttemptID != binding.VerifierAttemptID || !stored.CreatedAt.Equal(binding.CreatedAt) {
		return ErrAgentProposalVerificationConflict
	}
	return nil
}

// GetAgentProposalVerification returns the immutable binding for an Agent
// attempt. Callers must separately authorize the Tenant and verifier Run.
func GetAgentProposalVerification(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID,
	agentRunID domain.WorkloadRunID, agentAttemptID string) (AgentProposalVerification, error) {
	if tx == nil || ctx == nil || tenantID == "" || agentRunID == "" || agentAttemptID == "" {
		return AgentProposalVerification{}, ErrAgentProposalVerificationConflict
	}
	var binding AgentProposalVerification
	err := tx.QueryRow(ctx, `SELECT tenant_id,agent_run_id,agent_attempt_id,proposal_artifact_id,proposal_sha256,
		base_commit_sha,verifier_run_id,verifier_attempt_id,created_at
		FROM workload_agent_proposal_verifications WHERE tenant_id=$1 AND agent_run_id=$2 AND agent_attempt_id=$3`,
		tenantID, agentRunID, agentAttemptID).Scan(&binding.TenantID, &binding.AgentRunID, &binding.AgentAttemptID,
		&binding.ProposalArtifactID, &binding.ProposalSHA256, &binding.BaseCommitSHA, &binding.VerifierRunID,
		&binding.VerifierAttemptID, &binding.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentProposalVerification{}, ErrNotFound
	}
	if err != nil {
		return AgentProposalVerification{}, fmt.Errorf("read Agent proposal verification binding: %w", err)
	}
	return binding, nil
}
