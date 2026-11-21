//go:build integration

package githubbroker

import (
	"context"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestPostgresEnvironmentApprovalResolverPinsTenantCatalogAndActiveApp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_env_approval','Environment approval',now())`,
		`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_env_approval','env-approval','',now())`,
		`INSERT INTO github_credential_bindings(id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id)
		 VALUES ('binding_env_approval','tenant_env_approval',901,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/app-key-AbCdEf','oauth-ref','webhook-ref',now(),'arn:aws:iam::123456789012:role/hako/private-key-reader','0123456789abcdef0123456789abcdef')`,
		`INSERT INTO github_app_registrations(id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at)
		 VALUES ('registration_env_approval',901,'tenant_owned','tenant_env_approval','env-app','client',now())`,
		`INSERT INTO tenant_github_integrations(id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at)
		 VALUES ('integration_env_approval','tenant_env_approval','registration_env_approval',901,'binding_env_approval','active',4,8,'env-approval-key','env-approval-hash','usr_env_approval',now(),now())`,
		`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at)
		 VALUES ('tenant_env_approval',902,'acme','active','usr_env_approval',now(),now())`,
		`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (902,'tenant_env_approval',now())`,
		`INSERT INTO tenant_github_app_installation_bindings(tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_env_approval','integration_env_approval',902,now())`,
		`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at)
		 VALUES ('tenant_env_approval',902,903,'acme','payments','main',now())`,
		`INSERT INTO tenant_software_catalog_services(tenant_id,service_id,owner_team,installation_id,github_repository_id,runtime,language,dependencies,created_by,updated_by,created_at,updated_at,oidc_deploy_policy)
		 VALUES ('tenant_env_approval','payments-api','payments',902,903,'lambda-microvm','go','[]','usr_env_approval','usr_env_approval',now(),now(),
		 '{"enabled":true,"audience":"sts.amazonaws.com","allowed_refs":["refs/heads/main"],"environments":["production"],"allowed_workflow_refs":["acme/payments/.github/workflows/deploy.yml@refs/heads/main"],"require_human_approval":true}')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	resolver := NewPostgresEnvironmentApprovalResolver(pool, transaction.DefaultPolicy())
	pin, err := resolver(ctx, "tenant_env_approval", "payments-api", 902, 903, "production")
	if err != nil || pin.IntegrationID != "integration_env_approval" || pin.App.GitHubAppID != 901 || pin.App.ConfigRevision != 4 ||
		pin.App.CredentialGeneration != 8 || pin.App.SecretVersionID != "0123456789abcdef0123456789abcdef" || pin.Repository.Owner != "acme" || pin.Repository.Name != "payments" {
		t.Fatalf("resolved Environment approval pin=%+v error=%v", pin, err)
	}
	if _, err := resolver(ctx, "tenant_env_approval", "payments-api", 902, 903, "staging"); err == nil {
		t.Fatal("environment outside the Tenant policy was accepted")
	}
	if _, err := resolver(ctx, "tenant_env_approval", "foreign-service", 902, 903, "production"); err == nil {
		t.Fatal("unregistered service was accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_integrations SET credential_generation=credential_generation+1 WHERE id='integration_env_approval'`); err != nil {
		t.Fatal(err)
	}
	updated, err := resolver(ctx, "tenant_env_approval", "payments-api", 902, 903, "production")
	if err != nil || updated.App.CredentialGeneration != 9 {
		t.Fatalf("credential rotation was not reflected: pin=%+v error=%v", updated, err)
	}
}
