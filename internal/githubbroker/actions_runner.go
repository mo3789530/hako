package githubbroker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var ErrActionsRunnerDenied = errors.New("GitHub Actions runner registration denied")

type ActionsRunnerRegistrationRequest struct {
	IntegrationID string `json:"integration_id"`
	WorkflowJobID int64  `json:"workflow_job_id"`
	RunAttempt    int64  `json:"run_attempt"`
	ClaimID       string `json:"claim_id"`
}

type ActionsRunnerJobIdentity struct {
	IntegrationID   string
	DeliveryID      string
	TenantID        domain.TenantID
	Installation    int64
	Repository      int64
	WorkflowJobID   int64
	WorkflowRunID   int64
	RunAttempt      int64
	WorkflowName    string
	JobName         string
	HeadBranch      string
	HeadSHA         string
	Labels          []string
	ClaimID         string
	LeaseExpires    time.Time
	ResourcePlaneID string
	LambdaMicroVMID string
	Credential      githubapp.TenantPrivateKeyAccess
}

type ActionsRunnerJobStarter func(context.Context, ActionsRunnerRegistrationRequest) (ActionsRunnerJobIdentity, error)
type ActionsRunnerJobFailure func(context.Context, ActionsRunnerJobIdentity) error
type ActionsRunnerRegistrationTokenMinter func(context.Context, ActionsRunnerJobIdentity) (githubapp.RunnerRegistrationToken, githubapp.RepositoryAccess, error)

type ActionsRunnerRegistrationService struct {
	start ActionsRunnerJobStarter
	fail  ActionsRunnerJobFailure
	mint  ActionsRunnerRegistrationTokenMinter
	clock func() time.Time
}

type ActionsRunnerRegistrationGrant struct {
	Token      githubapp.RunnerRegistrationToken
	Repository githubapp.RepositoryAccess
}

func NewActionsRunnerRegistrationService(start ActionsRunnerJobStarter, fail ActionsRunnerJobFailure, mint ActionsRunnerRegistrationTokenMinter) (*ActionsRunnerRegistrationService, error) {
	if start == nil || fail == nil || mint == nil {
		return nil, ErrUnavailable
	}
	return &ActionsRunnerRegistrationService{start: start, fail: fail, mint: mint, clock: time.Now}, nil
}

