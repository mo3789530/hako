package githubbroker

import (
	"context"
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

var ErrActionsRunnerCleanupDenied = errors.New("GitHub Actions runner cleanup denied")

type ActionsRunnerCleanupRequest struct {
	IntegrationID string `json:"integration_id"`
	WorkflowJobID int64  `json:"workflow_job_id"`
	RunAttempt    int64  `json:"run_attempt"`
	ClaimID       string `json:"claim_id"`
}

type ActionsRunnerCleanupIdentity struct {
	ResourcePlaneID string
	IntegrationID   string
	TenantID        domain.TenantID
	Installation    int64
	Repository      int64
	WorkflowJobID   int64
	RunAttempt      int64
	RunnerID        int64
	ClaimID         string
	LeaseExpires    time.Time
	Credential      githubapp.TenantPrivateKeyAccess
}

type ActionsRunnerCleanupResolver func(context.Context, string, ActionsRunnerCleanupRequest) (ActionsRunnerCleanupIdentity, error)
type ActionsRunnerDeregistrator func(context.Context, ActionsRunnerCleanupIdentity) error

type ActionsRunnerCleanupService struct {
	resolve    ActionsRunnerCleanupResolver
	deregister ActionsRunnerDeregistrator
	clock      func() time.Time
}

func NewActionsRunnerCleanupService(resolve ActionsRunnerCleanupResolver, deregister ActionsRunnerDeregistrator) (*ActionsRunnerCleanupService, error) {
	if resolve == nil || deregister == nil {
		return nil, ErrUnavailable
	}
	return &ActionsRunnerCleanupService{resolve: resolve, deregister: deregister, clock: time.Now}, nil
}

func (s *ActionsRunnerCleanupService) Deregister(ctx context.Context, caller string, request ActionsRunnerCleanupRequest) error {
	planeID, err := resourcePlaneIDFromCaller(caller)
	if s == nil || ctx == nil || err != nil || !validActionsRunnerRegistrationRequest(ActionsRunnerRegistrationRequest{
		IntegrationID: request.IntegrationID, WorkflowJobID: request.WorkflowJobID, RunAttempt: request.RunAttempt, ClaimID: request.ClaimID}) || request.RunAttempt <= 0 {
		return ErrActionsRunnerCleanupDenied
	}
	identity, err := s.resolve(ctx, planeID, request)
	if err != nil || !validActionsRunnerCleanupIdentity(identity, planeID, request, s.clock()) {
		return ErrActionsRunnerCleanupDenied
	}
	if err := s.deregister(ctx, identity); err != nil {
		return ErrActionsRunnerCleanupDenied
	}
	return nil
}

func validActionsRunnerCleanupIdentity(identity ActionsRunnerCleanupIdentity, planeID string, request ActionsRunnerCleanupRequest, now time.Time) bool {
	return identity.ResourcePlaneID == planeID && identity.IntegrationID == request.IntegrationID &&
		identity.WorkflowJobID == request.WorkflowJobID && identity.RunAttempt == request.RunAttempt && identity.ClaimID == request.ClaimID &&
		identity.TenantID != "" && identity.Installation > 0 && identity.Repository > 0 && identity.RunnerID > 0 && identity.LeaseExpires.After(now) &&
		identity.Credential.TenantID == string(identity.TenantID) && identity.Credential.IntegrationID == identity.IntegrationID &&
		identity.Credential.GitHubAppID > 0 && identity.Credential.ConfigRevision > 0 && identity.Credential.CredentialGeneration > 0 &&
		identity.Credential.SecretARN != "" && identity.Credential.ReaderRoleARN != "" && identity.Credential.SecretVersionID != ""
}

func resourcePlaneIDFromCaller(caller string) (string, error) {
	if len(caller) <= len(resourcePlaneURIPrefix) || caller[:len(resourcePlaneURIPrefix)] != resourcePlaneURIPrefix {
		return "", ErrActionsRunnerCleanupDenied
	}
	planeID := caller[len(resourcePlaneURIPrefix):]
	if _, err := ResourcePlaneCallerID(planeID); err != nil {
		return "", ErrActionsRunnerCleanupDenied
	}
	return planeID, nil
}

// NewPostgresActionsRunnerCleanupResolver authorizes only a live cleanup lease
// whose database-pinned Resource Plane matches the verified mTLS caller. All
// Tenant, Installation, Repository, Runner and Secret references come from the
// durable cleanup record and active Tenant App binding, never the request.
func NewPostgresActionsRunnerCleanupResolver(pool transaction.Beginner, policy transaction.Policy) ActionsRunnerCleanupResolver {
	return func(ctx context.Context, planeID string, request ActionsRunnerCleanupRequest) (ActionsRunnerCleanupIdentity, error) {
		if pool == nil || ctx == nil || !resourcePlaneIDPattern.MatchString(planeID) || request.RunAttempt <= 0 {
			return ActionsRunnerCleanupIdentity{}, ErrActionsRunnerCleanupDenied
		}
		now := time.Now().UTC()
		identity, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (ActionsRunnerCleanupIdentity, error) {
			var result ActionsRunnerCleanupIdentity
			var revision, generation int64
			err := tx.QueryRow(ctx, `SELECT o.resource_plane_id,o.integration_id,o.tenant_id,o.installation_id,o.repository_id,
				o.workflow_job_id,o.run_attempt,o.runner_id,o.claim_id,o.lease_expires_at,i.config_revision,i.credential_generation
				FROM github_actions_runner_cleanup_outbox o
				JOIN tenant_github_integrations i ON i.id=o.integration_id AND i.tenant_id=o.tenant_id
				JOIN github_app_registrations app ON app.id=i.app_registration_id AND app.github_app_id=i.github_app_id
				JOIN github_credential_bindings credential ON credential.tenant_id=i.tenant_id AND credential.id=i.credential_binding_id AND credential.github_app_id=i.github_app_id
				JOIN tenant_github_app_installation_bindings binding ON binding.tenant_id=i.tenant_id AND binding.integration_id=i.id AND binding.installation_id=o.installation_id
				JOIN tenant_github_installations installation ON installation.tenant_id=binding.tenant_id AND installation.installation_id=binding.installation_id
				JOIN tenant_github_repositories repository ON repository.tenant_id=o.tenant_id AND repository.installation_id=o.installation_id AND repository.github_repository_id=o.repository_id
				WHERE o.integration_id=$1 AND o.workflow_job_id=$2 AND o.run_attempt=$3 AND o.state='claimed' AND o.claim_id=$4
				AND o.resource_plane_id=$5 AND o.runner_id IS NOT NULL AND o.lease_expires_at>$6
				AND i.status='active' AND app.mode='tenant_owned' AND app.owner_tenant_id=i.tenant_id AND credential.status='approved'
				AND installation.status='active' AND installation.suspended_at IS NULL`,
				request.IntegrationID, request.WorkflowJobID, request.RunAttempt, request.ClaimID, planeID, now).
				Scan(&result.ResourcePlaneID, &result.IntegrationID, &result.TenantID, &result.Installation, &result.Repository,
					&result.WorkflowJobID, &result.RunAttempt, &result.RunnerID, &result.ClaimID, &result.LeaseExpires, &revision, &generation)
			if err != nil {
				return ActionsRunnerCleanupIdentity{}, ErrActionsRunnerCleanupDenied
			}
			credential, err := githubregistry.ResolveActiveAppCredential(ctx, tx, result.TenantID, result.IntegrationID, revision, generation)
			if err != nil || credential.GitHubAppID <= 0 || credential.ConfigRevision != revision || credential.CredentialGeneration != generation {
				return ActionsRunnerCleanupIdentity{}, ErrActionsRunnerCleanupDenied
			}
			result.Credential = githubapp.TenantPrivateKeyAccess{TenantID: string(credential.TenantID), IntegrationID: credential.IntegrationID,
				GitHubAppID: credential.GitHubAppID, ConfigRevision: credential.ConfigRevision, CredentialGeneration: credential.CredentialGeneration,
				ReaderRoleARN: credential.PrivateKeyReaderRoleARN, SecretARN: credential.PrivateKeySecretARN, SecretVersionID: credential.PrivateKeyVersionID}
			return result, nil
		})
		if err != nil {
			return ActionsRunnerCleanupIdentity{}, ErrActionsRunnerCleanupDenied
		}
		return identity, nil
	}
}

