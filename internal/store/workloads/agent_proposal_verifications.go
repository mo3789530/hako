package workloads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/agenttask"
	"github.com/mo3789530/hako/internal/domain"
)

var (
	ErrAgentProposalVerificationConflict = errors.New("Agent proposal verification binding conflicts with trusted Run state")
	ErrAgentProposalPublicationNotReady  = errors.New("Agent proposal is missing current required verification evidence or write policy")
	proposalSHA256Pattern                = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type AgentProposalPublicationEvidence struct {
	TenantID             domain.TenantID
	AgentRunID           domain.WorkloadRunID
	AgentAttemptID       string
	ProposalArtifactID   string
	ProposalSHA256       string
	BaseCommitSHA        string
	VerifierRunID        domain.WorkloadRunID
	VerifierAttemptID    string
	VerifierGeneration   int64
	GitHubInstallationID int64
	GitHubRepositoryID   int64
	GitHubIntegrationID  string
	GitHubConfigRevision int64
	GitHubCredentialGen  int64
	GitHubOwner          string
	GitHubRepository     string
	DefaultBranch        string
	Policy               agenttask.WritePolicy
	PassedChecks         map[string]bool
}

// ResolveAgentProposalPublicationEvidence returns only when the successful
// Agent artifact and every configured required Check are still bound to the
// same latest, cleaned verifier attempt and current Tenant/App/Repository
// policy. It is a read-only authorization primitive; callers must still obtain
// broker credentials and fence writes around GitHub calls.
func ResolveAgentProposalPublicationEvidence(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID,
	agentRunID domain.WorkloadRunID, agentAttemptID string, now time.Time) (AgentProposalPublicationEvidence, error) {
	if ctx == nil || tx == nil || tenantID == "" || agentRunID == "" || agentAttemptID == "" || now.IsZero() {
		return AgentProposalPublicationEvidence{}, ErrAgentProposalPublicationNotReady
	}
	var evidence AgentProposalPublicationEvidence
	var policyJSON []byte
	err := tx.QueryRow(ctx, `SELECT v.tenant_id,v.agent_run_id,v.agent_attempt_id,v.proposal_artifact_id,v.proposal_sha256,v.base_commit_sha,
		v.verifier_run_id,v.verifier_attempt_id,verifier_attempt.generation,
		agent.github_installation_id,agent.github_repository_id,COALESCE(agent.github_integration_id,''),
		COALESCE(agent.github_config_revision,0),COALESCE(agent.github_credential_generation,0),repo.owner_login,repo.repository_name,
		repo.default_branch,repo.agent_write_policy
		FROM workload_agent_proposal_verifications v
		JOIN workload_runs agent ON agent.tenant_id=v.tenant_id AND agent.id=v.agent_run_id
		JOIN workload_run_attempts agent_attempt ON agent_attempt.tenant_id=agent.tenant_id
		  AND agent_attempt.workload_run_id=agent.id AND agent_attempt.id=v.agent_attempt_id
		JOIN workload_artifacts artifact ON artifact.tenant_id=agent.tenant_id AND artifact.workload_run_id=agent.id
		  AND artifact.id=v.proposal_artifact_id
		JOIN workload_runs verifier ON verifier.tenant_id=v.tenant_id AND verifier.id=v.verifier_run_id
		JOIN workload_run_attempts verifier_attempt ON verifier_attempt.tenant_id=verifier.tenant_id
		  AND verifier_attempt.workload_run_id=verifier.id AND verifier_attempt.id=v.verifier_attempt_id
		JOIN tenant_github_repositories repo ON repo.tenant_id=agent.tenant_id
		  AND repo.installation_id=agent.github_installation_id AND repo.github_repository_id=agent.github_repository_id
		JOIN tenant_github_installations installation ON installation.tenant_id=agent.tenant_id
		  AND installation.installation_id=agent.github_installation_id
		JOIN github_app_installation_bindings shared_binding ON shared_binding.tenant_id=agent.tenant_id
		  AND shared_binding.installation_id=agent.github_installation_id
		WHERE v.tenant_id=$1 AND v.agent_run_id=$2 AND v.agent_attempt_id=$3
		  AND agent.kind='agent' AND agent.state='succeeded' AND agent.desired_state='running'
		  AND agent_attempt.state='succeeded' AND agent_attempt.cleanup_confirmed_at IS NOT NULL AND agent_attempt.cleanup_confirmed_at<=$4
		  AND NOT EXISTS (SELECT 1 FROM workload_run_attempts newer WHERE newer.tenant_id=agent_attempt.tenant_id
		      AND newer.workload_run_id=agent_attempt.workload_run_id AND newer.generation>agent_attempt.generation)
		  AND artifact.kind='agent_changes' AND artifact.sha256=v.proposal_sha256
		  AND (artifact.expires_at IS NULL OR artifact.expires_at>$4)
		  AND agent.commit_sha=v.base_commit_sha AND agent.resource_plane_id=verifier.resource_plane_id
		  AND verifier.kind='job' AND verifier.state='succeeded' AND verifier.desired_state='running'
		  AND verifier.commit_sha=v.base_commit_sha
		  AND verifier.github_installation_id=agent.github_installation_id
		  AND verifier.github_repository_id=agent.github_repository_id
		  AND verifier.github_integration_id IS NOT DISTINCT FROM agent.github_integration_id
		  AND verifier.github_config_revision IS NOT DISTINCT FROM agent.github_config_revision
		  AND verifier.github_credential_generation IS NOT DISTINCT FROM agent.github_credential_generation
		  AND verifier_attempt.state='succeeded' AND verifier_attempt.cleanup_confirmed_at IS NOT NULL AND verifier_attempt.cleanup_confirmed_at<=$4
		  AND NOT EXISTS (SELECT 1 FROM workload_run_attempts newer WHERE newer.tenant_id=verifier_attempt.tenant_id
		      AND newer.workload_run_id=verifier_attempt.workload_run_id AND newer.generation>verifier_attempt.generation)
		  AND repo.factory_enabled=true AND repo.factory_enabled_at IS NOT NULL AND repo.agent_write_policy IS NOT NULL
		  AND installation.status='active' AND installation.suspended_at IS NULL
		  AND ((agent.github_integration_id IS NULL AND NOT EXISTS (SELECT 1 FROM tenant_github_integrations active_app
		        WHERE active_app.tenant_id=agent.tenant_id AND active_app.status='active'))
		    OR (agent.github_integration_id IS NOT NULL AND EXISTS (
		        SELECT 1 FROM tenant_github_integrations i
		        JOIN github_app_registrations registration ON registration.id=i.app_registration_id AND registration.github_app_id=i.github_app_id
		        JOIN github_credential_bindings credential ON credential.tenant_id=i.tenant_id AND credential.id=i.credential_binding_id
		          AND credential.github_app_id=i.github_app_id
		        JOIN tenant_github_app_installation_bindings app_binding ON app_binding.tenant_id=i.tenant_id AND app_binding.integration_id=i.id
		        WHERE i.tenant_id=agent.tenant_id AND i.id=agent.github_integration_id AND i.status='active'
		          AND i.config_revision=agent.github_config_revision AND i.credential_generation=agent.github_credential_generation
		          AND registration.mode='tenant_owned' AND registration.owner_tenant_id=i.tenant_id
		          AND credential.status='approved' AND app_binding.installation_id=agent.github_installation_id)))`,
		tenantID, agentRunID, agentAttemptID, now.UTC()).Scan(
		&evidence.TenantID, &evidence.AgentRunID, &evidence.AgentAttemptID, &evidence.ProposalArtifactID,
		&evidence.ProposalSHA256, &evidence.BaseCommitSHA, &evidence.VerifierRunID, &evidence.VerifierAttemptID,
		&evidence.VerifierGeneration, &evidence.GitHubInstallationID, &evidence.GitHubRepositoryID, &evidence.GitHubIntegrationID,
		&evidence.GitHubConfigRevision, &evidence.GitHubCredentialGen, &evidence.GitHubOwner, &evidence.GitHubRepository,
		&evidence.DefaultBranch, &policyJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentProposalPublicationEvidence{}, ErrAgentProposalPublicationNotReady
	}
	if err != nil {
		return AgentProposalPublicationEvidence{}, fmt.Errorf("resolve Agent proposal publication pins: %w", err)
	}
	if json.Unmarshal(policyJSON, &evidence.Policy) != nil || evidence.Policy.Validate() != nil || !evidence.Policy.Enabled ||
		!commitSHAPattern.MatchString(evidence.BaseCommitSHA) || !proposalSHA256Pattern.MatchString(evidence.ProposalSHA256) {
		return AgentProposalPublicationEvidence{}, ErrAgentProposalPublicationNotReady
	}
	evidence.PassedChecks = make(map[string]bool, len(evidence.Policy.RequiredChecks))
	rows, err := tx.Query(ctx, `SELECT pipeline_name,status,exit_code,proposal_sha256,base_commit_sha,verifier_attempt_id
		FROM workload_agent_proposal_verification_checks
		WHERE tenant_id=$1 AND verifier_run_id=$2 AND verifier_attempt_id=$3 AND generation=$4 ORDER BY pipeline_name`,
		evidence.TenantID, evidence.VerifierRunID, evidence.VerifierAttemptID, evidence.VerifierGeneration)
	if err != nil {
		return AgentProposalPublicationEvidence{}, fmt.Errorf("load digest-bound verifier Checks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pipelineName, status, proposalSHA, baseCommit, verifierAttempt string
		var exitCode int
		if err := rows.Scan(&pipelineName, &status, &exitCode, &proposalSHA, &baseCommit, &verifierAttempt); err != nil {
			return AgentProposalPublicationEvidence{}, fmt.Errorf("scan digest-bound verifier Check: %w", err)
		}
		checkName := "hako/" + pipelineName
		if proposalSHA == evidence.ProposalSHA256 && baseCommit == evidence.BaseCommitSHA &&
			verifierAttempt == evidence.VerifierAttemptID && status == "succeeded" && exitCode == 0 {
			evidence.PassedChecks[checkName] = true
		}
	}
	if err := rows.Err(); err != nil {
		return AgentProposalPublicationEvidence{}, fmt.Errorf("iterate digest-bound verifier Checks: %w", err)
	}
	for _, required := range evidence.Policy.RequiredChecks {
		// The policy's public check names intentionally map to stable pipeline
		// names (`hako/test` -> `test`); checks outside that catalog fail closed.
		pipelineName := strings.TrimPrefix(required, "hako/")
		if !githubPipelineNamePattern.MatchString(pipelineName) || !evidence.PassedChecks[required] {
			return AgentProposalPublicationEvidence{}, ErrAgentProposalPublicationNotReady
		}
	}
	return evidence, nil
}

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
		  AND agent_attempt.id=$3 AND agent_attempt.cleanup_confirmed_at IS NOT NULL AND agent_attempt.cleanup_confirmed_at<=$9
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
		  AND verifier.resource_plane_id=agent.resource_plane_id AND verifier.trust_level='untrusted'
		  AND EXISTS (SELECT 1 FROM tenant_github_repositories repo
		      WHERE repo.tenant_id=agent.tenant_id AND repo.installation_id=agent.github_installation_id
		        AND repo.github_repository_id=agent.github_repository_id AND repo.factory_enabled=true)
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

func GetAgentProposalVerificationForVerifier(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID,
	verifierRunID domain.WorkloadRunID) (AgentProposalVerification, error) {
	if tx == nil || ctx == nil || tenantID == "" || verifierRunID == "" {
		return AgentProposalVerification{}, ErrAgentProposalVerificationConflict
	}
	var binding AgentProposalVerification
	err := tx.QueryRow(ctx, `SELECT tenant_id,agent_run_id,agent_attempt_id,proposal_artifact_id,proposal_sha256,
		base_commit_sha,verifier_run_id,verifier_attempt_id,created_at FROM workload_agent_proposal_verifications
		WHERE tenant_id=$1 AND verifier_run_id=$2`, tenantID, verifierRunID).
		Scan(&binding.TenantID, &binding.AgentRunID, &binding.AgentAttemptID, &binding.ProposalArtifactID,
			&binding.ProposalSHA256, &binding.BaseCommitSHA, &binding.VerifierRunID, &binding.VerifierAttemptID, &binding.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentProposalVerification{}, ErrNotFound
	}
	if err != nil {
		return AgentProposalVerification{}, fmt.Errorf("read Agent proposal verifier binding: %w", err)
	}
	return binding, nil
}

func RecordAgentProposalVerificationCheck(ctx context.Context, tx pgx.Tx, binding AgentProposalVerification,
	generation int64, pipeline domain.WorkloadPipelineResult, completedAt time.Time) error {
	if tx == nil || ctx == nil || binding.TenantID == "" || binding.VerifierRunID == "" || binding.VerifierAttemptID == "" ||
		!proposalSHA256Pattern.MatchString(binding.ProposalSHA256) || !commitSHAPattern.MatchString(binding.BaseCommitSHA) ||
		generation < 1 || !githubPipelineNamePattern.MatchString(pipeline.Name) || completedAt.IsZero() {
		return ErrAgentProposalVerificationConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workload_agent_proposal_verification_checks
		(tenant_id,verifier_run_id,verifier_attempt_id,generation,pipeline_name,proposal_sha256,base_commit_sha,status,exit_code,error_code,completed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),$11) ON CONFLICT DO NOTHING`, binding.TenantID,
		binding.VerifierRunID, binding.VerifierAttemptID, generation, pipeline.Name, binding.ProposalSHA256,
		binding.BaseCommitSHA, pipeline.Status, pipeline.ExitCode, pipeline.ErrorCode, completedAt.UTC()); err != nil {
		return fmt.Errorf("record digest-bound Agent proposal Check: %w", err)
	}
	var status, errorCode string
	var exitCode int
	var storedDigest, storedBase, storedAttempt string
	var storedCompletedAt time.Time
	err := tx.QueryRow(ctx, `SELECT status,exit_code,COALESCE(error_code,''),proposal_sha256,base_commit_sha,verifier_attempt_id,completed_at
		FROM workload_agent_proposal_verification_checks
		WHERE tenant_id=$1 AND verifier_run_id=$2 AND generation=$3 AND pipeline_name=$4`,
		binding.TenantID, binding.VerifierRunID, generation, pipeline.Name).
		Scan(&status, &exitCode, &errorCode, &storedDigest, &storedBase, &storedAttempt, &storedCompletedAt)
	if err != nil {
		return fmt.Errorf("read digest-bound Agent proposal Check: %w", err)
	}
	if status != pipeline.Status || exitCode != pipeline.ExitCode || errorCode != pipeline.ErrorCode ||
		storedDigest != binding.ProposalSHA256 || storedBase != binding.BaseCommitSHA || storedAttempt != binding.VerifierAttemptID ||
		!storedCompletedAt.Equal(completedAt.UTC()) {
		return ErrAgentProposalVerificationConflict
	}
	return nil
}
