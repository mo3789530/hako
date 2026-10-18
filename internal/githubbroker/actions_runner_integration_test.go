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

func TestPostgresActionsRunnerLaunchAndBootstrapRequireExactClaimPlaneAndMicroVM(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, statement := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_runner_start','Runner start',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_runner_start','runner-start','',now())`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id) VALUES ('binding_runner_start','tenant_runner_start',712,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:key','oauth','webhook',now(),'arn:aws:iam::123456789012:role/hako/key-reader','0123456789abcdef0123456789abcdef')`,
		`INSERT INTO github_app_registrations (id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_runner_start',712,'tenant_owned','tenant_runner_start','runner-app','client',now())`,
		`INSERT INTO tenant_github_integrations (id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_runner_start','tenant_runner_start','registration_runner_start',712,'binding_runner_start','active',3,7,'runner-start-key','runner-start-hash','usr_runner_start',now(),now())`,
		`INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_runner_start',7121,'acme','active','usr_runner_start',now(),now())`,
		`INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at) VALUES (7121,'tenant_runner_start',now())`,
		`INSERT INTO tenant_github_app_installation_bindings (tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_runner_start','integration_runner_start',7121,now())`,
		`INSERT INTO tenant_github_repositories (tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,actions_runner_enabled,actions_runner_enabled_at) VALUES ('tenant_runner_start',7121,7122,'acme','runner-api','main',now(),true,now()-interval '1 minute')`,
		`INSERT INTO github_webhook_deliveries (integration_id,delivery_id,event_type,action,payload_json,payload_sha256,received_at,processing_status,event_schema_version,normalized_event_json,installation_id,credential_generation,payload_expires_at) VALUES ('integration_runner_start','a3b2c3d4-e5f6-4789-8abc-def012345678','workflow_job','queued','{}','hash',now(),'processed',1,'{}',7121,7,now()+interval '7 days')`,
		`INSERT INTO github_actions_runner_jobs (integration_id,delivery_id,tenant_id,installation_id,repository_id,workflow_job_id,workflow_run_id,run_attempt,workflow_name,job_name,head_branch,head_sha,labels,state,created_at,updated_at,claim_id,lease_expires_at,claim_attempts) VALUES ('integration_runner_start','a3b2c3d4-e5f6-4789-8abc-def012345678','tenant_runner_start',7121,7122,7130,7131,1,'CI','test','main','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','["self-hosted","hako"]','claimed',now(),now(),'controller-claim-1',now()+interval '1 minute',1)`,
		`INSERT INTO github_actions_runner_checks (integration_id,workflow_job_id,run_attempt,tenant_id,installation_id,repository_id,workflow_run_id,commit_sha,check_name,external_id,status,started_at,output_title,output_summary,revision,updated_at) VALUES ('integration_runner_start',7130,1,'tenant_runner_start',7121,7122,7131,'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','Hako Actions Runner','hako-actions-runner/integration_runner_start/7130/1','queued',now(),'Hako Actions Runner','Runner admission is queued.',1,now())`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	request := ActionsRunnerRegistrationRequest{IntegrationID: "integration_runner_start", WorkflowJobID: 7130, RunAttempt: 1, ClaimID: "controller-claim-1"}
	var proof []byte
	if err := pool.QueryRow(ctx, `UPDATE tenant_github_repositories SET actions_runner_workflow_policy=$1::jsonb
		WHERE tenant_id='tenant_runner_start' AND installation_id=7121 AND github_repository_id=7122
		RETURNING actions_runner_workflow_policy`,
		`{"workflow_path":".github/workflows/claude.yml","allowed_refs":["refs/heads/main"],"allowed_events":["push"],"required_labels":["hako"],"allowed_labels":["self-hosted","hako"]}`).Scan(&proof); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPostgresActionsRunnerLaunchStarter(pool, transaction.DefaultPolicy())(ctx, "rp-tokyo-01", request); err == nil {
		t.Fatal("unverified starter consumed a claim with a configured workflow policy")
	}
	verifiedStart := NewPostgresVerifiedActionsRunnerLaunchStarter(pool, transaction.DefaultPolicy())
	if _, err := verifiedStart(ctx, "rp-tokyo-01", request, []byte(`{"workflow_path":".github/workflows/other.yml"}`)); err == nil {
		t.Fatal("verified starter accepted a policy snapshot that no longer matches the repository")
	}
	start := func(ctx context.Context, planeID string, got ActionsRunnerRegistrationRequest) (ActionsRunnerLaunchCommand, error) {
		return verifiedStart(ctx, planeID, got, proof)
	}
	command, err := start(ctx, "rp-tokyo-01", request)
	if err != nil || command.TenantID != "tenant_runner_start" || command.Installation != 7121 || command.Repository != 7122 || command.Owner != "acme" || command.Name != "runner-api" || command.State != "starting" || command.LeaseExpires.Before(now) {
		t.Fatalf("start exact claimed Runner job=%+v error=%v", command, err)
	}
	var state, claimID string
	if err := pool.QueryRow(ctx, `SELECT state,claim_id FROM github_actions_runner_jobs WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=1`, request.IntegrationID, request.WorkflowJobID).Scan(&state, &claimID); err != nil || state != "starting" || claimID != request.ClaimID {
		t.Fatalf("registration start did not consume claim: state=%q claim=%q err=%v", state, claimID, err)
	}
	if retry, err := start(ctx, "rp-tokyo-01", request); err != nil || retry.ResourcePlaneID != command.ResourcePlaneID || retry.ClaimID != command.ClaimID {
		t.Fatalf("lost launch response was not idempotently replayed: command=%+v err=%v", retry, err)
	}
	if _, err := start(ctx, "rp-us-01", request); err == nil {
		t.Fatal("same claimed job was dispatched to another Resource Plane")
	}
	binder := NewPostgresActionsRunnerBinder(pool, transaction.DefaultPolicy())
	binding := ActionsRunnerBindingRequest{IntegrationID: request.IntegrationID, WorkflowJobID: request.WorkflowJobID,
		RunAttempt: request.RunAttempt, ClaimID: request.ClaimID, MicroVMID: "lambda-microvm-runner-test"}
	bound, err := binder(ctx, "rp-tokyo-01", binding)
	if err != nil || !bound {
		t.Fatalf("bind exact live claim to launched Lambda MicroVM: bound=%v err=%v", bound, err)
	}
	if bound, err := binder(ctx, "rp-us-01", binding); err != nil || bound {
		t.Fatalf("another Resource Plane replaced the pinned provider identity: bound=%v err=%v", bound, err)
	}
	if bound, err := binder(ctx, "rp-tokyo-01", ActionsRunnerBindingRequest{IntegrationID: request.IntegrationID,
		WorkflowJobID: request.WorkflowJobID, RunAttempt: request.RunAttempt, ClaimID: "different-claim-1", MicroVMID: "another-microvm"}); err != nil || bound {
		t.Fatalf("different claim replaced the pinned provider identity: bound=%v err=%v", bound, err)
	}
	bootstrapStarter := NewPostgresActionsRunnerBootstrapTokenStarter(pool, transaction.DefaultPolicy())
	bootstrapIdentity, err := bootstrapStarter(ctx, "rp-tokyo-01", request)
	if err != nil || bootstrapIdentity.ResourcePlaneID != "rp-tokyo-01" || bootstrapIdentity.LambdaMicroVMID != binding.MicroVMID ||
		bootstrapIdentity.Credential.GitHubAppID != 712 {
		t.Fatalf("bootstrap token claim was not fenced to the bound MicroVM: identity=%+v err=%v", bootstrapIdentity, err)
	}
	if _, err := bootstrapStarter(ctx, "rp-tokyo-01", request); err == nil {
		t.Fatal("same starting claim issued a second registration token")
	}
	if _, err := bootstrapStarter(ctx, "rp-us-01", request); err == nil {
		t.Fatal("a different Resource Plane obtained the registration token")
	}
	if _, err := start(ctx, "rp-tokyo-01", request); err == nil {
		t.Fatal("launch replay continued after one-time token issuance")
	}
	var planeID, microVMID string
	if err := pool.QueryRow(ctx, `SELECT resource_plane_id,lambda_microvm_id FROM github_actions_runner_jobs WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=1`,
		request.IntegrationID, request.WorkflowJobID).Scan(&planeID, &microVMID); err != nil || planeID != "rp-tokyo-01" || microVMID != binding.MicroVMID {
		t.Fatalf("durable claim binding mismatch: plane=%q microvm=%q err=%v", planeID, microVMID, err)
	}
	fail := NewPostgresActionsRunnerStartupFailureRecorder(pool, transaction.DefaultPolicy())
	if err := fail(ctx, "rp-tokyo-01", request); err != nil {
		t.Fatal(err)
	}
	if err := fail(ctx, "rp-tokyo-01", request); err != nil {
		t.Fatalf("idempotent startup failure replay failed: %v", err)
	}
	if err := fail(ctx, "rp-us-01", request); err == nil {
		t.Fatal("foreign Resource Plane marked the runner startup failed")
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM github_actions_runner_jobs WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=1`, request.IntegrationID, request.WorkflowJobID).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("registration mint failure did not become terminal: state=%q err=%v", state, err)
	}
	var checkStatus, checkConclusion string
	var checkRevision int64
	if err := pool.QueryRow(ctx, `SELECT status,conclusion,revision FROM github_actions_runner_checks
		WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=1`, request.IntegrationID, request.WorkflowJobID).
		Scan(&checkStatus, &checkConclusion, &checkRevision); err != nil || checkStatus != "completed" || checkConclusion != "failure" || checkRevision != 2 {
		t.Fatalf("startup failure Check did not terminalize durably: status=%q conclusion=%q revision=%d err=%v", checkStatus, checkConclusion, checkRevision, err)
	}
	var cleanupState, cleanupReason string
	var cleanupMicroVM *string
	var cleanupPlane *string
	if err := pool.QueryRow(ctx, `SELECT state,reason,resource_plane_id,lambda_microvm_id FROM github_actions_runner_cleanup_outbox
		WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=1`, request.IntegrationID, request.WorkflowJobID).
		Scan(&cleanupState, &cleanupReason, &cleanupPlane, &cleanupMicroVM); err != nil || cleanupState != "pending" || cleanupReason != "failed" ||
		cleanupPlane == nil || *cleanupPlane != "rp-tokyo-01" || cleanupMicroVM == nil || *cleanupMicroVM != binding.MicroVMID {
		t.Fatalf("failed startup cleanup was not transactionally queued: state=%q reason=%q plane=%v MicroVM=%v error=%v", cleanupState, cleanupReason, cleanupPlane, cleanupMicroVM, err)
	}
}
