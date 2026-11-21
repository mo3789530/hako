package githubbroker

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var ErrActionsRunnerChecksDenied = errors.New("GitHub Actions Runner Check credential unavailable")
var actionsRunnerCheckCommitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

type ActionsRunnerChecksRequest struct {
	IntegrationID string `json:"integration_id"`
	WorkflowJobID int64  `json:"workflow_job_id"`
	RunAttempt    int64  `json:"run_attempt"`
}

type ActionsRunnerCheckIdentity struct {
	IntegrationID string
	TenantID      domain.TenantID
	Installation  int64
	Repository    int64
	WorkflowJobID int64
	WorkflowRunID int64
	RunAttempt    int64
	CommitSHA     string
	Revision      int64
	Credential    githubapp.TenantPrivateKeyAccess
	RepositoryURL githubapp.RepositoryAccess
}

type ActionsRunnerChecksResolver func(context.Context, ActionsRunnerChecksRequest) (ActionsRunnerCheckIdentity, error)
type ActionsRunnerChecksTokenMinter func(context.Context, ActionsRunnerCheckIdentity) (githubapp.CheckToken, githubapp.RepositoryAccess, error)

type ActionsRunnerChecksGrant struct {
	token      githubapp.CheckToken
	repository githubapp.RepositoryAccess
}

func (ActionsRunnerChecksGrant) String() string {
	return "GitHub Actions Runner Checks grant [REDACTED]"
}
func (ActionsRunnerChecksGrant) GoString() string {
	return "GitHub Actions Runner Checks grant [REDACTED]"
}
func (g ActionsRunnerChecksGrant) Token() string                          { return g.token.Value() }
func (g ActionsRunnerChecksGrant) ExpiresAt() time.Time                   { return g.token.ExpiresAt() }
func (g ActionsRunnerChecksGrant) Repository() githubapp.RepositoryAccess { return g.repository }

type ActionsRunnerChecksService struct {
	resolve ActionsRunnerChecksResolver
	mint    ActionsRunnerChecksTokenMinter
	now     func() time.Time
}

func NewActionsRunnerChecksService(resolve ActionsRunnerChecksResolver, mint ActionsRunnerChecksTokenMinter) (*ActionsRunnerChecksService, error) {
	if resolve == nil || mint == nil {
		return nil, ErrUnavailable
	}
	return &ActionsRunnerChecksService{resolve: resolve, mint: mint, now: time.Now}, nil
}

func NewPostgresActionsRunnerChecksResolver(pool transaction.Beginner, policy transaction.Policy) ActionsRunnerChecksResolver {
	return func(ctx context.Context, request ActionsRunnerChecksRequest) (ActionsRunnerCheckIdentity, error) {
		if pool == nil || ctx == nil || !validActionsRunnerRegistrationRequest(ActionsRunnerRegistrationRequest{IntegrationID: request.IntegrationID,
			WorkflowJobID: request.WorkflowJobID, RunAttempt: request.RunAttempt, ClaimID: "checks-request"}) || request.RunAttempt <= 0 {
			return ActionsRunnerCheckIdentity{}, ErrActionsRunnerChecksDenied
		}
		identity, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (ActionsRunnerCheckIdentity, error) {
			var result ActionsRunnerCheckIdentity
			var revision, generation int64
			err := tx.QueryRow(ctx, `SELECT c.integration_id,c.tenant_id,c.installation_id,c.repository_id,c.workflow_job_id,c.workflow_run_id,
				c.run_attempt,c.commit_sha,c.revision,r.owner_login,r.repository_name,i.config_revision,i.credential_generation
				FROM github_actions_runner_checks c
				JOIN tenant_github_integrations i ON i.id=c.integration_id AND i.tenant_id=c.tenant_id
				JOIN tenant_github_app_installation_bindings b ON b.tenant_id=i.tenant_id AND b.integration_id=i.id AND b.installation_id=c.installation_id
				JOIN tenant_github_installations x ON x.tenant_id=b.tenant_id AND x.installation_id=b.installation_id
				JOIN tenant_github_repositories r ON r.tenant_id=c.tenant_id AND r.installation_id=c.installation_id AND r.github_repository_id=c.repository_id
				JOIN github_actions_runner_jobs j ON j.integration_id=c.integration_id AND j.workflow_job_id=c.workflow_job_id AND j.run_attempt=c.run_attempt
				WHERE c.integration_id=$1 AND c.workflow_job_id=$2 AND c.run_attempt=$3 AND c.status IN ('queued','in_progress','completed')
				AND i.status='active' AND x.status='active' AND x.suspended_at IS NULL AND r.actions_runner_enabled=TRUE`,
				request.IntegrationID, request.WorkflowJobID, request.RunAttempt).
				Scan(&result.IntegrationID, &result.TenantID, &result.Installation, &result.Repository, &result.WorkflowJobID, &result.WorkflowRunID,
					&result.RunAttempt, &result.CommitSHA, &result.Revision, &result.RepositoryURL.Owner, &result.RepositoryURL.Name, &revision, &generation)
			if err != nil || result.TenantID == "" || result.Installation <= 0 || result.Repository <= 0 || result.WorkflowRunID <= 0 ||
				!actionsRunnerCheckCommitPattern.MatchString(result.CommitSHA) || !validLogin(result.RepositoryURL.Owner) || !validCredentialRepository(result.RepositoryURL.Name) || revision <= 0 || generation <= 0 {
				return ActionsRunnerCheckIdentity{}, ErrActionsRunnerChecksDenied
			}
			credential, err := githubregistry.ResolveActiveAppCredential(ctx, tx, result.TenantID, result.IntegrationID, revision, generation)
			if err != nil || credential.TenantID != result.TenantID || credential.IntegrationID != result.IntegrationID || credential.GitHubAppID <= 0 ||
				credential.ConfigRevision != revision || credential.CredentialGeneration != generation {
				return ActionsRunnerCheckIdentity{}, ErrActionsRunnerChecksDenied
			}
			result.Credential = githubapp.TenantPrivateKeyAccess{TenantID: string(credential.TenantID), IntegrationID: credential.IntegrationID,
				GitHubAppID: credential.GitHubAppID, ConfigRevision: credential.ConfigRevision, CredentialGeneration: credential.CredentialGeneration,
				ReaderRoleARN: credential.PrivateKeyReaderRoleARN, SecretARN: credential.PrivateKeySecretARN, SecretVersionID: credential.PrivateKeyVersionID}
			return result, nil
		})
		if err != nil {
			return ActionsRunnerCheckIdentity{}, ErrActionsRunnerChecksDenied
		}
		return identity, nil
	}
}

