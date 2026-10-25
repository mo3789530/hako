package workloads

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

var ErrExecutionSpecNotFound = errors.New("Workload execution specification not found")

// ExecutionMetadata is the minimum non-secret Control Plane data needed by a
// trusted Resource Plane resolver. It intentionally excludes AWS image and
// network configuration, which are Resource Plane-owned, and all credentials.
type ExecutionMetadata struct {
	Kind                 domain.WorkloadKind
	TenantID             domain.TenantID
	WorkloadRunID        domain.WorkloadRunID
	AttemptID            string
	Generation           int64
	ResourcePlaneID      domain.ResourcePlaneID
	CorrelationID        string
	GitHubInstallationID int64
	GitHubRepositoryID   int64
	GitHubOwner          string
	GitHubRepository     string
	GitHubIntegrationID  string
	GitHubConfigRevision int64
	GitHubCredentialGen  int64
	CommitSHA            string
	Ref                  string
	TriggerEvent         string
	TriggerBranch        string
	RuntimeClass         string
	TrustLevel           domain.WorkloadTrustLevel
	RequestedAt          time.Time
	Deadline             time.Time
	ProposalVerification *AgentProposalVerification
}

// ResolveExecutionMetadata revalidates the active Run, its current attempt,
// Placement, Repository/Installation, and pinned Tenant App identity in one
// read transaction. No fields from the dispatch payload are treated as
// authority beyond the lookup identity supplied by the authenticated caller.
func ResolveExecutionMetadata(ctx context.Context, tx pgx.Tx, resourcePlaneID domain.ResourcePlaneID,
	tenantID domain.TenantID, runID domain.WorkloadRunID, attemptID string, generation int64, now time.Time) (ExecutionMetadata, error) {
	if ctx == nil || tx == nil || resourcePlaneID == "" || tenantID == "" || runID == "" || attemptID == "" || generation < 1 || now.IsZero() {
		return ExecutionMetadata{}, ErrExecutionSpecNotFound
	}
	var result ExecutionMetadata
	var proposalAgentRunID, proposalAgentAttemptID, proposalArtifactID, proposalSHA256, proposalBaseCommitSHA *string
	err := tx.QueryRow(ctx, `
		SELECT wr.kind, wr.tenant_id, wr.id, a.id, a.generation, a.resource_plane_id, COALESCE(wr.correlation_id, ''),
		       wr.github_installation_id, wr.github_repository_id, repo.owner_login, repo.repository_name,
		       COALESCE(wr.github_integration_id, ''), COALESCE(wr.github_config_revision, 0),
		       COALESCE(wr.github_credential_generation, 0), wr.commit_sha, wr.ref, wr.trigger_event, wr.trigger_branch, wr.runtime_class,
		       wr.trust_level, wr.created_at, wr.timeout_at,
	       proposal.agent_run_id, proposal.agent_attempt_id, proposal.proposal_artifact_id,
	       proposal.proposal_sha256, proposal.base_commit_sha
		FROM workload_runs wr
		JOIN workload_run_attempts a
		  ON a.tenant_id = wr.tenant_id AND a.workload_run_id = wr.id
		JOIN tenant_github_installations installation
		  ON installation.tenant_id = wr.tenant_id AND installation.installation_id = wr.github_installation_id
		JOIN github_app_installation_bindings shared_binding
		  ON shared_binding.tenant_id = wr.tenant_id AND shared_binding.installation_id = wr.github_installation_id
		JOIN tenant_github_repositories repo
		  ON repo.tenant_id = wr.tenant_id AND repo.installation_id = wr.github_installation_id
		 AND repo.github_repository_id = wr.github_repository_id
		LEFT JOIN workload_agent_proposal_verifications proposal
		  ON proposal.tenant_id=wr.tenant_id AND proposal.verifier_run_id=wr.id AND proposal.verifier_attempt_id=a.id
		WHERE wr.tenant_id = $1 AND wr.id = $2 AND a.id = $3 AND a.generation = $4
		  AND wr.resource_plane_id = $5 AND a.resource_plane_id = $5
		  AND ((wr.kind='job' AND (wr.trigger_event='' OR wr.trigger_event IN ('push','pull_request'))) OR
		       (wr.kind='agent' AND wr.trigger_event='issue_comment'))
		  AND (wr.kind <> 'agent' OR wr.github_integration_id IS NOT NULL)
		  AND wr.desired_state = 'running' AND wr.state IN ('provisioning', 'running')
		  AND a.state IN ('dispatch_pending', 'accepted', 'running')
		  AND wr.timeout_at > $6
		  AND (proposal.agent_run_id IS NULL OR EXISTS (
		      SELECT 1 FROM workload_runs agent
		      JOIN workload_run_attempts source_attempt
		        ON source_attempt.tenant_id=agent.tenant_id AND source_attempt.workload_run_id=agent.id
		      JOIN workload_artifacts artifact
		        ON artifact.tenant_id=agent.tenant_id AND artifact.workload_run_id=agent.id
		      WHERE agent.tenant_id=proposal.tenant_id AND agent.id=proposal.agent_run_id AND agent.kind='agent'
		        AND agent.state='succeeded' AND agent.commit_sha=proposal.base_commit_sha
		        AND agent.github_installation_id=wr.github_installation_id AND agent.github_repository_id=wr.github_repository_id
		        AND agent.github_integration_id IS NOT DISTINCT FROM wr.github_integration_id
		        AND agent.github_config_revision IS NOT DISTINCT FROM wr.github_config_revision
		        AND agent.github_credential_generation IS NOT DISTINCT FROM wr.github_credential_generation
		        AND agent.resource_plane_id=wr.resource_plane_id
		        AND EXISTS (SELECT 1 FROM tenant_github_repositories repo
		            WHERE repo.tenant_id=agent.tenant_id AND repo.installation_id=agent.github_installation_id
		              AND repo.github_repository_id=agent.github_repository_id AND repo.factory_enabled=true)
		        AND source_attempt.id=proposal.agent_attempt_id AND source_attempt.cleanup_confirmed_at IS NOT NULL
		        AND source_attempt.cleanup_confirmed_at<=$6
		        AND NOT EXISTS (SELECT 1 FROM workload_run_attempts newer
		            WHERE newer.tenant_id=agent.tenant_id AND newer.workload_run_id=agent.id AND newer.generation>source_attempt.generation)
		        AND artifact.id=proposal.proposal_artifact_id AND artifact.kind='agent_changes'
		        AND artifact.sha256=proposal.proposal_sha256 AND (artifact.expires_at IS NULL OR artifact.expires_at>$6)
		  ))
		  AND (proposal.agent_run_id IS NOT NULL OR NOT EXISTS (
		      SELECT 1 FROM workload_agent_proposal_verifications retry_fence
		      WHERE retry_fence.tenant_id=wr.tenant_id AND retry_fence.verifier_run_id=wr.id
		  ))
		  AND installation.status = 'active' AND installation.suspended_at IS NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM workload_run_attempts newer
		      WHERE newer.tenant_id = a.tenant_id AND newer.workload_run_id = a.workload_run_id
		        AND newer.generation > a.generation
		  )
		  AND (
		      (wr.github_integration_id IS NULL AND NOT EXISTS (
		          SELECT 1 FROM tenant_github_integrations active_app
		          WHERE active_app.tenant_id = wr.tenant_id AND active_app.status = 'active'
		      ))
		      OR
		      (wr.github_integration_id IS NOT NULL AND EXISTS (
		          SELECT 1
		          FROM tenant_github_integrations i
		          JOIN github_app_registrations registration
		            ON registration.id = i.app_registration_id AND registration.github_app_id = i.github_app_id
		          JOIN github_credential_bindings credential
		            ON credential.tenant_id = i.tenant_id AND credential.id = i.credential_binding_id
		           AND credential.github_app_id = i.github_app_id
		          JOIN tenant_github_app_installation_bindings app_binding
		            ON app_binding.tenant_id = i.tenant_id AND app_binding.integration_id = i.id
		          WHERE i.tenant_id = wr.tenant_id AND i.id = wr.github_integration_id
		            AND i.status = 'active' AND i.config_revision = wr.github_config_revision
		            AND i.credential_generation = wr.github_credential_generation
		            AND registration.mode = 'tenant_owned' AND registration.owner_tenant_id = i.tenant_id
		            AND credential.status = 'approved' AND app_binding.installation_id = wr.github_installation_id
		      ))
		  )`, tenantID, runID, attemptID, generation, resourcePlaneID, now.UTC()).Scan(
		&result.Kind, &result.TenantID, &result.WorkloadRunID, &result.AttemptID, &result.Generation, &result.ResourcePlaneID, &result.CorrelationID,
		&result.GitHubInstallationID, &result.GitHubRepositoryID, &result.GitHubOwner, &result.GitHubRepository,
		&result.GitHubIntegrationID, &result.GitHubConfigRevision, &result.GitHubCredentialGen,
		&result.CommitSHA, &result.Ref, &result.TriggerEvent, &result.TriggerBranch, &result.RuntimeClass, &result.TrustLevel, &result.RequestedAt, &result.Deadline,
		&proposalAgentRunID, &proposalAgentAttemptID, &proposalArtifactID, &proposalSHA256, &proposalBaseCommitSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		return ExecutionMetadata{}, ErrExecutionSpecNotFound
	}
	if err != nil {
		return ExecutionMetadata{}, fmt.Errorf("resolve trusted Workload execution metadata: %w", err)
	}
	if proposalAgentRunID != nil {
		result.ProposalVerification = &AgentProposalVerification{TenantID: result.TenantID, AgentRunID: domain.WorkloadRunID(*proposalAgentRunID),
			AgentAttemptID: *proposalAgentAttemptID, ProposalArtifactID: *proposalArtifactID, ProposalSHA256: *proposalSHA256,
			BaseCommitSHA: *proposalBaseCommitSHA, VerifierRunID: result.WorkloadRunID, VerifierAttemptID: result.AttemptID}
	}
	return result, nil
}
