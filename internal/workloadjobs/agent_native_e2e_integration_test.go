//go:build integration

package workloadjobs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/dsql"
	storewebhook "github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/testutil"
	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workloadjobs"
)

const (
	agentE2EIntegrationID = "integration_native_e2e"
	agentE2EDeliveryID    = "a1b2c3d4-e5f6-4789-8abc-def012345678"
	agentE2ECommit        = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type nativeE2EIssueProvider struct{ context githubapp.AgentIssueContext }

func (p nativeE2EIssueProvider) Resolve(_ context.Context, integrationID, deliveryID string) (githubapp.AgentIssueContext, error) {
	if integrationID != agentE2EIntegrationID || deliveryID != agentE2EDeliveryID {
		return githubapp.AgentIssueContext{}, fmt.Errorf("unexpected Issue context request")
	}
	return p.context, nil
}

type nativeE2EModel struct {
	responses []workloadagent.AnthropicMessagesResponse
	calls     int
}

func (m *nativeE2EModel) CreateMessage(_ context.Context, credential []byte, request workloadagent.AnthropicMessagesRequest) (workloadagent.AnthropicMessagesResponse, error) {
	if string(credential) != "synthetic-model-credential" || request.MaxTokens != 4096 || len(request.Tools) != 4 ||
		request.ToolChoice == nil || !request.ToolChoice.DisableParallelToolUse || len(request.Messages) < 1 ||
		!strings.Contains(string(request.Messages[0].Content), "Add a bounded native Agent E2E test") {
		return workloadagent.AnthropicMessagesResponse{}, workloadagent.ErrNativeAgentExecution
	}
	if m.calls >= len(m.responses) {
		return workloadagent.AnthropicMessagesResponse{}, workloadagent.ErrNativeAgentExecution
	}
	response := m.responses[m.calls]
	m.calls++
	return response, nil
}

type nativeE2ETestRunner struct{ calls int }

func (r *nativeE2ETestRunner) RunTests(_ context.Context, plan workloadagent.ExecutionPlan, output io.Writer) error {
	r.calls++
	if plan.Kind != domain.WorkloadAgent || plan.AgentTask == nil || plan.AgentTask.CommitSHA != agentE2ECommit || output != io.Discard {
		return workloadagent.ErrNativeAgentExecution
	}
	return nil
}

func nativeE2EToolResponse(t *testing.T, name, input, id string) workloadagent.AnthropicMessagesResponse {
	t.Helper()
	block, err := json.Marshal(workloadagent.AnthropicContentBlock{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)})
	if err != nil {
		t.Fatal(err)
	}
	return workloadagent.AnthropicMessagesResponse{ID: "native-e2e", Type: "message", Role: "assistant", Model: "claude-test",
		Content: []json.RawMessage{block}, StopReason: "tool_use"}
}

func nativeE2EEndResponse(t *testing.T) workloadagent.AnthropicMessagesResponse {
	t.Helper()
	block, err := json.Marshal(workloadagent.AnthropicContentBlock{Type: "text", Text: "Tests passed; proposal is ready for review."})
	if err != nil {
		t.Fatal(err)
	}
	return workloadagent.AnthropicMessagesResponse{ID: "native-e2e", Type: "message", Role: "assistant", Model: "claude-test",
		Content: []json.RawMessage{block}, StopReason: "end_turn"}
}

