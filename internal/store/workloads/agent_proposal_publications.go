package workloads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	ErrAgentProposalPublicationConflict = errors.New("Agent proposal publication identity conflicts with its immutable pins")
	publicationBranchSHA                = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
)

type AgentProposalPublicationState string

const (
	AgentProposalPublicationRequested          AgentProposalPublicationState = "requested"
	AgentProposalPublicationBranchCreated      AgentProposalPublicationState = "branch_created"
	AgentProposalPublicationPullRequestCreated AgentProposalPublicationState = "pull_request_created"
)

// AgentProposalPublication is the durable idempotency record for one exact
// proposal attempt. It contains GitHub identifiers only; credentials and
// artifact locations never enter this record.
type AgentProposalPublication struct {
	TenantID             domain.TenantID
	AgentRunID           domain.WorkloadRunID
	AgentAttemptID       string
	ProposalArtifactID   string
	ProposalSHA256       string
	BaseCommitSHA        string
	VerifierRunID        domain.WorkloadRunID
	VerifierAttemptID    string
	VerifierGeneration   int64
	InstallationID       int64
	RepositoryID         int64
	IntegrationID        string
	ConfigRevision       int64
	CredentialGeneration int64
	DefaultBranch        string
	PolicySHA256         string
	BranchName           string
	State                AgentProposalPublicationState
	BranchCommitSHA      string
	PullRequestNumber    int64
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// CreateOrGetAgentProposalPublication reserves at most one deterministic
// branch for an Agent attempt. The evidence is resolved again in this
// transaction so callers cannot use a stale or caller-constructed proof.
func CreateOrGetAgentProposalPublication(ctx context.Context, tx pgx.Tx, requested AgentProposalPublication,
	now time.Time) (AgentProposalPublication, error) {
	if ctx == nil || tx == nil || now.IsZero() || requested.TenantID == "" || requested.AgentRunID == "" || requested.AgentAttemptID == "" {
		return AgentProposalPublication{}, ErrAgentProposalPublicationConflict
	}
	evidence, err := ResolveAgentProposalPublicationEvidence(ctx, tx, requested.TenantID, requested.AgentRunID, requested.AgentAttemptID, now)
	if err != nil {
		return AgentProposalPublication{}, err
	}
	branchName := deterministicProposalBranch(evidence)
	if requested.BranchName != "" && requested.BranchName != branchName {
		return AgentProposalPublication{}, ErrAgentProposalPublicationConflict
	}
	requested.BranchName = branchName
	if !publicationMatchesEvidence(requested, evidence) || !branchAllowedByEvidence(evidence, requested.BranchName) {
		return AgentProposalPublication{}, ErrAgentProposalPublicationConflict
	}
	policyBytes, err := json.Marshal(evidence.Policy)
	if err != nil {
		return AgentProposalPublication{}, fmt.Errorf("encode Agent proposal write policy: %w", err)
	}
	policyDigest := sha256.Sum256(policyBytes)
	requested.PolicySHA256 = hex.EncodeToString(policyDigest[:])
	requested.VerifierGeneration = evidence.VerifierGeneration
	requested.DefaultBranch = evidence.DefaultBranch
	requested.State = AgentProposalPublicationRequested
	requested.CreatedAt = now.UTC()
	requested.UpdatedAt = now.UTC()
	_, err = tx.Exec(ctx, `INSERT INTO workload_agent_proposal_publications
		(tenant_id,agent_run_id,agent_attempt_id,proposal_artifact_id,proposal_sha256,base_commit_sha,
		 verifier_run_id,verifier_attempt_id,verifier_generation,installation_id,repository_id,integration_id,
		 config_revision,credential_generation,default_branch,policy_sha256,branch_name,state,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''),$13,$14,$15,$16,$17,'requested',$18,$18)
		ON CONFLICT DO NOTHING`, requested.TenantID, requested.AgentRunID, requested.AgentAttemptID,
		requested.ProposalArtifactID, requested.ProposalSHA256, requested.BaseCommitSHA, requested.VerifierRunID,
		requested.VerifierAttemptID, requested.VerifierGeneration, requested.InstallationID, requested.RepositoryID,
		requested.IntegrationID, requested.ConfigRevision, requested.CredentialGeneration, requested.DefaultBranch,
		requested.PolicySHA256, requested.BranchName, requested.CreatedAt)
	if err != nil {
		return AgentProposalPublication{}, fmt.Errorf("reserve Agent proposal publication: %w", err)
	}
	stored, err := GetAgentProposalPublication(ctx, tx, requested.TenantID, requested.AgentRunID, requested.AgentAttemptID)
	if errors.Is(err, ErrNotFound) {
		return AgentProposalPublication{}, ErrAgentProposalPublicationConflict
	}
	if err != nil {
		return AgentProposalPublication{}, err
	}
	if !samePublicationPins(stored, requested) {
		return AgentProposalPublication{}, ErrAgentProposalPublicationConflict
	}
	return stored, nil
}

// RecordAgentProposalPublicationBranch monotonically records a branch commit.
// Replays with the same commit are idempotent; a different commit is a hard
// conflict, and a published PR can never be moved to a new branch commit.
func RecordAgentProposalPublicationBranch(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID,
	runID domain.WorkloadRunID, attemptID, commitSHA string, now time.Time) (AgentProposalPublication, error) {
	if ctx == nil || tx == nil || tenantID == "" || runID == "" || attemptID == "" ||
		!publicationBranchSHA.MatchString(commitSHA) || now.IsZero() {
		return AgentProposalPublication{}, ErrAgentProposalPublicationConflict
	}
	_, err := tx.Exec(ctx, `UPDATE workload_agent_proposal_publications
		SET state=CASE WHEN state='requested' THEN 'branch_created' ELSE state END,
		    branch_commit_sha=COALESCE(branch_commit_sha,$4),updated_at=$5
		WHERE tenant_id=$1 AND agent_run_id=$2 AND agent_attempt_id=$3
		  AND (branch_commit_sha IS NULL OR branch_commit_sha=$4)`, tenantID, runID, attemptID, commitSHA, now.UTC())
	if err != nil {
		return AgentProposalPublication{}, fmt.Errorf("record Agent proposal branch: %w", err)
	}
	stored, err := GetAgentProposalPublication(ctx, tx, tenantID, runID, attemptID)
	if err != nil {
		return AgentProposalPublication{}, err
	}
	if stored.BranchCommitSHA != commitSHA || stored.State == AgentProposalPublicationRequested {
		return AgentProposalPublication{}, ErrAgentProposalPublicationConflict
	}
	return stored, nil
}

// RecordAgentProposalPublicationPullRequest records one PR only after a branch
// commit was durably confirmed. Duplicate reports must name the same PR.
func RecordAgentProposalPublicationPullRequest(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID,
	runID domain.WorkloadRunID, attemptID string, number int64, now time.Time) (AgentProposalPublication, error) {
	if ctx == nil || tx == nil || tenantID == "" || runID == "" || attemptID == "" || number < 1 || now.IsZero() {
		return AgentProposalPublication{}, ErrAgentProposalPublicationConflict
	}
	_, err := tx.Exec(ctx, `UPDATE workload_agent_proposal_publications
		SET state='pull_request_created',pull_request_number=COALESCE(pull_request_number,$4),updated_at=$5
		WHERE tenant_id=$1 AND agent_run_id=$2 AND agent_attempt_id=$3
		  AND branch_commit_sha IS NOT NULL AND (pull_request_number IS NULL OR pull_request_number=$4)`,
		tenantID, runID, attemptID, number, now.UTC())
	if err != nil {
		return AgentProposalPublication{}, fmt.Errorf("record Agent proposal pull request: %w", err)
	}
	stored, err := GetAgentProposalPublication(ctx, tx, tenantID, runID, attemptID)
	if err != nil {
		return AgentProposalPublication{}, err
	}
	if stored.PullRequestNumber != number || stored.State != AgentProposalPublicationPullRequestCreated {
		return AgentProposalPublication{}, ErrAgentProposalPublicationConflict
	}
	return stored, nil
}

// GetAgentProposalPublication returns safe status metadata for one attempt.
func GetAgentProposalPublication(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID,
	runID domain.WorkloadRunID, attemptID string) (AgentProposalPublication, error) {
	if ctx == nil || tx == nil || tenantID == "" || runID == "" || attemptID == "" {
		return AgentProposalPublication{}, ErrAgentProposalPublicationConflict
	}
	return scanAgentProposalPublication(tx.QueryRow(ctx, agentProposalPublicationSelect+` WHERE tenant_id=$1 AND agent_run_id=$2 AND agent_attempt_id=$3`,
		tenantID, runID, attemptID))
}

// GetLatestAgentProposalPublication returns the newest attempt's safe
// publication record for an Agent Run.
func GetLatestAgentProposalPublication(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID,
	runID domain.WorkloadRunID) (AgentProposalPublication, error) {
	if ctx == nil || tx == nil || tenantID == "" || runID == "" {
		return AgentProposalPublication{}, ErrAgentProposalPublicationConflict
	}
	return scanAgentProposalPublication(tx.QueryRow(ctx, agentProposalPublicationSelect+` WHERE tenant_id=$1 AND agent_run_id=$2
		ORDER BY (SELECT attempt.generation FROM workload_run_attempts attempt
		  WHERE attempt.tenant_id=workload_agent_proposal_publications.tenant_id
		    AND attempt.workload_run_id=workload_agent_proposal_publications.agent_run_id
		    AND attempt.id=workload_agent_proposal_publications.agent_attempt_id) DESC,
		 created_at DESC,agent_attempt_id DESC LIMIT 1`, tenantID, runID))
}

const agentProposalPublicationSelect = `SELECT tenant_id,agent_run_id,agent_attempt_id,proposal_artifact_id,proposal_sha256,base_commit_sha,
		verifier_run_id,verifier_attempt_id,verifier_generation,installation_id,repository_id,COALESCE(integration_id,''),
		config_revision,credential_generation,default_branch,policy_sha256,branch_name,state,
		COALESCE(branch_commit_sha,''),COALESCE(pull_request_number,0),created_at,updated_at
		FROM workload_agent_proposal_publications`

func scanAgentProposalPublication(row pgx.Row) (AgentProposalPublication, error) {
	var result AgentProposalPublication
	err := row.Scan(&result.TenantID, &result.AgentRunID, &result.AgentAttemptID,
		&result.ProposalArtifactID, &result.ProposalSHA256, &result.BaseCommitSHA, &result.VerifierRunID,
		&result.VerifierAttemptID, &result.VerifierGeneration, &result.InstallationID, &result.RepositoryID,
		&result.IntegrationID, &result.ConfigRevision, &result.CredentialGeneration, &result.DefaultBranch,
		&result.PolicySHA256, &result.BranchName, &result.State, &result.BranchCommitSHA,
		&result.PullRequestNumber, &result.CreatedAt, &result.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentProposalPublication{}, ErrNotFound
	}
	if err != nil {
		return AgentProposalPublication{}, fmt.Errorf("read Agent proposal publication: %w", err)
	}
	return result, nil
}

func publicationMatchesEvidence(requested AgentProposalPublication, evidence AgentProposalPublicationEvidence) bool {
	return requested.ProposalArtifactID == evidence.ProposalArtifactID && requested.ProposalSHA256 == evidence.ProposalSHA256 &&
		requested.BaseCommitSHA == evidence.BaseCommitSHA && requested.VerifierRunID == evidence.VerifierRunID &&
		requested.VerifierAttemptID == evidence.VerifierAttemptID && requested.InstallationID == evidence.GitHubInstallationID &&
		requested.RepositoryID == evidence.GitHubRepositoryID && requested.IntegrationID == evidence.GitHubIntegrationID &&
		requested.ConfigRevision == evidence.GitHubConfigRevision && requested.CredentialGeneration == evidence.GitHubCredentialGen
}

func branchAllowedByEvidence(evidence AgentProposalPublicationEvidence, branch string) bool {
	return agenttask.AuthorizeBranchPush(evidence.Policy, branch, evidence.DefaultBranch, true, evidence.PassedChecks) == nil
}

func deterministicProposalBranch(evidence AgentProposalPublicationEvidence) string {
	if len(evidence.Policy.BranchPrefixes) == 0 {
		return ""
	}
	identity := strings.Join([]string{string(evidence.TenantID), string(evidence.AgentRunID), evidence.AgentAttemptID,
		evidence.ProposalSHA256, string(evidence.VerifierRunID), evidence.VerifierAttemptID}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return evidence.Policy.BranchPrefixes[0] + "proposal-" + hex.EncodeToString(digest[:12])
}

func samePublicationPins(left, right AgentProposalPublication) bool {
	return left.TenantID == right.TenantID && left.AgentRunID == right.AgentRunID && left.AgentAttemptID == right.AgentAttemptID &&
		left.ProposalArtifactID == right.ProposalArtifactID && left.ProposalSHA256 == right.ProposalSHA256 &&
		left.BaseCommitSHA == right.BaseCommitSHA && left.VerifierRunID == right.VerifierRunID &&
		left.VerifierAttemptID == right.VerifierAttemptID && left.VerifierGeneration == right.VerifierGeneration &&
		left.InstallationID == right.InstallationID && left.RepositoryID == right.RepositoryID &&
		left.IntegrationID == right.IntegrationID && left.ConfigRevision == right.ConfigRevision &&
		left.CredentialGeneration == right.CredentialGeneration && left.DefaultBranch == right.DefaultBranch &&
		left.PolicySHA256 == right.PolicySHA256 && left.BranchName == right.BranchName
}