func NewAWSActionsRunnerChecksTokenMinter(config aws.Config, pool transaction.Beginner, policy transaction.Policy, httpClient *http.Client) (ActionsRunnerChecksTokenMinter, error) {
	if strings.TrimSpace(config.Region) == "" || pool == nil {
		return nil, ErrUnavailable
	}
	authorizer := githubregistry.CredentialAuthorizer{Pool: pool, Policy: policy}
	return func(ctx context.Context, identity ActionsRunnerCheckIdentity) (githubapp.CheckToken, githubapp.RepositoryAccess, error) {
		client, err := githubapp.NewTenantAppTokenClientFromConfig(identity.Credential, config, authorizer, httpClient)
		if err != nil {
			return nil, githubapp.RepositoryAccess{}, ErrActionsRunnerChecksDenied
		}
		token, err := client.ChecksToken(ctx, identity.TenantID, identity.Installation, identity.Repository)
		if err != nil {
			return nil, githubapp.RepositoryAccess{}, ErrActionsRunnerChecksDenied
		}
		repository, err := authorizer.AuthorizeRepository(ctx, identity.TenantID, identity.Installation, identity.Repository)
		if err != nil || repository != identity.RepositoryURL {
			return nil, githubapp.RepositoryAccess{}, ErrActionsRunnerChecksDenied
		}
		return token, repository, nil
	}, nil
}

func (s *ActionsRunnerChecksService) Issue(ctx context.Context, caller string, request ActionsRunnerChecksRequest) (ActionsRunnerChecksGrant, error) {
	if s == nil || ctx == nil || caller != controlPlaneURI || request.RunAttempt <= 0 {
		return ActionsRunnerChecksGrant{}, ErrActionsRunnerChecksDenied
	}
	identity, err := s.resolve(ctx, request)
	if err != nil || !sameActionsRunnerCheckRequest(identity, request) {
		return ActionsRunnerChecksGrant{}, ErrActionsRunnerChecksDenied
	}
	token, repository, err := s.mint(ctx, identity)
	if err != nil || token == nil || repository != identity.RepositoryURL || !validBrokerToken(token.Value(), token.ExpiresAt(), s.now()) ||
		token.ExpiresAt().After(s.now().Add(65*time.Minute)) {
		return ActionsRunnerChecksGrant{}, ErrActionsRunnerChecksDenied
	}
	current, err := s.resolve(ctx, request)
	if err != nil || !sameActionsRunnerChecksIdentity(identity, current) {
		return ActionsRunnerChecksGrant{}, ErrActionsRunnerChecksDenied
	}
	return ActionsRunnerChecksGrant{token: token, repository: repository}, nil
}

func sameActionsRunnerCheckRequest(identity ActionsRunnerCheckIdentity, request ActionsRunnerChecksRequest) bool {
	return identity.IntegrationID == request.IntegrationID && identity.WorkflowJobID == request.WorkflowJobID && identity.RunAttempt == request.RunAttempt &&
		identity.TenantID != "" && identity.Installation > 0 && identity.Repository > 0 && identity.Revision > 0 && identity.RepositoryURL.Owner != "" && identity.RepositoryURL.Name != ""
}

func sameActionsRunnerChecksIdentity(left, right ActionsRunnerCheckIdentity) bool {
	return left.IntegrationID == right.IntegrationID && left.TenantID == right.TenantID && left.Installation == right.Installation && left.Repository == right.Repository &&
		left.WorkflowJobID == right.WorkflowJobID && left.WorkflowRunID == right.WorkflowRunID && left.RunAttempt == right.RunAttempt && left.CommitSHA == right.CommitSHA &&
		left.Revision == right.Revision && left.Credential == right.Credential && left.RepositoryURL == right.RepositoryURL
}
