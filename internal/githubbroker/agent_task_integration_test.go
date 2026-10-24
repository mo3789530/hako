//go:build integration

package githubbroker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/agenttask"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestPostgresAgentTaskResolverReturnsOnlyCurrentPlaneAttempt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	commitSHA := strings.Repeat("a", 40)
	contextSHA256 := agenttask.SHA256(agenttask.Context{TenantID: "tenant_agent_task", WorkloadRunID: "run_agent_task",
		IntegrationID: "integration_agent_task", DeliveryID: "a1b2c3d4-e5f6-4789-8abc-def012345678", InstallationID: 17,
		RepositoryID: 18, IssueNumber: 19, CommentID: 20, DefaultBranch: "main", CommitSHA: commitSHA, Title: "Fix bug", Body: "details"})
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_agent_task','Agent task',$1)`, []any{now}},
		{`INSERT INTO users(id,cognito_subject,email,created_at,github_user_id,github_login) VALUES ('usr_agent_task','agent-task','',$1,991,'agent-task')`, []any{now}},
		{`INSERT INTO tenant_members(tenant_id,user_id,role,joined_at) VALUES ('tenant_agent_task','usr_agent_task','member',$1)`, []any{now}},
		{`INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES ('rp-agent-task','aws','ap-northeast-1','["microvm"]')`, nil},
		{`INSERT INTO github_credential_bindings(id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id)
		 VALUES ('binding_agent_task','tenant_agent_task',7701,'approved','private-ref','oauth-ref','webhook-ref',$1,'arn:aws:iam::123456789012:role/reader','0123456789abcdef0123456789abcdef')`, []any{now}},
		{`INSERT INTO github_app_registrations(id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at)
		 VALUES ('registration_agent_task',7701,'tenant_owned','tenant_agent_task','agent-app','client',$1)`, []any{now}},
		{`INSERT INTO tenant_github_integrations(id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at)
		 VALUES ('integration_agent_task','tenant_agent_task','registration_agent_task',7701,'binding_agent_task','active',2,3,'idem-agent-app','hash-agent-app','usr_agent_task',$1,$1)`, []any{now}},
		{`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at)
		 VALUES ('tenant_agent_task',17,'agent-task','active','usr_agent_task',$1,$1)`, []any{now}},
		{`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (17,'tenant_agent_task',$1)`, []any{now}},
		{`INSERT INTO tenant_github_app_installation_bindings(tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_agent_task','integration_agent_task',17,$1)`, []any{now}},
		{`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,factory_enabled,factory_enabled_at)
		 VALUES ('tenant_agent_task',17,18,'agent-task','agent-repo','main',$1::timestamptz,true,$1::timestamptz-interval '1 minute')`, []any{now}},
		{`INSERT INTO github_webhook_deliveries(integration_id,delivery_id,event_type,action,payload_json,payload_sha256,received_at,processing_status,event_schema_version,normalized_event_json,credential_generation,payload_expires_at)
		 VALUES ('integration_agent_task','a1b2c3d4-e5f6-4789-8abc-def012345678','issue_comment','created','{}','hash',$1::timestamptz,'processed',1,'{"actor_github_id":991}',3,$1::timestamptz+interval '7 days')`, []any{now}},
		{`INSERT INTO workload_runs(id,tenant_id,kind,requested_by,github_installation_id,github_repository_id,github_integration_id,github_config_revision,github_credential_generation,commit_sha,ref,trigger_event,trigger_branch,runtime_class,trust_level,desired_state,state,resource_plane_id,idempotency_key,request_hash,timeout_seconds,timeout_at,created_at,updated_at)
		 VALUES ('run_agent_task','tenant_agent_task','agent','usr_agent_task',17,18,'integration_agent_task',2,3,$2,'main','issue_comment','main','standard','untrusted','running','provisioning','rp-agent-task','idem-agent-task','hash-agent-task',1800,$1::timestamptz+interval '30 minutes',$1,$1)`, []any{now, commitSHA}},
		{`INSERT INTO workload_run_attempts(id,tenant_id,workload_run_id,generation,attempt_number,resource_plane_id,idempotency_token,state,created_at,updated_at)
		 VALUES ('attempt_agent_task','tenant_agent_task','run_agent_task',1,1,'rp-agent-task','token-agent-task','dispatch_pending',$1,$1)`, []any{now}},
		{`INSERT INTO workload_agent_tasks(tenant_id,workload_run_id,requested_by,integration_id,delivery_id,installation_id,repository_id,issue_number,comment_id,default_branch,commit_sha,issue_title,issue_body,context_sha256,created_at)
		 VALUES ('tenant_agent_task','run_agent_task','usr_agent_task','integration_agent_task','a1b2c3d4-e5f6-4789-8abc-def012345678',17,18,19,20,'main',$2,'Fix bug','details',$3,$1)`, []any{now, commitSHA, contextSHA256}},
	}
	for i, statement := range statements {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("fixture statement %d: %v", i, err)
		}
	}
	identity := AgentTaskIdentity{TenantID: "tenant_agent_task", WorkloadRunID: "run_agent_task", AttemptID: "attempt_agent_task",
		Generation: 1, ResourcePlaneID: "rp-agent-task"}
	service, err := NewAgentTaskService([]string{"rp-agent-task", "rp-other"}, NewPostgresAgentTaskResolver(pool, transaction.DefaultPolicy(), func() time.Time { return now.Add(time.Minute) }))
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.Resolve(ctx, resourcePlaneURIPrefix+"rp-agent-task", identity)
	if err != nil || task.Title != "Fix bug" || task.Body != "details" || task.CommitSHA != commitSHA {
		t.Fatalf("resolved task=%+v error=%v", task, err)
	}
	if _, err := service.Resolve(ctx, resourcePlaneURIPrefix+"rp-other", identity); err == nil {
		t.Fatal("foreign Resource Plane read Agent task")
	}
	if _, err := service.Resolve(ctx, resourcePlaneURIPrefix+"rp-agent-task", AgentTaskIdentity{TenantID: identity.TenantID,
		WorkloadRunID: identity.WorkloadRunID, AttemptID: identity.AttemptID, Generation: 2, ResourcePlaneID: identity.ResourcePlaneID}); err == nil {
		t.Fatal("stale attempt generation read Agent task")
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_repositories SET factory_enabled=false WHERE tenant_id='tenant_agent_task' AND github_repository_id=18`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resolve(ctx, resourcePlaneURIPrefix+"rp-agent-task", identity); err == nil {
		t.Fatal("disabled Factory repository still exposed Agent task")
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_repositories SET factory_enabled=true WHERE tenant_id='tenant_agent_task' AND github_repository_id=18`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM tenant_members WHERE tenant_id='tenant_agent_task' AND user_id='usr_agent_task'`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resolve(ctx, resourcePlaneURIPrefix+"rp-agent-task", identity); err == nil {
		t.Fatal("removed Tenant member still exposed Agent task")
	}
	if _, err := pool.Exec(ctx, `UPDATE workload_runs SET state='succeeded',desired_state='cancelled',completed_at=$1,updated_at=$1 WHERE tenant_id=$2 AND id=$3`, now, identity.TenantID, identity.WorkloadRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resolve(ctx, resourcePlaneURIPrefix+"rp-agent-task", identity); err == nil {
		t.Fatal("terminal Run still exposed Agent task")
	}
}
