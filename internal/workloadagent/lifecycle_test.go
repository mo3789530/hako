package workloadagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

func TestMicroVMLifecycleRunPinsTicketToProviderAndAttemptIdentity(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	id := testIdentity()
	execution := ExecutionBinding{Kind: domain.WorkloadAgent, AgentIntegrationID: "integration_1", AgentInstallationID: 71, AgentRepositoryID: 81,
		AgentTaskContextSHA256: strings.Repeat("d", 64),
		SourceDigest:           strings.Repeat("a", 64), CommitSHA: strings.Repeat("b", 40), TriggerEvent: "issue_comment", TriggerBranch: "main",
		CorrelationID: "req_bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Deadline: now.Add(time.Hour)}
	backend := &memoryAgentBackend{}
	server, err := newServer(backend, &memorySourceInstaller{}, map[string]ed25519.PublicKey{"rp-v1": publicKey}, func() time.Time { return now }, true)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewMicroVMLifecycleHandler(server.Handler(), server)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &Ed25519TicketIssuer{KeyID: "rp-v1", PrivateKey: privateKey, Now: func() time.Time { return now }}
	statusPath := "/v1/tenants/tenant_1/runs/run_1/attempts/attempt_1/generations/2/resource-planes/rp_1/status"
	statusRequest := func(microVMID string, identity workloadjournal.Command) *httptest.ResponseRecorder {
		t.Helper()
		ticket, err := issuer.Issue(microVMID, identity, OperationStatus)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, statusPath, nil)
		req.Header.Set("X-Hako-Agent-Protocol", ProtocolVersion)
		req.Header.Set("X-Hako-Idempotency-Token", id.IdempotencyToken)
		req.Header.Set("Authorization", "Bearer "+ticket)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder
	}

	if got := statusRequest("microvm_1", id).Code; got != http.StatusServiceUnavailable {
		t.Fatalf("protocol request before /run hook status = %d, want 503", got)
	}
	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodPost, microVMLifecyclePrefix+"ready", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("image ready hook status = %d, want 200", ready.Code)
	}

	postRun := func(microVMID string, runtimeIdentity workloadjournal.Command) *httptest.ResponseRecorder {
		t.Helper()
		payload, err := json.Marshal(runtimeHookPayload{Identity: runtimeIdentity, Kind: execution.Kind,
			AgentIntegrationID: execution.AgentIntegrationID, AgentInstallationID: execution.AgentInstallationID,
			AgentRepositoryID: execution.AgentRepositoryID, AgentTaskContextSHA256: execution.AgentTaskContextSHA256,
			SourceDigest: execution.SourceDigest, TriggerEvent: execution.TriggerEvent,
			TriggerBranch: execution.TriggerBranch, CommitSHA: execution.CommitSHA, CorrelationID: execution.CorrelationID, Deadline: execution.Deadline})
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(runtimeHookRequest{MicroVMID: microVMID, RunHookPayload: string(payload)})
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, microVMLifecyclePrefix+"run", strings.NewReader(string(body))))
		return recorder
	}
	if got := postRun("microvm_1", id).Code; got != http.StatusOK {
		t.Fatalf("first /run hook status = %d, want 200", got)
	}
	if got := postRun("microvm_1", id).Code; got != http.StatusOK {
		t.Fatalf("idempotent /run hook retry status = %d, want 200", got)
	}
	if binding := server.currentRuntimeBinding(); binding == nil || binding.Execution != execution {
		t.Fatalf("run hook lost correlation ID: %+v", binding)
	}
	if got := statusRequest("microvm_1", id).Code; got != http.StatusNotFound {
		t.Fatalf("matching VM/attempt ticket should reach backend, status = %d, want 404", got)
	}
	if got := statusRequest("microvm_other", id).Code; got != http.StatusUnauthorized {
		t.Fatalf("ticket for another MicroVM was accepted, status = %d", got)
	}
	otherAttempt := id
	otherAttempt.WorkloadRunID = "run_other"
	if got := statusRequest("microvm_1", otherAttempt).Code; got != http.StatusUnauthorized {
		t.Fatalf("ticket for another attempt was accepted, status = %d", got)
	}
	if got := postRun("microvm_other", id).Code; got != http.StatusConflict {
		t.Fatalf("conflicting /run hook changed the pinned runtime, status = %d", got)
	}
	if got := postRun("microvm_1", otherAttempt).Code; got != http.StatusConflict {
		t.Fatalf("conflicting /run attempt changed the pinned runtime, status = %d", got)
	}
	wrongExecution := execution
	wrongExecution.SourceDigest = strings.Repeat("c", 64)
	wrongExecutionTicket, err := issuer.IssueExecution("microvm_1", id, OperationStart, wrongExecution)
	if err != nil {
		t.Fatal(err)
	}
	startRequest := httptest.NewRequest(http.MethodPut, "/v1/tenants/tenant_1/runs/run_1/attempts/attempt_1/generations/2/resource-planes/rp_1", strings.NewReader("{}"))
	startRequest.Header.Set("X-Hako-Agent-Protocol", ProtocolVersion)
	startRequest.Header.Set("X-Hako-Idempotency-Token", id.IdempotencyToken)
	startRequest.Header.Set("Authorization", "Bearer "+wrongExecutionTicket)
	wrongExecutionResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongExecutionResponse, startRequest)
	if wrongExecutionResponse.Code != http.StatusUnauthorized || backend.starts != 0 {
		t.Fatalf("ticket with different pinned execution was accepted: status=%d starts=%d", wrongExecutionResponse.Code, backend.starts)
	}

	badBody := `{"microvmId":"microvm_1","runHookPayload":"{}","unexpected":true}`
	badRun := httptest.NewRecorder()
	handler.ServeHTTP(badRun, httptest.NewRequest(http.MethodPost, microVMLifecyclePrefix+"run", strings.NewReader(badBody)))
	if badRun.Code != http.StatusBadRequest {
		t.Fatalf("unknown /run hook field status = %d, want 400", badRun.Code)
	}
}

func TestMicroVMLifecycleRunRejectsInvalidPayloadAndBoundsBody(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server, err := newServer(&memoryAgentBackend{}, &memorySourceInstaller{}, map[string]ed25519.PublicKey{"rp-v1": publicKey}, time.Now, true)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewMicroVMLifecycleHandler(server.Handler(), server)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"empty":             "",
		"unknown inner key": `{"microvmId":"microvm_1","runHookPayload":"{\"unexpected\":true}"}`,
		"bad deadline":      `{"microvmId":"microvm_1","runHookPayload":"{\"identity\":{},\"source_digest\":\"bad\",\"commit_sha\":\"bad\",\"deadline\":\"not-a-time\"}"}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, microVMLifecyclePrefix+"run", strings.NewReader(body)))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("invalid /run payload status = %d, want 400", recorder.Code)
			}
		})
	}
	oversized := httptest.NewRecorder()
	handler.ServeHTTP(oversized, httptest.NewRequest(http.MethodPost, microVMLifecyclePrefix+"run", strings.NewReader(strings.Repeat("x", maxRunHookPayloadBytes+1))))
	if oversized.Code != http.StatusBadRequest {
		t.Fatalf("oversized /run payload status = %d, want 400", oversized.Code)
	}
}
