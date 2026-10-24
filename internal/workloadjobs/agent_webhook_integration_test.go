//go:build integration

package workloadjobs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/dsql"
	storewebhook "github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/testutil"
)

type fakeAgentIssueContextProvider struct {
	issue githubapp.AgentIssueContext
	calls int
}

func (p *fakeAgentIssueContextProvider) Resolve(_ context.Context, integrationID, deliveryID string) (githubapp.AgentIssueContext, error) {
	p.calls++
	if integrationID != "integration_agent_run" || deliveryID != "a6b2c3d4-e5f6-4789-8abc-def012345678" {
		return githubapp.AgentIssueContext{}, fmt.Errorf("unexpected issue request")
	}
	return p.issue, nil
}

func TestProcessAgentIssueCommentCreatesOneImmutableRunAndTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, query := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_agent_run','Agent run',now())`,
		`INSERT INTO users(id,cognito_subject,email,created_at,github_user_id,github_login) VALUES ('usr_agent_run','agent-run','',now(),78101,'agent-author')`,
		`INSERT INTO tenant_members(tenant_id,user_id,role,joined_at) VALUES ('tenant_agent_run','usr_agent_run','member',now())`,
		`INSERT INTO github_credential_bindings(id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id)
		 VALUES ('binding_agent_run','tenant_agent_run',78102,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/app-key-AbCdEf','oauth-ref','webhook-ref',now(),'arn:aws:iam::123456789012:role/hako/private-key-reader','0123456789abcdef0123456789abcdef')`,
		`INSERT INTO github_app_registrations(id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_agent_run',78102,'tenant_owned','tenant_agent_run','agent-app','client',now())`,
		`INSERT INTO tenant_github_integrations(id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at)
		 VALUES ('integration_agent_run','tenant_agent_run','registration_agent_run',78102,'binding_agent_run','active',1,1,'agent-idem','request-hash','usr_agent_run',now(),now())`,
		`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_agent_run',78103,'agent-owner','active','usr_agent_run',now(),now())`,
		`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (78103,'tenant_agent_run',now())`,
		`INSERT INTO tenant_github_app_installation_bindings(tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_agent_run','integration_agent_run',78103,now())`,
		`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,factory_enabled,factory_enabled_at)
		 VALUES ('tenant_agent_run',78103,78104,'agent-owner','agent-repo','main',now(),true,now()-interval '1 minute')`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	inbox := storewebhook.Inbox{Pool: pool, Policy: transaction.DefaultPolicy()}
	delivery := githubwebhook.VerifiedDelivery{IntegrationID: "integration_agent_run", CredentialGeneration: 1,
		DeliveryID: "a6b2c3d4-e5f6-4789-8abc-def012345678", Event: "issue_comment", Action: "created",
		Payload: []byte(`{"action":"created","installation":{"id":78103,"app_id":78102},"repository":{"id":78104,"name":"agent-repo","default_branch":"main","owner":{"login":"agent-owner"}},"issue":{"number":31},"comment":{"id":41,"body":"/hako implement","author_association":"MEMBER"},"sender":{"id":78101,"login":"agent-author"}}`)}
	if inserted, err := inbox.Record(ctx, delivery, now); err != nil || !inserted {
		t.Fatalf("record delivery inserted=%v error=%v", inserted, err)
	}
	provider := &fakeAgentIssueContextProvider{issue: githubapp.AgentIssueContext{IssueNumber: 31, Title: "Implement feature", Body: "Add a bounded feature", CommitSHA: strings.Repeat("a", 40), DefaultRef: "main"}}
	firstBatch, err := ProcessAgentIssueCommandBatch(ctx, pool, transaction.DefaultPolicy(), 10, provider, now.Add(time.Second))
	if err != nil || firstBatch.Claimed != 1 || firstBatch.Processed != 1 || firstBatch.WorkloadRunsCreated != 1 {
		t.Fatalf("first Agent command batch=%+v error=%v", firstBatch, err)
	}
	replayBatch, err := ProcessAgentIssueCommandBatch(ctx, pool, transaction.DefaultPolicy(), 10, provider, now.Add(2*time.Second))
	if err != nil || replayBatch.Claimed != 0 || replayBatch.Processed != 0 || replayBatch.WorkloadRunsCreated != 0 {
		t.Fatalf("Agent command replay batch=%+v error=%v", replayBatch, err)
	}
	var run domain.WorkloadRun
	if err := pool.QueryRow(ctx, `SELECT `+workloadRunColumnsForTest+` FROM workload_runs WHERE tenant_id='tenant_agent_run'`).Scan(
		&run.ID, &run.TenantID, &run.Kind, &run.RequestedBy, &run.GitHubInstallation, &run.CorrelationID, &run.GitHubRepository,
		&run.GitHubIntegrationID, &run.GitHubConfigRevision, &run.GitHubCredentialGeneration, &run.CommitSHA, &run.Ref, &run.TriggerEvent,
		&run.TriggerBranch, &run.RuntimeClass, &run.TrustLevel, &run.DesiredState, &run.State, &run.ResourcePlaneID, &run.IdempotencyKey,
		&run.RequestHash, &run.TimeoutSeconds, &run.TimeoutAt, &run.StartedAt, &run.CompletedAt, &run.ErrorCode, &run.Revision, &run.CreatedAt, &run.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	if run.Kind != domain.WorkloadAgent || run.RequestedBy != "usr_agent_run" || run.CommitSHA != strings.Repeat("a", 40) || run.TriggerEvent != "issue_comment" || run.TriggerBranch != "main" ||
		run.GitHubIntegrationID != delivery.IntegrationID || run.State != domain.WorkloadPending {
		t.Fatalf("created Agent Run = %+v", run)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task, err := workloads.GetAgentTask(ctx, tx, run.TenantID, run.ID)
	_ = tx.Rollback(ctx)
	if err != nil || task.IssueNumber != 31 || task.CommentID != 41 || task.Title != provider.issue.Title || task.Body != provider.issue.Body || task.CommitSHA != run.CommitSHA {
		t.Fatalf("stored Agent task = %+v error=%v", task, err)
	}
	var runCount, taskCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_runs WHERE tenant_id='tenant_agent_run'`).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_agent_tasks`).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	var deliveryState string
	if err := pool.QueryRow(ctx, `SELECT processing_status FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2`, delivery.IntegrationID, delivery.DeliveryID).Scan(&deliveryState); err != nil {
		t.Fatal(err)
	}
	if runCount != 1 || taskCount != 1 || deliveryState != "processed" || provider.calls != 1 {
		t.Fatalf("run count=%d task count=%d delivery=%q broker lookups=%d", runCount, taskCount, deliveryState, provider.calls)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM tenant_members WHERE tenant_id='tenant_agent_run' AND user_id='usr_agent_run'`); err != nil {
		t.Fatal(err)
	}
	unauthorized := delivery
	unauthorized.DeliveryID = "b6b2c3d4-e5f6-4789-8abc-def012345678"
	unauthorized.Payload = []byte(strings.Replace(string(delivery.Payload), `"id":41`, `"id":42`, 1))
	if inserted, err := inbox.Record(ctx, unauthorized, now.Add(3*time.Second)); err != nil || !inserted {
		t.Fatalf("record unauthorized Issue command inserted=%v error=%v", inserted, err)
	}
	processed, created, err := ProcessAgentIssueComment(ctx, pool, transaction.DefaultPolicy(), provider, unauthorized.IntegrationID, unauthorized.DeliveryID, now.Add(4*time.Second))
	if err != nil || processed || created || provider.calls != 1 {
		t.Fatalf("revoked Tenant member reached broker or created Run: processed=%v created=%v calls=%d error=%v", processed, created, provider.calls, err)
	}
	if err := pool.QueryRow(ctx, `SELECT processing_status FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2`, unauthorized.IntegrationID, unauthorized.DeliveryID).Scan(&deliveryState); err != nil || deliveryState != "failed" {
		t.Fatalf("unauthorized delivery state=%q error=%v", deliveryState, err)
	}
}

const workloadRunColumnsForTest = `id,tenant_id,kind,COALESCE(requested_by,''),COALESCE(github_installation_id,0),COALESCE(correlation_id,''),
	COALESCE(github_repository_id,0),COALESCE(github_integration_id,''),COALESCE(github_config_revision,0),COALESCE(github_credential_generation,0),
	COALESCE(commit_sha,''),ref,trigger_event,trigger_branch,runtime_class,trust_level,desired_state,state,COALESCE(resource_plane_id,''),idempotency_key,request_hash,
	timeout_seconds,timeout_at,started_at,completed_at,COALESCE(error_code,''),revision,created_at,updated_at`
