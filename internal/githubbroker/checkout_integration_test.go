//go:build integration

package githubbroker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestPostgresCheckoutResolverUsesPersistedTenantRunAndExactActivePin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	statements := []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_broker_a','Broker A',$1),('tenant_broker_b','Broker B',$1)`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_broker_a','broker-a','',$1)`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id)
		 VALUES ('binding_broker_a','tenant_broker_a',731,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/key-AbCdEf','oauth-ref','webhook-ref',$1,'arn:aws:iam::123456789012:role/hako/private-key-reader','0123456789abcdef0123456789abcdef')`,
		`INSERT INTO github_app_registrations (id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at)
		 VALUES ('registration_broker_a',731,'tenant_owned','tenant_broker_a','broker-app','client',$1)`,
		`INSERT INTO tenant_github_integrations (id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at)
		 VALUES ('integration_broker_a','tenant_broker_a','registration_broker_a',731,'binding_broker_a','active',3,7,'broker-key','broker-hash','usr_broker_a',$1,$1)`,
		`INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at)
		 VALUES ('tenant_broker_a',7311,'broker-owner','active','usr_broker_a',$1,$1)`,
		`INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at) VALUES (7311,'tenant_broker_a',$1)`,
		`INSERT INTO tenant_github_app_installation_bindings (tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_broker_a','integration_broker_a',7311,$1)`,
		`INSERT INTO tenant_github_repositories (tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at)
		 VALUES ('tenant_broker_a',7311,7312,'broker-owner','broker-repo','main',$1)`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement, now); err != nil {
			t.Fatal(err)
		}
	}
	type createResult struct {
		run     domain.WorkloadRun
		created bool
	}
	createdRun, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (createResult, error) {
		returnRun, created, err := workloads.CreateOrGet(ctx, tx, domain.WorkloadRun{ID: "run_broker_a", TenantID: "tenant_broker_a", Kind: domain.WorkloadJob,
			RequestedBy: "usr_broker_a", GitHubInstallation: 7311, GitHubRepository: 7312, CommitSHA: strings.Repeat("a", 40),
			RuntimeClass: "standard", TrustLevel: domain.WorkloadTrusted, IdempotencyKey: "broker-a-run", CreatedAt: now,
			TimeoutAt: now.Add(5 * time.Minute)})
		return createResult{run: returnRun, created: created}, err
	})
	run, created := createdRun.run, createdRun.created
	if err != nil || run.GitHubIntegrationID != "integration_broker_a" || run.GitHubConfigRevision != 3 || run.GitHubCredentialGeneration != 7 || !created {
		t.Fatalf("create pinned Run = %+v created=%v err=%v", run, created, err)
	}
	checksResolve := NewPostgresChecksRunResolver(pool, transaction.DefaultPolicy())
	checksIdentity, err := checksResolve(ctx, run.TenantID, run.ID)
	if err != nil || checksIdentity.Shared || checksIdentity.Run.ID != run.ID || checksIdentity.Access.IntegrationID != run.GitHubIntegrationID ||
		checksIdentity.Repository != (githubapp.RepositoryAccess{Owner: "broker-owner", Name: "broker-repo"}) || checksIdentity.DesiredCheckRevision != 1 {
		t.Fatalf("resolve Tenant-owned GitHub Check identity failed (identity=%+v err=%v)", checksIdentity, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id,name,created_at) VALUES ('tenant_broker_shared','Broker Shared',$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_broker_shared','broker-shared','',$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at)
		VALUES ('tenant_broker_shared',7321,'shared-owner','active','usr_broker_shared',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at) VALUES (7321,'tenant_broker_shared',$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_github_repositories (tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at)
		VALUES ('tenant_broker_shared',7321,7322,'shared-owner','shared-repo','main',$1)`, now); err != nil {
		t.Fatal(err)
	}
	sharedRun, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		created, _, err := workloads.CreateOrGet(ctx, tx, domain.WorkloadRun{ID: "run_broker_shared", TenantID: "tenant_broker_shared", Kind: domain.WorkloadJob,
			RequestedBy: "usr_broker_shared", GitHubInstallation: 7321, GitHubRepository: 7322, CommitSHA: strings.Repeat("b", 40),
			RuntimeClass: "standard", TrustLevel: domain.WorkloadTrusted, IdempotencyKey: "broker-shared-run", CreatedAt: now,
			TimeoutAt: now.Add(5 * time.Minute)})
		return created, err
	})
	if err != nil || sharedRun.GitHubIntegrationID != "" || sharedRun.GitHubConfigRevision != 0 || sharedRun.GitHubCredentialGeneration != 0 {
		t.Fatalf("create legacy shared Run failed: run=%+v err=%v", sharedRun, err)
	}
	sharedChecksIdentity, err := checksResolve(ctx, sharedRun.TenantID, sharedRun.ID)
	if err != nil || !sharedChecksIdentity.Shared || sharedChecksIdentity.Access != (githubapp.TenantPrivateKeyAccess{}) ||
		sharedChecksIdentity.Repository != (githubapp.RepositoryAccess{Owner: "shared-owner", Name: "shared-repo"}) {
		t.Fatalf("resolve legacy shared GitHub Check identity failed (identity=%+v err=%v)", sharedChecksIdentity, err)
	}
	resolve := NewPostgresCheckoutRunResolver(pool, transaction.DefaultPolicy())
	resolvedRun, access, err := resolve(ctx, run.TenantID, run.ID)
	if err != nil || resolvedRun.ID != run.ID || access.TenantID != string(run.TenantID) || access.IntegrationID != run.GitHubIntegrationID ||
		access.GitHubAppID != 731 || access.ConfigRevision != 3 || access.CredentialGeneration != 7 || access.SecretVersionID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("resolve persisted Run/pin failed (access=%v err=%v)", access, err)
	}
	if _, _, err := resolve(ctx, "tenant_broker_b", run.ID); !errors.Is(err, ErrCheckoutDenied) {
		t.Fatal("cross-Tenant Run credential resolution was allowed")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM tenant_github_app_installation_bindings WHERE tenant_id='tenant_broker_a' AND integration_id='integration_broker_a' AND installation_id=7311`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolve(ctx, run.TenantID, run.ID); !errors.Is(err, ErrCheckoutDenied) {
		t.Fatal("Run resolved after its Installation was unbound from the pinned App")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_github_app_installation_bindings (tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_broker_a','integration_broker_a',7311,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_integrations SET credential_generation = 8 WHERE id = 'integration_broker_a'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolve(ctx, run.TenantID, run.ID); !errors.Is(err, ErrCheckoutDenied) {
		t.Fatal("stale Run generation resolved after rotation")
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_integrations SET credential_generation = 7, status = 'disabled' WHERE id = 'integration_broker_a'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolve(ctx, run.TenantID, run.ID); !errors.Is(err, ErrCheckoutDenied) {
		t.Fatal("disabled Tenant App resolved for checkout")
	}
}