func NewPostgresActionsRunnerJobStarter(pool transaction.Beginner, policy transaction.Policy) ActionsRunnerJobStarter {
	return func(ctx context.Context, request ActionsRunnerRegistrationRequest) (ActionsRunnerJobIdentity, error) {
		if pool == nil || ctx == nil || !validActionsRunnerRegistrationRequest(request) {
			return ActionsRunnerJobIdentity{}, ErrActionsRunnerDenied
		}
		now := time.Now().UTC()
		identity, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (ActionsRunnerJobIdentity, error) {
			var result ActionsRunnerJobIdentity
			var labelsJSON []byte
			var revision, generation int64
			err := tx.QueryRow(ctx, `SELECT j.integration_id,j.delivery_id,j.tenant_id,j.installation_id,j.repository_id,j.workflow_job_id,j.workflow_run_id,j.run_attempt,
				j.workflow_name,j.job_name,j.head_branch,j.head_sha,j.labels,j.claim_id,j.lease_expires_at,i.config_revision,i.credential_generation
				FROM github_actions_runner_jobs j
				JOIN github_webhook_deliveries d ON d.integration_id=j.integration_id AND d.delivery_id=j.delivery_id
				JOIN tenant_github_integrations i ON i.id=j.integration_id AND i.tenant_id=j.tenant_id
				JOIN tenant_github_app_installation_bindings b ON b.tenant_id=i.tenant_id AND b.integration_id=i.id AND b.installation_id=j.installation_id
				JOIN tenant_github_installations x ON x.tenant_id=b.tenant_id AND x.installation_id=b.installation_id
				JOIN tenant_github_repositories r ON r.tenant_id=j.tenant_id AND r.installation_id=j.installation_id AND r.github_repository_id=j.repository_id
				WHERE j.integration_id=$1 AND j.workflow_job_id=$2 AND j.run_attempt=$3 AND j.state='claimed' AND j.claim_id=$4
				AND j.lease_expires_at>$5 AND d.processing_status='processed' AND d.credential_generation=i.credential_generation
				AND i.status='active' AND x.status='active' AND x.suspended_at IS NULL
				AND r.actions_runner_enabled=TRUE AND r.actions_runner_enabled_at IS NOT NULL AND r.actions_runner_enabled_at<=d.received_at`,
				request.IntegrationID, request.WorkflowJobID, request.RunAttempt, request.ClaimID, now).
				Scan(&result.IntegrationID, &result.DeliveryID, &result.TenantID, &result.Installation, &result.Repository, &result.WorkflowJobID, &result.WorkflowRunID, &result.RunAttempt,
					&result.WorkflowName, &result.JobName, &result.HeadBranch, &result.HeadSHA, &labelsJSON, &result.ClaimID, &result.LeaseExpires, &revision, &generation)
			if err != nil {
				return ActionsRunnerJobIdentity{}, ErrActionsRunnerDenied
			}
			if err := json.Unmarshal(labelsJSON, &result.Labels); err != nil {
				return ActionsRunnerJobIdentity{}, ErrActionsRunnerDenied
			}
			tag, err := tx.Exec(ctx, `UPDATE github_actions_runner_jobs SET state='starting',updated_at=$5
				WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 AND state='claimed' AND claim_id=$4 AND lease_expires_at>$5`,
				request.IntegrationID, request.WorkflowJobID, request.RunAttempt, request.ClaimID, now)
			if err != nil || tag.RowsAffected() != 1 {
				return ActionsRunnerJobIdentity{}, ErrActionsRunnerDenied
			}
			credential, err := githubregistry.ResolveActiveAppCredential(ctx, tx, result.TenantID, result.IntegrationID, revision, generation)
			if err != nil || credential.GitHubAppID <= 0 || credential.CredentialGeneration != generation || credential.ConfigRevision != revision {
				return ActionsRunnerJobIdentity{}, ErrActionsRunnerDenied
			}
			result.Credential = githubapp.TenantPrivateKeyAccess{TenantID: string(credential.TenantID), IntegrationID: credential.IntegrationID,
				GitHubAppID: credential.GitHubAppID, ConfigRevision: credential.ConfigRevision, CredentialGeneration: credential.CredentialGeneration,
				ReaderRoleARN: credential.PrivateKeyReaderRoleARN, SecretARN: credential.PrivateKeySecretARN, SecretVersionID: credential.PrivateKeyVersionID}
			return result, nil
		})
		if err != nil {
			return ActionsRunnerJobIdentity{}, ErrActionsRunnerDenied
		}
		return identity, nil
	}
}

func NewPostgresActionsRunnerJobFailure(pool transaction.Beginner, policy transaction.Policy) ActionsRunnerJobFailure {
	return func(ctx context.Context, identity ActionsRunnerJobIdentity) error {
		if pool == nil || ctx == nil || !validActionsRunnerRegistrationRequest(ActionsRunnerRegistrationRequest{
			IntegrationID: identity.IntegrationID, WorkflowJobID: identity.WorkflowJobID, RunAttempt: identity.RunAttempt, ClaimID: identity.ClaimID}) {
			return ErrActionsRunnerDenied
		}
		_, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
			now := time.Now().UTC()
			_, err := tx.Exec(ctx, `UPDATE github_actions_runner_jobs SET state='failed',claim_id=NULL,lease_expires_at=NULL,updated_at=$5
				WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 AND state='starting' AND claim_id=$4`,
				identity.IntegrationID, identity.WorkflowJobID, identity.RunAttempt, identity.ClaimID, now)
			if err != nil {
				return struct{}{}, err
			}
			// Token issuance happens before Resource Plane startup. A failure can
			// therefore race with a successful MicroVM launch; record terminal
			// cleanup in the same transaction and carry the immutable provider ID.
			_, err = tx.Exec(ctx, `INSERT INTO github_actions_runner_cleanup_outbox
				(integration_id,workflow_job_id,run_attempt,tenant_id,installation_id,repository_id,runner_id,resource_plane_id,lambda_microvm_id,reason,state,available_at,created_at,updated_at)
				SELECT integration_id,workflow_job_id,run_attempt,tenant_id,installation_id,repository_id,runner_id,resource_plane_id,lambda_microvm_id,'failed','pending',$4,$4,$4
				FROM github_actions_runner_jobs WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 AND state='failed'
				ON CONFLICT (integration_id,workflow_job_id,run_attempt) DO NOTHING`,
				identity.IntegrationID, identity.WorkflowJobID, identity.RunAttempt, now)
			return struct{}{}, err
		})
		if err != nil {
			return ErrUnavailable
		}
		return nil
	}
}

