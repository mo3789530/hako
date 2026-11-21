//go:build integration

package githubbroker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestPostgresActionsRunnerCleanupResolverRequiresPlaneAndLiveClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, statement := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_runner_cleanup','Runner cleanup',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_runner_cleanup','runner-cleanup','',now())`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id) VALUES ('binding_runner_cleanup','tenant_runner_cleanup',712,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:key','oauth','webhook',now(),'arn:aws:iam::123456789012:role/key-reader','0123456789abcdef0123456789abcdef')`,
		`INSERT INTO github_app_registrations (id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_runner_cleanup',712,'tenant_owned','tenant_runner_cleanup','runner-app','client',now())`,
		`INSERT INTO tenant_github_integrations (id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_runner_cleanup','tenant_runner_cleanup','registration_runner_cleanup',712,'binding_runner_cleanup','active',3,7,'runner-cleanup-key','runner-cleanup-hash','usr_runner_cleanup',now(),now())`,
		`INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_runner_cleanup',7121,'acme','active','usr_runner_cleanup',now(),now())`,
		`INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at) VALUES (7121,'tenant_runner_cleanup',now())`,
		`INSERT INTO tenant_github_app_installation_bindings (tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_runner_cleanup','integration_runner_cleanup',7121,now())`,
		`INSERT INTO tenant_github_repositories (tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,actions_runner_enabled,actions_runner_enabled_at) VALUES ('tenant_runner_cleanup',7121,7122,'acme','runner-api','main',now(),false,now())`,
		`INSERT INTO github_actions_runner_jobs (integration_id,delivery_id,tenant_id,installation_id,repository_id,workflow_job_id,workflow_run_id,run_attempt,workflow_name,job_name,head_branch,head_sha,labels,state,created_at,updated_at) VALUES ('integration_runner_cleanup','c3b2c3d4-e5f6-4789-8abc-def012345678','tenant_runner_cleanup',7121,7122,7130,7131,1,'CI','test','main','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','["self-hosted","hako"]','completed',now(),now())`,
		`INSERT INTO github_actions_runner_cleanup_outbox (integration_id,workflow_job_id,run_attempt,tenant_id,installation_id,repository_id,runner_id,resource_plane_id,reason,state,attempt_count,available_at,claim_id,lease_expires_at,created_at,updated_at) VALUES ('integration_runner_cleanup',7130,1,'tenant_runner_cleanup',7121,7122,7140,'rp-tokyo-01','completed','claimed',1,now(),'cleanup-claim-1',now()+interval '1 minute',now(),now())`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	resolver := NewPostgresActionsRunnerCleanupResolver(pool, transaction.DefaultPolicy())
	request := ActionsRunnerCleanupRequest{IntegrationID: "integration_runner_cleanup", WorkflowJobID: 7130, RunAttempt: 1, ClaimID: "cleanup-claim-1"}
	if _, err := resolver(ctx, "rp-us-01", request); err == nil {
		t.Fatal("cleanup resolver accepted a foreign Resource Plane")
	}
	if _, err := resolver(ctx, "rp-tokyo-01", ActionsRunnerCleanupRequest{IntegrationID: request.IntegrationID, WorkflowJobID: request.WorkflowJobID, RunAttempt: 1, ClaimID: "wrong-claim-1"}); err == nil {
		t.Fatal("cleanup resolver accepted a different claim")
	}
	identity, err := resolver(ctx, "rp-tokyo-01", request)
	if err != nil || identity.ResourcePlaneID != "rp-tokyo-01" || identity.TenantID != "tenant_runner_cleanup" || identity.Installation != 7121 || identity.Repository != 7122 || identity.RunnerID != 7140 || identity.Credential.GitHubAppID != 712 || identity.Credential.ConfigRevision != 3 || identity.Credential.CredentialGeneration != 7 || identity.Credential.SecretVersionID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("resolve exact cleanup identity=%+v error=%v", identity, err)
	}
	credentialAuthorizer := githubregistry.CredentialAuthorizer{Pool: pool, Policy: transaction.DefaultPolicy()}
	leaseAuthorizer := actionsRunnerCleanupAuthorizer{base: credentialAuthorizer, pool: pool, policy: transaction.DefaultPolicy(), identity: identity}
	appIdentity := githubapp.TenantAppIdentity{TenantID: identity.TenantID, IntegrationID: identity.IntegrationID,
		GitHubAppID: identity.Credential.GitHubAppID, ConfigRevision: identity.Credential.ConfigRevision,
		CredentialGeneration: identity.Credential.CredentialGeneration}
	if err := leaseAuthorizer.AuthorizeTenantApp(ctx, appIdentity); err != nil {
		t.Fatalf("live cleanup lease did not authorize Tenant App use: %v", err)
	}
	if _, err := leaseAuthorizer.AuthorizeRepository(ctx, identity.TenantID, identity.Installation, identity.Repository); err != nil {
		t.Fatalf("live cleanup lease did not authorize scoped Repository use: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE github_actions_runner_cleanup_outbox SET lease_expires_at=$1 WHERE integration_id=$2 AND workflow_job_id=$3 AND run_attempt=$4`, now.Add(-time.Second), request.IntegrationID, request.WorkflowJobID, request.RunAttempt); err != nil {
		t.Fatal(err)
	}
	if err := leaseAuthorizer.AuthorizeTenantApp(ctx, appIdentity); err == nil {
		t.Fatal("credential authorization ignored an expired cleanup lease")
	}
	if _, err := leaseAuthorizer.AuthorizeRepository(ctx, identity.TenantID, identity.Installation, identity.Repository); err == nil {
		t.Fatal("repository authorization ignored an expired cleanup lease")
	}
	if _, err := resolver(ctx, "rp-tokyo-01", request); err == nil {
		t.Fatal("cleanup resolver accepted an expired lease")
	}
	if _, err := pool.Exec(ctx, `UPDATE github_actions_runner_cleanup_outbox SET runner_id=NULL,lease_expires_at=$1 WHERE integration_id=$2 AND workflow_job_id=$3 AND run_attempt=$4`, now.Add(time.Minute), request.IntegrationID, request.WorkflowJobID, request.RunAttempt); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver(ctx, "rp-tokyo-01", request); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("cleanup resolver accepted missing Runner ID or leaked credential detail")
	}
}