func NewAWSActionsRunnerDeregistrator(config aws.Config, pool transaction.Beginner, policy transaction.Policy, httpClient *http.Client) (ActionsRunnerDeregistrator, error) {
	if strings.TrimSpace(config.Region) == "" || pool == nil {
		return nil, ErrUnavailable
	}
	baseAuthorizer := githubregistry.CredentialAuthorizer{Pool: pool, Policy: policy}
	return func(ctx context.Context, identity ActionsRunnerCleanupIdentity) error {
		authorizer := actionsRunnerCleanupAuthorizer{base: baseAuthorizer, pool: pool, policy: policy, identity: identity}
		client, err := githubapp.NewTenantAppTokenClientFromConfig(identity.Credential, config, authorizer, httpClient)
		if err != nil {
			return ErrActionsRunnerCleanupDenied
		}
		if err := client.DeregisterActionsRunner(ctx, identity.TenantID, identity.Installation, identity.Repository, identity.RunnerID); err != nil {
			return ErrActionsRunnerCleanupDenied
		}
		return nil
	}, nil
}

// actionsRunnerCleanupAuthorizer repeats the durable claim/lease check during
// each credential authorization callback, not just before reading the Secret.
// This prevents a slow broker exchange from using a deregistration token after
// another worker has recovered the cleanup lease.
type actionsRunnerCleanupAuthorizer struct {
	base     githubregistry.CredentialAuthorizer
	pool     transaction.Beginner
	policy   transaction.Policy
	identity ActionsRunnerCleanupIdentity
}

