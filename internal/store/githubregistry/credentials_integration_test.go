//go:build integration

package githubregistry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
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

func TestCredentialAuthorizerRevalidatesTenantAppRevisionAndGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_app_auth','Tenant',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_app_auth','subject','',now())`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id) VALUES ('binding_app_auth','tenant_app_auth',901,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:key','oauth-ref','webhook-ref',now(),'arn:aws:iam::123456789012:role/hako/key-reader','0123456789abcdef0123456789abcdef')`,
		`INSERT INTO github_app_registrations (id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_app_auth',901,'tenant_owned','tenant_app_auth','tenant-app','client',now())`,
		`INSERT INTO tenant_github_integrations (id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_app_auth','tenant_app_auth','registration_app_auth',901,'binding_app_auth','active',5,8,'app-auth-key','app-auth-hash','usr_app_auth',now(),now())`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	authorizer := CredentialAuthorizer{Pool: pool}
	identity := githubapp.TenantAppIdentity{TenantID: "tenant_app_auth", IntegrationID: "integration_app_auth", GitHubAppID: 901, ConfigRevision: 5, CredentialGeneration: 8}
	if err := authorizer.AuthorizeTenantApp(ctx, identity); err != nil {
		t.Fatal("active exact Tenant App identity was denied")
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (AppProofCredential, error) {
		return ResolveActiveAppCredential(ctx, tx, identity.TenantID, identity.IntegrationID, identity.ConfigRevision, identity.CredentialGeneration)
	}); err != nil {
		t.Fatal("active Tenant App credential did not resolve")
	}
	for _, stale := range []githubapp.TenantAppIdentity{
		{TenantID: "tenant_other", IntegrationID: identity.IntegrationID, GitHubAppID: identity.GitHubAppID, ConfigRevision: 5, CredentialGeneration: 8},
		{TenantID: identity.TenantID, IntegrationID: identity.IntegrationID, GitHubAppID: identity.GitHubAppID + 1, ConfigRevision: 5, CredentialGeneration: 8},
		{TenantID: identity.TenantID, IntegrationID: identity.IntegrationID, GitHubAppID: identity.GitHubAppID, ConfigRevision: 4, CredentialGeneration: 8},
		{TenantID: identity.TenantID, IntegrationID: identity.IntegrationID, GitHubAppID: identity.GitHubAppID, ConfigRevision: 5, CredentialGeneration: 7},
	} {
		if err := authorizer.AuthorizeTenantApp(ctx, stale); !errors.Is(err, ErrInstallationNotActive) {
			t.Fatal("cross-Tenant or stale App identity was authorized")
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_integrations SET credential_generation = 9 WHERE id = 'integration_app_auth'`); err != nil {
		t.Fatal(err)
	}
	if err := authorizer.AuthorizeTenantApp(ctx, identity); !errors.Is(err, ErrInstallationNotActive) {
		t.Fatal("old in-memory client remained authorized after credential rotation")
	}
}

func TestWebhookCredentialResolverAndAuthorizerAreIntegrationScoped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_hook_auth','Webhook auth',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_hook_auth','hook-auth-sub','',now())`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,webhook_secret_reader_role_arn,webhook_secret_version_id,previous_webhook_secret_version_id,previous_webhook_secret_valid_until) VALUES ('binding_hook_auth','tenant_hook_auth',911,'approved','private-ref','oauth-ref','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:webhook',now(),'arn:aws:iam::123456789012:role/hako/webhook-reader','11111111111111111111111111111111','22222222222222222222222222222222',now()+interval '5 minutes')`,
		`INSERT INTO github_app_registrations (id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_hook_auth',911,'tenant_owned','tenant_hook_auth','hook-app','client',now())`,
		`INSERT INTO tenant_github_integrations (id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_hook_auth','tenant_hook_auth','registration_hook_auth',911,'binding_hook_auth','active',3,6,'hook-auth-key','hook-auth-hash','usr_hook_auth',now(),now())`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	access, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (AppWebhookCredentialAccess, error) {
		return ResolveActiveWebhookCredentialAccess(ctx, tx, "integration_hook_auth")
	})
	if err != nil || access.TenantID != "tenant_hook_auth" || access.GitHubAppID != 911 || access.CredentialGeneration != 6 || access.ReaderRoleARN != "arn:aws:iam::123456789012:role/hako/webhook-reader" || access.SecretVersionID != "11111111111111111111111111111111" || access.PreviousVersionID != "22222222222222222222222222222222" || access.PreviousValidUntil == nil {
		t.Fatalf("approved Webhook-purpose metadata did not resolve exactly: %+v err=%v", access, err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (AppWebhookCredentialAccess, error) {
		return ResolveActiveWebhookCredentialAccess(ctx, tx, "unknown_integration")
	}); !errors.Is(err, ErrAppBindingDenied) {
		t.Fatal("unknown route resolved an arbitrary credential binding")
	}

	authorizer := CredentialAuthorizer{Pool: pool}
	created := githubwebhook.RepositoryEvent{GitHubEvent: "installation", Action: "created", InstallationID: 9201}
	wrongAppID := int64(912)
	created.GitHubAppID = &wrongAppID
	if err := authorizer.AuthorizeWebhookDelivery(ctx, access.IntegrationID, access.CredentialGeneration, created); !errors.Is(err, ErrInstallationNotActive) {
		t.Fatal("pre-binding installation.created with another App ID was accepted")
	}
	correctAppID := int64(911)
	created.GitHubAppID = &correctAppID
	if err := authorizer.AuthorizeWebhookDelivery(ctx, access.IntegrationID, access.CredentialGeneration, created); err != nil {
		t.Fatal("signed pre-binding installation.created should be accepted only for inboxing")
	}
	repositoryEvent := githubwebhook.RepositoryEvent{GitHubEvent: "installation_repositories", Action: "added", InstallationID: 9201}
	repositoryEvent.GitHubAppID = &correctAppID
	if err := authorizer.AuthorizeWebhookDelivery(ctx, access.IntegrationID, access.CredentialGeneration, repositoryEvent); !errors.Is(err, ErrInstallationNotActive) {
		t.Fatal("unbound Installation repository event was accepted")
	}
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_hook_auth',9201,'acme','active','usr_hook_auth',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at) VALUES (9201,'tenant_hook_auth',$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_github_app_installation_bindings (tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_hook_auth','integration_hook_auth',9201,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if err := authorizer.AuthorizeWebhookDelivery(ctx, access.IntegrationID, access.CredentialGeneration, repositoryEvent); err != nil {
		t.Fatal("active bound Installation was denied")
	}
	if err := authorizer.AuthorizeWebhookDelivery(ctx, access.IntegrationID, access.CredentialGeneration-1, repositoryEvent); !errors.Is(err, ErrInstallationNotActive) {
		t.Fatal("stale integration generation was accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_installations SET suspended_at=$1 WHERE tenant_id='tenant_hook_auth' AND installation_id=9201`, now); err != nil {
		t.Fatal(err)
	}
	if err := authorizer.AuthorizeWebhookDelivery(ctx, access.IntegrationID, access.CredentialGeneration, repositoryEvent); !errors.Is(err, ErrInstallationNotActive) {
		t.Fatal("suspended Installation repository event was accepted")
	}
	suspendEvent := githubwebhook.RepositoryEvent{GitHubEvent: "installation", Action: "suspend", InstallationID: 9201}
	if err := authorizer.AuthorizeWebhookDelivery(ctx, access.IntegrationID, access.CredentialGeneration, suspendEvent); err != nil {
		t.Fatal("signed lifecycle events for a known binding must remain processable")
	}
}
