//go:build integration

package modelcredentialbroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/modelcredentials"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

type fakeReader struct {
	reads  int
	secret []byte
	lastID string
	change func(context.Context)
}

func (f *fakeReader) ReadSecret(ctx context.Context, access modelcredentials.SecretAccess) ([]byte, error) {
	f.reads++
	f.lastID = access.CredentialID
	value := append([]byte(nil), f.secret...)
	if f.change != nil {
		f.change(ctx)
	}
	return value, nil
}

func TestModelCredentialResolverRequiresLiveProvenanceAndRechecksRotation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_model_run','Model run',now())`,
		`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_model_run','model-run','',now())`,
		`INSERT INTO github_credential_bindings(id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id)
		 VALUES ('binding_github_model_run','tenant_model_run',812,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:key-AbCdEf','oauth-ref','webhook-ref',now(),
		 'arn:aws:iam::123456789012:role/hako/github-reader','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa')`,
		`INSERT INTO github_app_registrations(id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at)
		 VALUES ('registration_model_run',812,'tenant_owned','tenant_model_run','model-app','client',now())`,
		`INSERT INTO tenant_github_integrations(id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at)
		 VALUES ('integration_model_run','tenant_model_run','registration_model_run',812,'binding_github_model_run','active',1,1,'model-run-key','model-run-hash','usr_model_run',now(),now())`,
		`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at)
		 VALUES ('tenant_model_run',8121,'acme','active','usr_model_run',now(),now())`,
		`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (8121,'tenant_model_run',now())`,
		`INSERT INTO tenant_github_app_installation_bindings(tenant_id,integration_id,installation_id,bound_at)
		 VALUES ('tenant_model_run','integration_model_run',8121,now())`,
		`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,actions_runner_enabled,actions_runner_enabled_at,actions_runner_workflow_policy)
		 VALUES ('tenant_model_run',8121,8122,'acme','model-api','main',now(),true,now(),
		 '{"workflow_path":".github/workflows/claude.yml","allowed_refs":["refs/heads/main"],"allowed_events":["push"],"required_labels":["hako"],"allowed_labels":["self-hosted","hako"]}'::jsonb)`,
		`INSERT INTO github_webhook_deliveries(integration_id,delivery_id,event_type,action,payload_json,payload_sha256,received_at,processing_status,event_schema_version,normalized_event_json,installation_id,credential_generation,payload_expires_at)
		 VALUES ('integration_model_run','a3b2c3d4-e5f6-4789-8abc-def012345678','workflow_job','queued','{}','hash',now(),'processed',1,'{}',8121,1,now()+interval '7 days')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	var policyJSON []byte
	if err := pool.QueryRow(ctx, `SELECT actions_runner_workflow_policy FROM tenant_github_repositories
		WHERE tenant_id='tenant_model_run' AND installation_id=8121 AND github_repository_id=8122`).Scan(&policyJSON); err != nil {
		t.Fatal(err)
	}
	policyHash := sha256.Sum256(policyJSON)
	if _, err := pool.Exec(ctx, `INSERT INTO github_actions_runner_jobs(integration_id,delivery_id,tenant_id,installation_id,repository_id,workflow_job_id,workflow_run_id,run_attempt,
		workflow_name,job_name,head_branch,head_sha,labels,state,created_at,updated_at,claim_id,lease_expires_at,claim_attempts,resource_plane_id,lambda_microvm_id,provenance_verified_at,provenance_policy_sha256)
		VALUES ('integration_model_run','a3b2c3d4-e5f6-4789-8abc-def012345678','tenant_model_run',8121,8122,8130,8131,1,'Claude','review','main',
		repeat('0',40),'["self-hosted","hako"]','starting',now(),now(),'claim-model-run',now()+interval '1 minute',1,'rp-tokyo-01','lambda-microvm-model-run',now(),$1)`,
		hex.EncodeToString(policyHash[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_model_credential_bindings(id,tenant_id,provider,secret_arn,reader_role_arn,status,created_at,updated_at)
		VALUES ('binding_model_run','tenant_model_run','anthropic','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/model/run-AbCdEf',
		'arn:aws:iam::123456789012:role/hako/tenant-model-credential-reader/tenant-tenant_model_run','approved',now(),now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_model_credentials(id,tenant_id,provider,binding_id,secret_version_id,status,credential_generation,created_by,updated_by,created_at,updated_at)
		VALUES ('mcred_modelrun01','tenant_model_run','anthropic','binding_model_run','11111111111111111111111111111111','active',1,'usr_model_run','usr_model_run',now(),now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_model_credential_bindings(id,tenant_id,provider,purpose,secret_arn,reader_role_arn,status,created_at,updated_at)
		VALUES ('binding_model_agent','tenant_model_run','anthropic','native_agent','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/model/agent-AbCdEf',
		'arn:aws:iam::123456789012:role/hako/tenant-model-credential-reader/tenant-tenant_model_run','approved',now(),now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_model_credentials(id,tenant_id,provider,purpose,binding_id,secret_version_id,status,credential_generation,created_by,updated_by,created_at,updated_at)
		VALUES ('mcred_modelagent1','tenant_model_run','anthropic','native_agent','binding_model_agent','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','active',1,'usr_model_run','usr_model_run',now(),now())`); err != nil {
		t.Fatal(err)
	}
	reader := &fakeReader{secret: []byte("sk-ant-model-test")}
	resolver, err := New(pool, transaction.DefaultPolicy(), reader)
	if err != nil {
		t.Fatal(err)
	}
	resolver.now = func() time.Time { return time.Now().UTC() }
	identity := modelcredentials.RunnerClaimIdentity{IntegrationID: "integration_model_run", WorkflowJobID: 8130, RunAttempt: 1,
		ClaimID: "claim-model-run", ResourcePlaneID: "rp-tokyo-01", LambdaMicroVMID: "lambda-microvm-model-run"}
	wrongPlane := identity
	wrongPlane.ResourcePlaneID = "rp-us-01"
	if err := resolver.WithCredential(ctx, wrongPlane, func(context.Context, []byte) error { t.Fatal("wrong Plane got model credential"); return nil }); err == nil || reader.reads != 0 {
		t.Fatalf("wrong Plane resolution error=%v reads=%d", err, reader.reads)
	}
	reader.change = func(ctx context.Context) {
		if _, err := pool.Exec(ctx, `UPDATE tenant_model_credentials SET secret_version_id='22222222222222222222222222222222',credential_generation=2,updated_at=now()
			WHERE tenant_id='tenant_model_run' AND provider='anthropic' AND purpose='claude_action'`); err != nil {
			t.Errorf("rotate during Secret retrieval: %v", err)
		}
	}
	var consumed []byte
	if err := resolver.WithCredential(ctx, identity, func(_ context.Context, secret []byte) error { consumed = secret; return nil }); err == nil || consumed != nil || reader.reads != 1 {
		t.Fatalf("stale credential was delivered after rotation: err=%v consumed=%q reads=%d", err, consumed, reader.reads)
	}
	reader.change = nil
	if err := resolver.WithCredential(ctx, identity, func(_ context.Context, secret []byte) error {
		consumed = secret
		if string(secret) != "sk-ant-model-test" {
			t.Fatalf("trusted consumer received %q", secret)
		}
		return nil
	}); err != nil {
		t.Fatalf("trusted resolver failed: %v", err)
	}
	if reader.lastID != "mcred_modelrun01" {
		t.Fatalf("Claude Action resolver selected another purpose credential ID=%q", reader.lastID)
	}
	for _, b := range consumed {
		if b != 0 {
			t.Fatal("secret buffer was not wiped after trusted consumption")
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_model_credentials SET status='disabled',credential_generation=3,updated_at=now() WHERE tenant_id='tenant_model_run'`); err != nil {
		t.Fatal(err)
	}
	reads := reader.reads
	if err := resolver.WithCredential(ctx, identity, func(context.Context, []byte) error { t.Fatal("disabled credential was consumed"); return nil }); err == nil || reader.reads != reads {
		t.Fatalf("disabled credential resolution error=%v reads=%d want=%d", err, reader.reads, reads)
	}
}