func TestIssueCommandToNativeAgentProposalUsesPersistedBoundedContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_native_e2e','Native E2E',now())`,
		`INSERT INTO users(id,cognito_subject,email,created_at,github_user_id,github_login) VALUES ('usr_native_e2e','native-e2e','',now(),98101,'native-author')`,
		`INSERT INTO tenant_members(tenant_id,user_id,role,joined_at) VALUES ('tenant_native_e2e','usr_native_e2e','member',now())`,
		`INSERT INTO github_credential_bindings(id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id)
		 VALUES ('binding_native_e2e','tenant_native_e2e',98102,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/native-key-AbCdEf','oauth-ref','webhook-ref',now(),'arn:aws:iam::123456789012:role/hako/private-key-reader','0123456789abcdef0123456789abcdef')`,
		`INSERT INTO github_app_registrations(id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_native_e2e',98102,'tenant_owned','tenant_native_e2e','native-app','client',now())`,
		`INSERT INTO tenant_github_integrations(id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at)
		 VALUES ('integration_native_e2e','tenant_native_e2e','registration_native_e2e',98102,'binding_native_e2e','active',1,1,'native-idem','request-hash','usr_native_e2e',now(),now())`,
		`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_native_e2e',98103,'native-owner','active','usr_native_e2e',now(),now())`,
		`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (98103,'tenant_native_e2e',now())`,
		`INSERT INTO tenant_github_app_installation_bindings(tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_native_e2e','integration_native_e2e',98103,now())`,
		`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,factory_enabled,factory_enabled_at,agent_model_access_enabled,agent_model_access_enabled_at)
		 VALUES ('tenant_native_e2e',98103,98104,'native-owner','native-repo','main',now(),true,now()-interval '1 minute',true,now()-interval '1 minute')`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	delivery := githubwebhook.VerifiedDelivery{IntegrationID: agentE2EIntegrationID, CredentialGeneration: 1,
		DeliveryID: agentE2EDeliveryID, Event: "issue_comment", Action: "created",
		Payload: []byte(`{"action":"created","installation":{"id":98103,"app_id":98102},"repository":{"id":98104,"name":"native-repo","default_branch":"main","owner":{"login":"native-owner"}},"issue":{"number":31},"comment":{"id":41,"body":"/hako implement","author_association":"MEMBER"},"sender":{"id":98101,"login":"native-author"}}`)}
	if inserted, err := (storewebhook.Inbox{Pool: pool, Policy: transaction.DefaultPolicy()}).Record(ctx, delivery, now); err != nil || !inserted {
		t.Fatalf("record Issue delivery inserted=%v error=%v", inserted, err)
	}
	issue := githubapp.AgentIssueContext{IssueNumber: 31, Title: "Native Agent request", Body: "Add a bounded native Agent E2E test",
		CommitSHA: agentE2ECommit, DefaultRef: "main"}
	stats, err := workloadjobs.ProcessAgentIssueCommandBatch(ctx, pool, transaction.DefaultPolicy(), 10,
		nativeE2EIssueProvider{context: issue}, now.Add(time.Second))
	if err != nil || stats.Claimed != 1 || stats.Processed != 1 || stats.WorkloadRunsCreated != 1 {
		t.Fatalf("Issue command batch=%+v error=%v", stats, err)
	}

	var run domain.WorkloadRun
	var task workloads.AgentTaskRecord
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		var err error
		run, err = workloads.Get(ctx, tx, "tenant_native_e2e", mustFindNativeAgentRunID(ctx, t, tx))
		if err != nil {
			return struct{}{}, err
		}
		task, err = workloads.GetAgentTask(ctx, tx, run.TenantID, run.ID)
		return struct{}{}, err
	}); err != nil {
		t.Fatal(err)
	}
	if run.Kind != domain.WorkloadAgent || run.CommitSHA != agentE2ECommit || task.Title != issue.Title || task.Body != issue.Body ||
		task.DeliveryID != agentE2EDeliveryID || task.ContextSHA256 == "" {
		t.Fatalf("persisted Agent identity/context mismatch: run=%+v task=%+v", run, task)
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	identity := workloadagent.Identity{TenantID: string(run.TenantID), WorkloadRunID: string(run.ID), AttemptID: "attempt-native-e2e",
		Generation: 1, ResourcePlaneID: "rp-native-e2e", IdempotencyToken: "idem-native-e2e"}
	guestTask := workloadagent.AgentTask{TenantID: string(task.TenantID), WorkloadRunID: string(task.WorkloadRunID), IntegrationID: task.IntegrationID,
		DeliveryID: task.DeliveryID, InstallationID: task.InstallationID, RepositoryID: task.RepositoryID, IssueNumber: task.IssueNumber,
		CommentID: task.CommentID, DefaultBranch: task.DefaultBranch, CommitSHA: task.CommitSHA, Title: task.Title, Body: task.Body,
		ContextSHA256: task.ContextSHA256}
	model := &nativeE2EModel{responses: []workloadagent.AnthropicMessagesResponse{
		nativeE2EToolResponse(t, "write_file", `{"path":"main.go","content":"package main\n\nfunc Answer() int { return 42 }\n"}`, "write-native"),
		nativeE2EToolResponse(t, "run_tests", `{}`, "tests-native"), nativeE2EEndResponse(t),
	}}
	verifier := &nativeE2ETestRunner{}
	plan := workloadagent.ExecutionPlan{Identity: identity.Command(), Kind: domain.WorkloadAgent, RepositoryPath: root, CommitSHA: task.CommitSHA,
		AgentTask: &guestTask, AgentTaskContextSHA256: task.ContextSHA256, AgentModelCredential: []byte("synthetic-model-credential"), Deadline: time.Now().Add(time.Minute)}
	process, err := (workloadagent.NativeAgentProcessFactory{Model: model, Tests: verifier}).Start(ctx, plan, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := process.Wait(); err != nil || code != 0 || model.calls != 3 || verifier.calls != 1 {
		t.Fatalf("Agent result code=%d err=%v model calls=%d test calls=%d", code, err, model.calls, verifier.calls)
	}
	proposal, ok := process.(interface{ ChangesArchive() []byte })
	if !ok || len(proposal.ChangesArchive()) == 0 {
		t.Fatal("successful Issue Agent did not produce an untrusted proposal archive")
	}
	updated, err := os.ReadFile(filepath.Join(root, "main.go"))
	if err != nil || !strings.Contains(string(updated), "Answer() int") {
		t.Fatalf("Agent did not apply the bounded proposal: %q error=%v", updated, err)
	}
}

func mustFindNativeAgentRunID(ctx context.Context, t *testing.T, tx pgx.Tx) domain.WorkloadRunID {
	t.Helper()
	var id domain.WorkloadRunID
	if err := tx.QueryRow(ctx, `SELECT id FROM workload_runs WHERE tenant_id='tenant_native_e2e' AND kind='agent'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
