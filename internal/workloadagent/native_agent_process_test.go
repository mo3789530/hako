package workloadagent

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type nativeAgentModelFunc func(context.Context, []byte, AnthropicMessagesRequest) (AnthropicMessagesResponse, error)

func (f nativeAgentModelFunc) CreateMessage(ctx context.Context, credential []byte, request AnthropicMessagesRequest) (AnthropicMessagesResponse, error) {
	return f(ctx, credential, request)
}

type nativeAgentTestRunnerFunc func(context.Context, ExecutionPlan, io.Writer) error

func (f nativeAgentTestRunnerFunc) RunTests(ctx context.Context, plan ExecutionPlan, output io.Writer) error {
	return f(ctx, plan, output)
}

func TestNativeAgentProcessUsesOnlyFixedToolsAndRequiresTestsAfterWrites(t *testing.T) {
	root := t.TempDir()
	identity := testIdentity()
	task := AgentTask{TenantID: identity.TenantID, WorkloadRunID: identity.WorkloadRunID,
		IntegrationID: "integration_1", DeliveryID: "delivery_12345678", InstallationID: 71, RepositoryID: 81,
		IssueNumber: 3, CommentID: 4, DefaultBranch: "main", CommitSHA: strings.Repeat("b", 40),
		Title: "Add feature", Body: "Implement the requested feature"}
	task.ContextSHA256 = AgentTaskContextSHA256(task)
	plan := ExecutionPlan{Identity: identity, Kind: "agent", RepositoryPath: root, CommitSHA: task.CommitSHA,
		AgentTask: &task, AgentTaskContextSHA256: task.ContextSHA256, AgentModelCredential: []byte("sk-ant-test"),
		Deadline: time.Now().Add(time.Minute)}
	responses := []AnthropicMessagesResponse{
		nativeAgentToolResponse(t, "write_file", `{"path":"main.go","content":"package main\n"}`, "tool_write"),
		nativeAgentToolResponse(t, "run_tests", `{}`, "tool_tests"),
		nativeAgentEndResponse(t),
	}
	var modelCalls, testCalls int
	model := nativeAgentModelFunc(func(_ context.Context, credential []byte, request AnthropicMessagesRequest) (AnthropicMessagesResponse, error) {
		modelCalls++
		if string(credential) != "sk-ant-test" || request.Model != "" || request.MaxTokens != 4096 || len(request.Tools) != 4 ||
			request.ToolChoice == nil || !request.ToolChoice.DisableParallelToolUse || len(request.Messages) < modelCalls {
			t.Fatalf("model request exceeded the fixed Native Agent contract: calls=%d request=%+v", modelCalls, request)
		}
		return responses[modelCalls-1], nil
	})
	tests := nativeAgentTestRunnerFunc(func(_ context.Context, got ExecutionPlan, output io.Writer) error {
		testCalls++
		if got.RepositoryPath != root || got.AgentTask == nil || output != io.Discard {
			t.Fatalf("test runner received unexpected execution context: %+v", got)
		}
		return nil
	})
	var output strings.Builder
	process, err := (NativeAgentProcessFactory{Model: model, Tests: tests}).Start(context.Background(), plan, &output)
	if err != nil {
		t.Fatal(err)
	}
	code, err := process.Wait()
	if err != nil || code != 0 || modelCalls != 3 || testCalls != 1 {
		t.Fatalf("agent process result = (%d, %v), model calls=%d test calls=%d", code, err, modelCalls, testCalls)
	}
	changeProvider, ok := process.(interface{ ChangesArchive() []byte })
	if !ok {
		t.Fatal("Native Agent process did not expose its bounded change archive")
	}
	bundle := changeProvider.ChangesArchive()
	defer clear(bundle)
	zipReader, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil || len(zipReader.File) != 1 || zipReader.File[0].Name != "main.go" {
		t.Fatalf("change bundle did not contain the expected source file: files=%v err=%v", zipReader, err)
	}
	file, err := zipReader.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	packed, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || string(packed) != "package main\n" {
		t.Fatalf("change bundle content=%q readErr=%v closeErr=%v", packed, readErr, closeErr)
	}
	content, err := os.ReadFile(filepath.Join(root, "main.go"))
	if err != nil || string(content) != "package main\n" || !strings.Contains(output.String(), "tests_passed=true") || strings.Contains(output.String(), "sk-ant-test") {
		t.Fatalf("agent did not safely apply and verify the change: content=%q output=%q err=%v", content, output.String(), err)
	}
}