func (a actionsRunnerCleanupAuthorizer) AuthorizeRepository(ctx context.Context, tenant domain.TenantID, installationID, repositoryID int64) (githubapp.RepositoryAccess, error) {
	access, err := a.base.AuthorizeRepository(ctx, tenant, installationID, repositoryID)
	if err != nil {
		return githubapp.RepositoryAccess{}, err
	}
	if err := a.authorizeLiveCleanupLease(ctx, tenant, installationID, repositoryID); err != nil {
		return githubapp.RepositoryAccess{}, ErrActionsRunnerCleanupDenied
	}
	return access, nil
}

func (a actionsRunnerCleanupAuthorizer) AuthorizeTenantApp(ctx context.Context, app githubapp.TenantAppIdentity) error {
	if app.TenantID != a.identity.TenantID || app.IntegrationID != a.identity.IntegrationID || app.GitHubAppID != a.identity.Credential.GitHubAppID ||
		app.ConfigRevision != a.identity.Credential.ConfigRevision || app.CredentialGeneration != a.identity.Credential.CredentialGeneration {
		return ErrActionsRunnerCleanupDenied
	}
	if err := a.base.AuthorizeTenantApp(ctx, app); err != nil {
		return err
	}
	return a.authorizeLiveCleanupLease(ctx, a.identity.TenantID, a.identity.Installation, a.identity.Repository)
}

func (a actionsRunnerCleanupAuthorizer) authorizeLiveCleanupLease(ctx context.Context, tenant domain.TenantID, installationID, repositoryID int64) error {
	if a.pool == nil || ctx == nil || tenant != a.identity.TenantID || installationID != a.identity.Installation || repositoryID != a.identity.Repository {
		return ErrActionsRunnerCleanupDenied
	}
	now := time.Now().UTC()
	valid, err := transaction.Within(ctx, a.pool, a.policy, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		var found bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM github_actions_runner_cleanup_outbox o
			WHERE o.integration_id=$1 AND o.workflow_job_id=$2 AND o.run_attempt=$3 AND o.state='claimed' AND o.claim_id=$4
			AND o.resource_plane_id=$5 AND o.tenant_id=$6 AND o.installation_id=$7 AND o.repository_id=$8 AND o.runner_id=$9
			AND o.lease_expires_at>$10)`, a.identity.IntegrationID, a.identity.WorkflowJobID, a.identity.RunAttempt, a.identity.ClaimID,
			a.identity.ResourcePlaneID, a.identity.TenantID, a.identity.Installation, a.identity.Repository, a.identity.RunnerID, now).Scan(&found)
		return found, err
	})
	if err != nil || !valid {
		return ErrActionsRunnerCleanupDenied
	}
	return nil
}

var _ githubapp.RepositoryAuthorizer = actionsRunnerCleanupAuthorizer{}
var _ githubapp.TenantAppAuthorizer = actionsRunnerCleanupAuthorizer{}

func validResourcePlaneCleanupCaller(caller string) bool {
	_, err := resourcePlaneIDFromCaller(caller)
	return err == nil
}

func validCleanupRequest(request ActionsRunnerCleanupRequest) bool {
	return validActionsRunnerRegistrationRequest(ActionsRunnerRegistrationRequest{
		IntegrationID: request.IntegrationID, WorkflowJobID: request.WorkflowJobID, RunAttempt: request.RunAttempt, ClaimID: request.ClaimID}) && request.RunAttempt > 0
}
