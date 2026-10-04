//go:build integration

package githubregistry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestCredentialAuthorizerRequiresActiveBoundTenantRepository(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants (id, name, created_at) VALUES ($1, $2, $3)`, []any{"tenant_credentials", "Credentials", now}},
		{`INSERT INTO tenants (id, name, created_at) VALUES ($1, $2, $3)`, []any{"tenant_other", "Other", now}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ($1, $2, '', $3)`, []any{"usr_credentials", "credentials-sub", now}},
		{`INSERT INTO tenant_github_installations (tenant_id, installation_id, account_login, status, requested_by, requested_at, updated_at) VALUES ($1, $2, 'acme', 'active', 'usr_credentials', $3, $3)`, []any{"tenant_credentials", int64(77), now}},
		{`INSERT INTO github_app_installation_bindings (installation_id, tenant_id, bound_at) VALUES ($1, $2, $3)`, []any{int64(77), "tenant_credentials", now}},
		{`INSERT INTO tenant_github_repositories (tenant_id, installation_id, github_repository_id, owner_login, repository_name, default_branch, synchronized_at) VALUES ($1, $2, $3, 'acme', 'api', 'main', $4)`, []any{"tenant_credentials", int64(77), int64(88), now}},
	} {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	authorizer := CredentialAuthorizer{Pool: pool}
	access, err := authorizer.AuthorizeRepository(ctx, "tenant_credentials", 77, 88)
	if err != nil || access.Owner != "acme" || access.Name != "api" {
		t.Fatal("active bound repository was not authorized")
	}
	for _, request := range []struct {
		tenant                   domain.TenantID
		installation, repository int64
	}{
		{"tenant_other", 77, 88}, {"tenant_credentials", 99, 88}, {"tenant_credentials", 77, 99}, {"", 77, 88}, {"tenant_credentials", 0, 88}, {"tenant_credentials", 77, 0},
	} {
		if _, err := authorizer.AuthorizeRepository(ctx, request.tenant, request.installation, request.repository); !errors.Is(err, ErrInstallationNotActive) {
			t.Fatal("unauthorized credential request was not denied")
		}
	}
	for _, state := range []struct{ name, sql string }{
		{"suspended", `UPDATE tenant_github_installations SET suspended_at = now() WHERE installation_id = 77`},
		{"revoked", `UPDATE tenant_github_installations SET suspended_at = NULL, status = 'revoked' WHERE installation_id = 77`},
		{"pending", `UPDATE tenant_github_installations SET status = 'pending' WHERE installation_id = 77`},
		{"unbound", `DELETE FROM github_app_installation_bindings WHERE installation_id = 77`},
	} {
		t.Run(state.name, func(t *testing.T) {
			if state.name == "unbound" {
				if _, err := pool.Exec(ctx, `DELETE FROM tenant_github_repositories WHERE installation_id = 77`); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `UPDATE tenant_github_installations SET status = 'active' WHERE installation_id = 77`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pool.Exec(ctx, state.sql); err != nil {
				t.Fatal(err)
			}
			if _, err := authorizer.AuthorizeRepository(ctx, "tenant_credentials", 77, 88); !errors.Is(err, ErrInstallationNotActive) {
				t.Fatal("inactive or unbound repository was authorized")
			}
		})
	}
	if _, err := (CredentialAuthorizer{}).AuthorizeRepository(ctx, "tenant_credentials", 77, 88); !errors.Is(err, ErrInstallationNotActive) {
		t.Fatal("missing pool was accepted")
	}
}