func TestNativeAgentProcessRejectsEndTurnAfterChangesWithoutTests(t *testing.T) {
	root := t.TempDir()
	identity := testIdentity()
	task := AgentTask{TenantID: identity.TenantID, WorkloadRunID: identity.WorkloadRunID,
		IntegrationID: "integration_1", DeliveryID: "delivery_12345678", InstallationID: 71, RepositoryID: 81,
		IssueNumber: 3, CommentID: 4, DefaultBranch: "main", CommitSHA: strings.Repeat("b", 40), Title: "Add feature"}
	task.ContextSHA256 = AgentTaskContextSHA256(task)
	plan := ExecutionPlan{Identity: identity, Kind: "agent", RepositoryPath: root, CommitSHA: task.CommitSHA,
		AgentTask: &task, AgentTaskContextSHA256: task.ContextSHA256, AgentModelCredential: []byte("sk-ant-test"),
		Deadline: time.Now().Add(time.Minute)}
	responses := []AnthropicMessagesResponse{
		nativeAgentToolResponse(t, "write_file", `{"path":"main.go","content":"package main\n"}`, "tool_write"),
		nativeAgentEndResponse(t),
	}
	var calls int
	model := nativeAgentModelFunc(func(context.Context, []byte, AnthropicMessagesRequest) (AnthropicMessagesResponse, error) {
		response := responses[calls]
		calls++
		return response, nil
	})
	tests := nativeAgentTestRunnerFunc(func(context.Context, ExecutionPlan, io.Writer) error {
		t.Fatal("tests must not run unless requested")
		return nil
	})
	process, err := (NativeAgentProcessFactory{Model: model, Tests: tests}).Start(context.Background(), plan, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	code, err := process.Wait()
	if code == 0 || !errors.Is(err, ErrNativeAgentExecution) {
		t.Fatalf("agent accepted untested changes: code=%d err=%v", code, err)
	}
}

func TestProcessBackendServesChangeBundleOnlyForCompletedAgentAttempt(t *testing.T) {
	identity := testIdentity()
	now := time.Now().UTC()
	root := t.TempDir()
	task := AgentTask{TenantID: identity.TenantID, WorkloadRunID: identity.WorkloadRunID,
		IntegrationID: "integration_1", DeliveryID: "delivery_12345678", InstallationID: 71, RepositoryID: 81,
		IssueNumber: 3, CommentID: 4, DefaultBranch: "main", CommitSHA: strings.Repeat("b", 40), Title: "Add feature"}
	task.ContextSHA256 = AgentTaskContextSHA256(task)
	plan := ExecutionPlan{Identity: identity, Kind: "agent", RepositoryPath: root, SourceDigest: strings.Repeat("a", 64),
		CommitSHA: task.CommitSHA, TriggerEvent: "issue_comment", TriggerBranch: task.DefaultBranch, AgentIntegrationID: task.IntegrationID,
		AgentInstallationID: task.InstallationID, AgentRepositoryID: task.RepositoryID,
		AgentTaskContextSHA256: task.ContextSHA256, AgentTask: &task, AgentModelCredential: []byte("sk-ant-test"), Deadline: now.Add(time.Minute)}
	responses := []AnthropicMessagesResponse{
		nativeAgentToolResponse(t, "write_file", `{"path":"new.go","content":"package main\n"}`, "tool_write"),
		nativeAgentToolResponse(t, "run_tests", `{}`, "tool_tests"),
		nativeAgentEndResponse(t),
	}
	var calls int
	factory := NativeAgentProcessFactory{Model: nativeAgentModelFunc(func(context.Context, []byte, AnthropicMessagesRequest) (AnthropicMessagesResponse, error) {
		response := responses[calls]
		calls++
		return response, nil
	}), Tests: nativeAgentTestRunnerFunc(func(context.Context, ExecutionPlan, io.Writer) error { return nil })}
	backend, err := NewProcessBackend(ProcessConfig{Resolver: fixedExecutionResolver{plan: plan}, Factory: factory, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close(context.Background())
	claims := planTicketClaims("microvm_1", identity, OperationStart, plan)
	request := StartRequest{Identity: IdentityFromCommand(identity), Kind: "agent", AgentTask: &task,
		AgentModelCredential: []byte("sk-ant-test"), SourceDigest: plan.SourceDigest, CommitSHA: plan.CommitSHA, Deadline: plan.Deadline}
	if err := backend.Start(context.Background(), claims, request); err != nil {
		t.Fatal(err)
	}
	changeClaims := planTicketClaims("microvm_1", identity, OperationChanges, plan)
	if _, err := backend.ChangeBundle(context.Background(), changeClaims); !errors.Is(err, ErrAgentBusy) {
		t.Fatalf("change bundle became available before completion: %v", err)
	}
	waitForAgentTerminal(t, backend, identity)
	bundle, err := backend.ChangeBundle(context.Background(), changeClaims)
	if err != nil {
		t.Fatal(err)
	}
	zipReader, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil || len(zipReader.File) != 1 || zipReader.File[0].Name != "new.go" {
		t.Fatalf("attempt-bound change bundle invalid: files=%v err=%v", zipReader, err)
	}
	foreign := changeClaims
	foreign.MicroVMID = "microvm_other"
	if _, err := backend.ChangeBundle(context.Background(), foreign); !errors.Is(err, ErrAttemptNotFound) {
		t.Fatalf("foreign VM read change bundle: %v", err)
	}
}

func TestNativeAgentWorkspaceRejectsTraversalSecretsProtectedFilesAndSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	workspace := &nativeAgentWorkspace{root: root, maxFiles: 2, maxBytes: 32, maxFileBytes: 16, changed: make(map[string]int)}
	for _, path := range []string{"../outside.txt", "link.txt", ".env", ".aws/credentials", ".github/workflows/test.yml", "terraform/main.tf", "C:/outside.txt"} {
		if err := workspace.writeFile(path, []byte("bad")); err == nil {
			t.Errorf("write_file accepted protected path %q", path)
		}
	}
	if _, err := workspace.readFile("link.txt"); err == nil {
		t.Fatal("read_file followed a symlink")
	}
	if _, err := workspace.readFile("../outside.txt"); err == nil {
		t.Fatal("read_file escaped the repository")
	}
	if _, err := workspace.readFile(".github/workflows/test.yml"); err == nil {
		t.Fatal("read_file exposed protected workflow configuration")
	}
	if err := workspace.writeFile("src/main.go", []byte("package main\n")); err == nil {
		t.Fatal("write_file created a new directory outside its bounded contract")
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "outside" {
		t.Fatalf("outside file changed: %q err=%v", got, err)
	}
}

func nativeAgentToolResponse(t *testing.T, name, input, id string) AnthropicMessagesResponse {
	t.Helper()
	block, err := json.Marshal(AnthropicContentBlock{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)})
	if err != nil {
		t.Fatal(err)
	}
	return AnthropicMessagesResponse{ID: "msg_test", Type: "message", Role: "assistant", Model: "claude-test-1",
		Content: []json.RawMessage{block}, StopReason: "tool_use"}
}

func nativeAgentEndResponse(t *testing.T) AnthropicMessagesResponse {
	t.Helper()
	block, err := json.Marshal(AnthropicContentBlock{Type: "text", Text: "Work is complete."})
	if err != nil {
		t.Fatal(err)
	}
	return AnthropicMessagesResponse{ID: "msg_test", Type: "message", Role: "assistant", Model: "claude-test-1",
		Content: []json.RawMessage{block}, StopReason: "end_turn"}
}
