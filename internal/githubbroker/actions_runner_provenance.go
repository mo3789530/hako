package githubbroker

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
)

type ActionsRunnerLaunchPreflight func(context.Context, ActionsRunnerRegistrationRequest) ([]byte, error)

type VerifiedActionsRunnerLaunchStarter func(context.Context, string, ActionsRunnerRegistrationRequest, []byte) (ActionsRunnerLaunchCommand, error)

type runnerProvenanceIdentity struct {
	IntegrationID  string
	TenantID       domain.TenantID
	Installation   int64
	Repository     int64
	WorkflowJobID  int64
	WorkflowRunID  int64
	RunAttempt     int64
	WorkflowName   string
	JobName        string
	HeadBranch     string
	HeadSHA        string
	Labels         []string
	ClaimID        string
	LeaseExpires   time.Time
	Owner          string
	RepositoryName string
	Credential     githubapp.TenantPrivateKeyAccess
	PolicyJSON     []byte
}

// NewPostgresActionsRunnerLaunchPreflight resolves an optional Tenant policy
// from the live claimed row, verifies GitHub's authoritative Run/Job metadata
// outside the transaction, and returns the exact stored JSONB bytes. The
// verified starter compares that snapshot again while consuming the claim.
func NewPostgresActionsRunnerLaunchPreflight(pool transaction.Beginner, policy transaction.Policy, config aws.Config,
	httpClient *http.Client) (ActionsRunnerLaunchPreflight, error) {
	if pool == nil || strings.TrimSpace(config.Region) == "" {
		return nil, ErrUnavailable
	}
	runClient, err := githubapp.NewActionsRunClient(httpClient)
	if err != nil {
		return nil, ErrUnavailable
	}
	authorizer := githubregistry.CredentialAuthorizer{Pool: pool, Policy: policy}
	return func(ctx context.Context, request ActionsRunnerRegistrationRequest) ([]byte, error) {
		if ctx == nil || !validActionsRunnerRegistrationRequest(request) || request.RunAttempt <= 0 {
			return nil, ErrActionsRunnerLaunchDenied
		}
		identity, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (runnerProvenanceIdentity, error) {
			var result runnerProvenanceIdentity
			var labelsJSON []byte
			err := tx.QueryRow(ctx, `SELECT j.integration_id,j.tenant_id,j.installation_id,j.repository_id,j.workflow_job_id,j.workflow_run_id,j.run_attempt,
				j.workflow_name,j.job_name,j.head_branch,j.head_sha,j.labels,j.claim_id,j.lease_expires_at,r.owner_login,r.repository_name,
				r.actions_runner_workflow_policy
				FROM github_actions_runner_jobs j
				JOIN github_webhook_deliveries d ON d.integration_id=j.integration_id AND d.delivery_id=j.delivery_id
				JOIN tenant_github_integrations i ON i.id=j.integration_id AND i.tenant_id=j.tenant_id
				JOIN tenant_github_app_installation_bindings b ON b.tenant_id=i.tenant_id AND b.integration_id=i.id AND b.installation_id=j.installation_id
				JOIN tenant_github_installations x ON x.tenant_id=b.tenant_id AND x.installation_id=b.installation_id
				JOIN tenant_github_repositories r ON r.tenant_id=j.tenant_id AND r.installation_id=j.installation_id AND r.github_repository_id=j.repository_id
				WHERE j.integration_id=$1 AND j.workflow_job_id=$2 AND j.run_attempt=$3 AND j.claim_id=$4
				AND ((j.state='claimed' AND j.resource_plane_id IS NULL AND j.lambda_microvm_id IS NULL) OR
				(j.state='starting' AND j.registration_token_issued_at IS NULL))
				AND j.lease_expires_at>$5 AND d.processing_status='processed' AND d.credential_generation=i.credential_generation
				AND i.status='active' AND x.status='active' AND x.suspended_at IS NULL
				AND r.actions_runner_enabled=TRUE AND r.actions_runner_enabled_at IS NOT NULL AND r.actions_runner_enabled_at<=d.received_at`,
				request.IntegrationID, request.WorkflowJobID, request.RunAttempt, request.ClaimID, time.Now().UTC()).
				Scan(&result.IntegrationID, &result.TenantID, &result.Installation, &result.Repository, &result.WorkflowJobID, &result.WorkflowRunID,
					&result.RunAttempt, &result.WorkflowName, &result.JobName, &result.HeadBranch, &result.HeadSHA, &labelsJSON, &result.ClaimID,
					&result.LeaseExpires, &result.Owner, &result.RepositoryName, &result.PolicyJSON)
			if err != nil || json.Unmarshal(labelsJSON, &result.Labels) != nil || result.ClaimID != request.ClaimID ||
				result.WorkflowJobID != request.WorkflowJobID || result.RunAttempt != request.RunAttempt || result.WorkflowRunID <= 0 ||
				!result.LeaseExpires.After(time.Now().UTC()) {
				return runnerProvenanceIdentity{}, ErrActionsRunnerLaunchDenied
			}
			if len(result.PolicyJSON) == 0 {
				return result, nil
			}
			var revision, generation int64
			if err := tx.QueryRow(ctx, `SELECT config_revision,credential_generation FROM tenant_github_integrations WHERE id=$1 AND tenant_id=$2 AND status='active'`,
				result.IntegrationID, result.TenantID).Scan(&revision, &generation); err != nil {
				return runnerProvenanceIdentity{}, ErrActionsRunnerLaunchDenied
			}
			credential, err := githubregistry.ResolveActiveAppCredential(ctx, tx, result.TenantID, result.IntegrationID, revision, generation)
			if err != nil || credential.GitHubAppID <= 0 || credential.CredentialGeneration != generation || credential.ConfigRevision != revision {
				return runnerProvenanceIdentity{}, ErrActionsRunnerLaunchDenied
			}
			result.Credential = githubapp.TenantPrivateKeyAccess{TenantID: string(credential.TenantID), IntegrationID: credential.IntegrationID,
				GitHubAppID: credential.GitHubAppID, ConfigRevision: credential.ConfigRevision, CredentialGeneration: credential.CredentialGeneration,
				ReaderRoleARN: credential.PrivateKeyReaderRoleARN, SecretARN: credential.PrivateKeySecretARN, SecretVersionID: credential.PrivateKeyVersionID}
			return result, nil
		})
		if err != nil {
			return nil, ErrActionsRunnerLaunchDenied
		}
		if len(identity.PolicyJSON) == 0 {
			return nil, nil
		}
		var policy githubregistry.ActionsRunnerWorkflowPolicy
		if json.Unmarshal(identity.PolicyJSON, &policy) != nil || !githubregistry.ValidActionsRunnerWorkflowPolicy(policy) {
			return nil, ErrActionsRunnerLaunchDenied
		}
		tokens, err := githubapp.NewTenantAppTokenClientFromConfig(identity.Credential, config, authorizer, httpClient)
		if err != nil {
			return nil, ErrActionsRunnerLaunchDenied
		}
		token, repository, err := tokens.ActionsRunnerProvenanceToken(ctx, identity.TenantID, identity.Installation, identity.Repository)
		if err != nil || repository.Owner != identity.Owner || repository.Name != identity.RepositoryName {
			return nil, ErrActionsRunnerLaunchDenied
		}
		snapshot, err := runClient.Fetch(ctx, token, repository, identity.Repository, identity.WorkflowRunID, identity.RunAttempt, identity.WorkflowJobID)
		if err != nil || !verifyRunnerPolicyJob(identity, policy, snapshot) {
			return nil, ErrActionsRunnerLaunchDenied
		}
		return append([]byte(nil), identity.PolicyJSON...), nil
	}, nil
}