func NewAWSActionsRunnerRegistrationTokenMinter(config aws.Config, pool transaction.Beginner, policy transaction.Policy, httpClient *http.Client) (ActionsRunnerRegistrationTokenMinter, error) {
	if strings.TrimSpace(config.Region) == "" || pool == nil {
		return nil, ErrUnavailable
	}
	authorizer := githubregistry.CredentialAuthorizer{Pool: pool, Policy: policy}
	return func(ctx context.Context, identity ActionsRunnerJobIdentity) (githubapp.RunnerRegistrationToken, githubapp.RepositoryAccess, error) {
		client, err := githubapp.NewTenantAppTokenClientFromConfig(identity.Credential, config, authorizer, httpClient)
		if err != nil {
			return githubapp.RunnerRegistrationToken{}, githubapp.RepositoryAccess{}, ErrUnavailable
		}
		token, repository, err := client.ActionsRunnerRegistrationToken(ctx, identity.TenantID, identity.Installation, identity.Repository)
		if err != nil {
			return githubapp.RunnerRegistrationToken{}, githubapp.RepositoryAccess{}, ErrUnavailable
		}
		return token, repository, nil
	}, nil
}

func (s *ActionsRunnerRegistrationService) Issue(ctx context.Context, caller string, request ActionsRunnerRegistrationRequest) (ActionsRunnerRegistrationGrant, error) {
	if s == nil || ctx == nil || caller != controlPlaneURI || !validActionsRunnerRegistrationRequest(request) {
		return ActionsRunnerRegistrationGrant{}, ErrActionsRunnerDenied
	}
	identity, err := s.start(ctx, request)
	if err != nil || !validActionsRunnerIdentity(identity, request, s.clock()) {
		return ActionsRunnerRegistrationGrant{}, ErrActionsRunnerDenied
	}
	token, repository, err := s.mint(ctx, identity)
	if err != nil || token.Value() == "" || !token.ExpiresAt().After(s.clock().Add(2*time.Minute)) || token.ExpiresAt().After(s.clock().Add(65*time.Minute)) ||
		repository.Owner == "" || repository.Name == "" {
		_ = s.fail(ctx, identity)
		return ActionsRunnerRegistrationGrant{}, ErrActionsRunnerDenied
	}
	return ActionsRunnerRegistrationGrant{Token: token, Repository: repository}, nil
}

func validActionsRunnerRegistrationRequest(request ActionsRunnerRegistrationRequest) bool {
	if len(request.IntegrationID) < 3 || len(request.IntegrationID) > 128 || request.WorkflowJobID <= 0 || request.RunAttempt < 0 || len(request.ClaimID) < 8 || len(request.ClaimID) > 128 {
		return false
	}
	for _, char := range request.IntegrationID + request.ClaimID {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

func validActionsRunnerIdentity(identity ActionsRunnerJobIdentity, request ActionsRunnerRegistrationRequest, now time.Time) bool {
	return identity.IntegrationID == request.IntegrationID && identity.WorkflowJobID == request.WorkflowJobID && identity.RunAttempt == request.RunAttempt &&
		identity.ClaimID == request.ClaimID && identity.TenantID != "" && identity.Installation > 0 && identity.Repository > 0 && identity.WorkflowRunID > 0 &&
		identity.HeadSHA != "" && identity.HeadBranch != "" && identity.LeaseExpires.After(now) && identity.Credential.TenantID == string(identity.TenantID) &&
		identity.Credential.IntegrationID == identity.IntegrationID && identity.Credential.GitHubAppID > 0 && identity.Credential.ConfigRevision > 0 &&
		identity.Credential.CredentialGeneration > 0 && identity.Credential.SecretARN != "" && identity.Credential.ReaderRoleARN != "" && identity.Credential.SecretVersionID != ""
}
