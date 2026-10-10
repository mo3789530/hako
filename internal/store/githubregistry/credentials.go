package githubregistry

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/transaction"
)

// CredentialAuthorizer belongs exclusively in the trusted Control Plane
// credential service. Do not construct it inside a Resource Plane executor.
type CredentialAuthorizer struct {
	Pool   transaction.Beginner
	Policy transaction.Policy
}

func (a CredentialAuthorizer) AuthorizeRepository(ctx context.Context, tenant domain.TenantID, installationID, repositoryID int64) (githubapp.RepositoryAccess, error) {
	if a.Pool == nil || tenant == "" || installationID <= 0 || repositoryID <= 0 {
		return githubapp.RepositoryAccess{}, ErrInstallationNotActive
	}
	access, err := transaction.Within(ctx, a.Pool, a.Policy, func(ctx context.Context, tx pgx.Tx) (githubapp.RepositoryAccess, error) {
		var result githubapp.RepositoryAccess
		err := tx.QueryRow(ctx, `SELECT r.owner_login, r.repository_name
			FROM tenant_github_repositories r
			JOIN github_app_installation_bindings b ON b.tenant_id = r.tenant_id AND b.installation_id = r.installation_id
			JOIN tenant_github_installations i ON i.tenant_id = b.tenant_id AND i.installation_id = b.installation_id
			WHERE r.tenant_id = $1 AND r.installation_id = $2 AND r.github_repository_id = $3
			AND i.status = 'active' AND i.suspended_at IS NULL`, tenant, installationID, repositoryID).Scan(&result.Owner, &result.Name)
		return result, err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return githubapp.RepositoryAccess{}, ErrInstallationNotActive
		}
		// Do not leak SQL arguments or downstream transport errors to callers.
		return githubapp.RepositoryAccess{}, githubapp.ErrRepositoryAuthorizationUnavailable
	}
	return access, nil
}

// AuthorizeTenantApp checks the integration's live identity/generation before
// any GitHub request or cached-token return. A stale in-memory client therefore
// cannot use the new generation's credential material.
func (a CredentialAuthorizer) AuthorizeTenantApp(ctx context.Context, identity githubapp.TenantAppIdentity) error {
	if a.Pool == nil || identity.TenantID == "" || !validOpaqueMetadata(identity.IntegrationID, 128) || identity.GitHubAppID <= 0 || identity.ConfigRevision < 1 || identity.CredentialGeneration < 1 {
		return ErrInstallationNotActive
	}
	active, err := transaction.Within(ctx, a.Pool, a.Policy, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		var active bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM tenant_github_integrations i
			JOIN github_app_registrations r ON r.id = i.app_registration_id AND r.github_app_id = i.github_app_id
			JOIN github_credential_bindings b ON b.tenant_id = i.tenant_id AND b.id = i.credential_binding_id AND b.github_app_id = i.github_app_id
			WHERE i.tenant_id = $1 AND i.id = $2 AND i.github_app_id = $3
			AND i.status = 'active' AND i.config_revision = $4 AND i.credential_generation = $5
			AND r.mode = 'tenant_owned' AND r.owner_tenant_id = i.tenant_id AND b.status = 'approved'
		)`, identity.TenantID, identity.IntegrationID, identity.GitHubAppID, identity.ConfigRevision, identity.CredentialGeneration).Scan(&active)
		return active, err
	})
	if err != nil {
		return githubapp.ErrRepositoryAuthorizationUnavailable
	}
	if !active {
		return ErrInstallationNotActive
	}
	return nil
}

var _ githubapp.RepositoryAuthorizer = CredentialAuthorizer{}
var _ githubapp.TenantAppAuthorizer = CredentialAuthorizer{}