func verifyRunnerPolicyJob(identity runnerProvenanceIdentity, policy githubregistry.ActionsRunnerWorkflowPolicy, run githubapp.ActionsRunSnapshot) bool {
	if run.RepositoryID != identity.Repository || run.HeadRepository != identity.Repository || run.RunID != identity.WorkflowRunID ||
		run.RunAttempt != identity.RunAttempt || run.JobID != identity.WorkflowJobID || run.WorkflowPath != policy.WorkflowPath ||
		run.HeadSHA != strings.ToLower(identity.HeadSHA) || run.HeadBranch != identity.HeadBranch || run.WorkflowRef != "refs/heads/"+identity.HeadBranch ||
		run.JobHeadBranch != identity.HeadBranch || run.JobName != identity.JobName || (identity.WorkflowName != "" && run.WorkflowName != identity.WorkflowName) ||
		run.Event == "pull_request_target" || !containsString(policy.AllowedRefs, run.WorkflowRef) || !containsString(policy.AllowedEvents, run.Event) ||
		!containsAllStrings(run.Labels, policy.RequiredLabels) || !containsOnlyStrings(run.Labels, policy.AllowedLabels) || !sameStringsAsSet(run.Labels, identity.Labels) {
		return false
	}
	return true
}

func containsString(values []string, value string) bool { return slices.Contains(values, value) }

func containsAllStrings(values, required []string) bool {
	for _, requiredValue := range required {
		if !slices.Contains(values, requiredValue) {
			return false
		}
	}
	return true
}

func containsOnlyStrings(values, allowed []string) bool {
	if len(values) == 0 {
		return false
	}
	seen := map[string]struct{}{}
	for _, value := range values {
		if value == "" || !slices.Contains(allowed, value) {
			return false
		}
		if _, ok := seen[value]; ok {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func sameStringsAsSet(left, right []string) bool {
	return len(left) == len(right) && containsAllStrings(left, right) && containsAllStrings(right, left)
}
