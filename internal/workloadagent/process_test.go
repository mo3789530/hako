package workloadagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/factoryconfig"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

type fixedExecutionResolver struct {
	plan ExecutionPlan
	err  error
}

func (r fixedExecutionResolver) ResolveExecution(context.Context, TicketClaims) (ExecutionPlan, error) {
	return r.plan, r.err
}

func planTicketClaims(vm string, id workloadjournal.Command, operation Operation, plan ExecutionPlan) TicketClaims {
	claims := TicketClaims{MicroVMID: vm, Identity: IdentityFromCommand(id), Operation: operation}
	if operation == OperationStart || operation == OperationSource {
		claims.Execution = &ExecutionBinding{Kind: plan.Kind, AgentIntegrationID: plan.AgentIntegrationID,
			AgentInstallationID: plan.AgentInstallationID, AgentRepositoryID: plan.AgentRepositoryID,
			AgentTaskContextSHA256: plan.AgentTaskContextSHA256,
			SourceDigest:           plan.SourceDigest, CommitSHA: plan.CommitSHA,
			TriggerEvent: plan.TriggerEvent, TriggerBranch: plan.TriggerBranch, CorrelationID: plan.CorrelationID, Deadline: plan.Deadline.UTC()}
	}
	return claims
}

type fakeManagedProcess struct {
	ctx  context.Context
	done chan struct{}
	mu   sync.Mutex
	code int
	err  error
	once sync.Once
}

func (p *fakeManagedProcess) Wait() (int, error) {
	select {
	case <-p.ctx.Done():
		return 130, p.ctx.Err()
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.code, p.err
	}
}

func (p *fakeManagedProcess) finish(code int, err error) {
	p.mu.Lock()
	p.code, p.err = code, err
	p.mu.Unlock()
	p.once.Do(func() { close(p.done) })
}

type fakeProcessFactory struct {
	starts  int
	process *fakeManagedProcess
	plan    ExecutionPlan
	output  []byte
	err     error
}

func (f *fakeProcessFactory) Start(ctx context.Context, plan ExecutionPlan, output io.Writer) (ManagedProcess, error) {
	f.starts++
	if f.err != nil {
		return nil, f.err
	}
	f.plan = plan
	_, _ = output.Write(f.output)
	f.process = &fakeManagedProcess{ctx: ctx, done: make(chan struct{})}
	return f.process, nil
}

func TestProcessBackendPinsAttemptRunsBoundedFixedPlanAndReturnsRedactedResult(t *testing.T) {
	now := time.Now().UTC()
	id := testIdentity()
	checkout := filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, err := workloadsource.CreateArchive(context.Background(), checkout, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	plan := ExecutionPlan{Identity: id, RepositoryPath: filepath.Join(t.TempDir(), "installed-source"), SourceDigest: archive.SHA256(), CommitSHA: strings.Repeat("b", 40),
		CorrelationID: "req_ffffffff-ffff-4fff-8fff-ffffffffffff", Deadline: now.Add(time.Hour)}
	factory := &fakeProcessFactory{output: []byte("tests passed ghp_123456789012345678901234567890\n")}
	backend, err := NewProcessBackend(ProcessConfig{Resolver: fixedExecutionResolver{plan: plan}, Factory: factory, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close(context.Background())
	claims := planTicketClaims("microvm_1", id, OperationStart, plan)
	request := StartRequest{Identity: IdentityFromCommand(id), SourceDigest: plan.SourceDigest, CommitSHA: plan.CommitSHA, Deadline: plan.Deadline}
	if err := backend.Start(context.Background(), claims, request); err != nil {
		t.Fatal(err)
	}
	if err := backend.Start(context.Background(), claims, request); err != nil || factory.starts != 1 {
		t.Fatalf("duplicate start was not idempotent: starts=%d err=%v", factory.starts, err)
	}
	changed := request
	changed.CommitSHA = strings.Repeat("c", 40)
	if err := backend.Start(context.Background(), claims, changed); !errors.Is(err, ErrProtocolRequest) {
		t.Fatalf("changed immutable plan was accepted: %v", err)
	}
	if state, err := backend.Status(context.Background(), TicketClaims{MicroVMID: "microvm_1", Identity: IdentityFromCommand(id), Operation: OperationStatus}); err != nil || state != workloadjournal.Running {
		t.Fatalf("running status = %q, %v", state, err)
	}
	if _, err := backend.Result(context.Background(), TicketClaims{MicroVMID: "microvm_1", Identity: IdentityFromCommand(id), Operation: OperationResult}); !errors.Is(err, ErrAgentBusy) {
		t.Fatalf("unfinished result was returned: %v", err)
	}
	factory.process.finish(0, nil)
	waitForAgentTerminal(t, backend, id)
	result, err := backend.Result(context.Background(), TicketClaims{MicroVMID: "microvm_1", Identity: IdentityFromCommand(id), Operation: OperationResult})
	if err != nil || result.Status != domain.WorkloadSucceeded || result.CorrelationID != plan.CorrelationID || workloadjobs.ValidateResult(result) != nil {
		t.Fatalf("success result = %+v, %v", result, err)
	}
	if result.LogArtifact != nil {
		t.Fatalf("Agent unexpectedly returned a controller-owned log artifact: %+v", result.LogArtifact)
	}
	redactedLogs, err := backend.Logs(context.Background(), TicketClaims{MicroVMID: "microvm_1", Identity: IdentityFromCommand(id), Operation: OperationLogs})
	if err != nil || strings.Contains(string(redactedLogs), "ghp_123") || !strings.Contains(string(redactedLogs), "[REDACTED]") {
		t.Fatalf("Controller-visible logs were not redacted: %q err=%v", redactedLogs, err)
	}
	retried, err := backend.Result(context.Background(), TicketClaims{MicroVMID: "microvm_1", Identity: IdentityFromCommand(id), Operation: OperationResult})
	if err != nil || retried.LogArtifact != nil || retried.Status != result.Status {
		t.Fatalf("retry changed the idempotent result: %+v, %v", retried, err)
	}
}

func TestProcessBackendRecordsInvalidFactoryManifestAsTerminalRunFailure(t *testing.T) {
	now := time.Now().UTC()
	id := testIdentity()
	plan := ExecutionPlan{Identity: id, RepositoryPath: "/workspace/source", SourceDigest: strings.Repeat("a", 64),
		CommitSHA: strings.Repeat("b", 40), TriggerEvent: "push", TriggerBranch: "main", Deadline: now.Add(time.Hour)}
	factory := &fakeProcessFactory{}
	backend, err := NewProcessBackend(ProcessConfig{Resolver: fixedExecutionResolver{plan: plan, err: ErrFactoryManifest},
		Factory: factory, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close(context.Background())
	claims := planTicketClaims("microvm_1", id, OperationStart, plan)
	request := StartRequest{Identity: IdentityFromCommand(id), SourceDigest: plan.SourceDigest, CommitSHA: plan.CommitSHA, Deadline: plan.Deadline}
	if err := backend.Start(context.Background(), claims, request); err != nil {
		t.Fatalf("record manifest failure: %v", err)
	}
	result, err := backend.Result(context.Background(), TicketClaims{MicroVMID: "microvm_1", Identity: IdentityFromCommand(id), Operation: OperationResult})
	if err != nil || result.Status != domain.WorkloadFailed || result.ErrorCode != "factory_manifest_invalid" || factory.starts != 0 {
		t.Fatalf("invalid manifest result = %+v, err=%v, executor starts=%d", result, err, factory.starts)
	}
	logs, err := backend.Logs(context.Background(), TicketClaims{MicroVMID: "microvm_1", Identity: IdentityFromCommand(id), Operation: OperationLogs})
	if err != nil || !strings.Contains(string(logs), "factory manifest rejected") || strings.Contains(string(logs), "secret") {
		t.Fatalf("invalid manifest logs = %q err=%v", logs, err)
	}
}

func TestProcessBackendKeepsAgentTaskInMemoryAndFailsClosedOnTaskDrift(t *testing.T) {
	now := time.Now().UTC()
	id := testIdentity()
	body := "Implement the requested feature"
	task := AgentTask{TenantID: id.TenantID, WorkloadRunID: id.WorkloadRunID, IntegrationID: "integration_1", DeliveryID: "delivery_12345678",
		InstallationID: 71, RepositoryID: 81, IssueNumber: 3, CommentID: 4, DefaultBranch: "main", CommitSHA: strings.Repeat("b", 40),
		Title: "Add feature", Body: body}
	task.ContextSHA256 = AgentTaskContextSHA256(task)
	plan := ExecutionPlan{Identity: id, Kind: domain.WorkloadAgent, RepositoryPath: "/workspace/source", SourceDigest: strings.Repeat("a", 64),
		CommitSHA: task.CommitSHA, TriggerEvent: "issue_comment", TriggerBranch: task.DefaultBranch,
		AgentIntegrationID: task.IntegrationID, AgentInstallationID: task.InstallationID, AgentRepositoryID: task.RepositoryID, Deadline: now.Add(time.Hour)}
	plan.AgentTaskContextSHA256 = task.ContextSHA256
	factory := &fakeProcessFactory{output: []byte("agent process started\n")}
	backend, err := NewProcessBackend(ProcessConfig{Resolver: fixedExecutionResolver{plan: plan}, Factory: factory, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close(context.Background())
	claims := planTicketClaims("microvm_1", id, OperationStart, plan)
	request := StartRequest{Identity: IdentityFromCommand(id), Kind: domain.WorkloadAgent, AgentTask: &task,
		SourceDigest: plan.SourceDigest, CommitSHA: plan.CommitSHA, Deadline: plan.Deadline}
	if err := backend.Start(context.Background(), claims, request); err != nil {
		t.Fatal(err)
	}
	if factory.starts != 1 || factory.plan.AgentTask == nil || *factory.plan.AgentTask != task {
		t.Fatalf("trusted executor did not receive the validated in-memory task: starts=%d task=%+v", factory.starts, factory.plan.AgentTask)
	}
	if err := backend.Start(context.Background(), claims, request); err != nil || factory.starts != 1 {
		t.Fatalf("identical Agent start replay was not idempotent: starts=%d err=%v", factory.starts, err)
	}
	changed := *request.AgentTask
	changed.Body = "different untrusted instruction"
	changed.ContextSHA256 = AgentTaskContextSHA256(changed)
	changedRequest := request
	changedRequest.AgentTask = &changed
	if err := backend.Start(context.Background(), claims, changedRequest); !errors.Is(err, ErrProtocolRequest) || factory.starts != 1 {
		t.Fatalf("changed Agent task replaced the active attempt: starts=%d err=%v", factory.starts, err)
	}
	factory.process.finish(0, nil)
	waitForAgentTerminal(t, backend, id)
	result, err := backend.Result(context.Background(), TicketClaims{MicroVMID: "microvm_1", Identity: IdentityFromCommand(id), Operation: OperationResult})
	logs, logsErr := backend.Logs(context.Background(), TicketClaims{MicroVMID: "microvm_1", Identity: IdentityFromCommand(id), Operation: OperationLogs})
	if err != nil || logsErr != nil || result.Status != domain.WorkloadSucceeded || strings.Contains(string(logs), body) {
		t.Fatalf("Agent result/logs exposed task text or failed: %+v logs=%q err=%v logsErr=%v", result, logs, err, logsErr)
	}
}

func TestGoTestProcessFactoryRejectsAgentRuns(t *testing.T) {
	_, err := (GoTestProcessFactory{}).Start(context.Background(), ExecutionPlan{Kind: domain.WorkloadAgent}, io.Discard)
	if !errors.Is(err, ErrAgentExecutorUnavailable) {
		t.Fatalf("Go test factory accepted an Agent Run: %v", err)
	}
}

func TestProcessBackendWithholdsLogsAtOutputBoundaryAndMapsFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		output   []byte
		limit    int
		exitCode int
		want     domain.WorkloadRunState
	}{
		{name: "truncated output", output: []byte("secret-output-over-boundary"), limit: 8, exitCode: 0, want: domain.WorkloadSucceeded},
		{name: "nonzero exit", output: []byte("safe failure"), limit: 64, exitCode: 2, want: domain.WorkloadFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			id := testIdentity()
			plan := ExecutionPlan{Identity: id, RepositoryPath: "/workspace/checkout", SourceDigest: strings.Repeat("a", 64), CommitSHA: strings.Repeat("b", 40), Deadline: now.Add(time.Hour)}
			factory := &fakeProcessFactory{output: tc.output}
			backend, err := NewProcessBackend(ProcessConfig{Resolver: fixedExecutionResolver{plan: plan}, Factory: factory, MaxOutput: tc.limit})
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close(context.Background())
			claims := planTicketClaims("microvm_1", id, OperationStart, plan)
			request := StartRequest{Identity: IdentityFromCommand(id), SourceDigest: plan.SourceDigest, CommitSHA: plan.CommitSHA, Deadline: plan.Deadline}
			if err := backend.Start(context.Background(), claims, request); err != nil {
				t.Fatal(err)
			}
			factory.process.finish(tc.exitCode, nil)
			waitForAgentTerminal(t, backend, id)
			result, err := backend.Result(context.Background(), TicketClaims{MicroVMID: "microvm_1", Identity: IdentityFromCommand(id), Operation: OperationResult})
			if err != nil || result.Status != tc.want || workloadjobs.ValidateResult(result) != nil {
				t.Fatalf("result = %+v, %v", result, err)
			}
			logs, err := backend.Logs(context.Background(), TicketClaims{MicroVMID: "microvm_1", Identity: IdentityFromCommand(id), Operation: OperationLogs})
			if err != nil {
				t.Fatalf("read controller-visible logs: %v", err)
			}
			if tc.limit < len(tc.output) && (strings.Contains(string(logs), "secret-output-over-boundary") || !strings.Contains(string(logs), "logs withheld")) {
				t.Fatalf("truncated output was not withheld: %q", logs)
			}
		})
	}
}

func TestProcessBackendRejectsPlanMismatchAndShutdownCancelsProcess(t *testing.T) {
	now := time.Now().UTC()
	id := testIdentity()
	plan := ExecutionPlan{Identity: id, RepositoryPath: "/workspace/checkout", SourceDigest: strings.Repeat("a", 64), CommitSHA: strings.Repeat("b", 40), Deadline: now.Add(time.Hour)}
	factory := &fakeProcessFactory{}
	backend, err := NewProcessBackend(ProcessConfig{Resolver: fixedExecutionResolver{plan: plan}, Factory: factory})
	if err != nil {
		t.Fatal(err)
	}
	claims := planTicketClaims("microvm_1", id, OperationStart, plan)
	request := StartRequest{Identity: IdentityFromCommand(id), SourceDigest: strings.Repeat("c", 64), CommitSHA: plan.CommitSHA, Deadline: plan.Deadline}
	if err := backend.Start(context.Background(), claims, request); !errors.Is(err, ErrProtocolRequest) || factory.starts != 0 {
		t.Fatalf("resolver plan mismatch started a process: starts=%d err=%v", factory.starts, err)
	}
	request.SourceDigest = plan.SourceDigest
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := backend.Start(cancelledCtx, claims, request); !errors.Is(err, context.Canceled) || factory.starts != 0 {
		t.Fatalf("cancelled start request was not rejected before process launch: starts=%d err=%v", factory.starts, err)
	}
	if err := backend.Start(context.Background(), claims, request); err != nil {
		t.Fatal(err)
	}
	if err := backend.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := backend.Result(context.Background(), TicketClaims{MicroVMID: "microvm_1", Identity: IdentityFromCommand(id), Operation: OperationResult})
	if err != nil || result.Status != domain.WorkloadCancelled || result.ErrorCode != "cancelled" {
		t.Fatalf("shutdown did not cancel the process: %+v, %v", result, err)
	}
	if err := backend.Start(context.Background(), claims, request); !errors.Is(err, ErrAgentBusy) {
		t.Fatalf("closed backend accepted new process: %v", err)
	}
}

func TestGoTestProcessFactoryVerifiesCommitAndRunsFixedCommand(t *testing.T) {
	repository := t.TempDir()
	for name, contents := range map[string]string{
		"go.mod":         "module example.test/hako-agent-fixture\n\ngo 1.20\n",
		"sample_test.go": "package sample\n\nimport \"testing\"\n\nfunc TestFixedGoTest(t *testing.T) {}\n",
	} {
		if err := os.WriteFile(filepath.Join(repository, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is required for the immutable checkout smoke test")
	}
	runGit := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(git, append([]string{"-C", repository}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + repository, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
		cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=Hako Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Hako Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, output)
		}
		return output
	}
	runGit("init", "-q")
	runGit("add", "go.mod", "sample_test.go")
	runGit("commit", "-qm", "fixture")
	commit := strings.TrimSpace(string(runGit("rev-parse", "--verify", "HEAD")))
	goBinary := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(goBinary); err != nil {
		t.Skip("Go toolchain binary is unavailable")
	}
	archive, err := workloadsource.CreateArchive(context.Background(), repository, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	checkout := filepath.Join(t.TempDir(), "installed-source")
	installer, err := NewFilesystemSourceInstaller(checkout, 0)
	if err != nil {
		t.Fatal(err)
	}
	id := testIdentity()
	now := time.Now().UTC()
	plan := ExecutionPlan{Identity: id, RepositoryPath: checkout, SourceDigest: archive.SHA256(),
		CommitSHA: commit, Deadline: now.Add(30 * time.Second)}
	claims := planTicketClaims("microvm_1", id, OperationSource, plan)
	if err := installer.Install(context.Background(), claims, archive.SHA256(), commit, archive.Reader(), archive.Size()); err != nil {
		t.Fatal(err)
	}
	factory := GoTestProcessFactory{GoBinary: goBinary, GoCache: filepath.Join(t.TempDir(), "gocache")}
	output := &boundedOutput{limit: 64 << 10}
	ctx, cancel := context.WithDeadline(context.Background(), plan.Deadline)
	defer cancel()
	process, err := factory.Start(ctx, plan, output)
	if err != nil {
		t.Fatal(err)
	}
	code, err := process.Wait()
	logs, _ := output.snapshot()
	if err != nil || code != 0 || !strings.Contains(string(logs), "ok  ") || !strings.Contains(string(logs), "example.test/hako-agent-fixture") {
		t.Fatalf("fixed go test command failed: code=%d err=%v output=%q", code, err, logs)
	}
	nativePlan := plan
	nativePlan.TriggerEvent, nativePlan.TriggerBranch = "push", "main"
	nativePlan.FactoryPipelines = []factoryconfig.ExecutionPlan{{Pipeline: "verify", RuntimeClass: "standard", TimeoutSecond: 30,
		MemoryMB: 2048, CPUMillis: 1000, Network: "registry-only",
		Commands: []factoryconfig.Command{{Step: "go-test", Executable: "/usr/local/go/bin/go", Args: []string{"test", "./..."}}}}}
	nativeFactory := GoTestProcessFactory{GoBinary: goBinary, GoCache: filepath.Join(t.TempDir(), "native-gocache")}
	nativeOutput := &boundedOutput{limit: 64 << 10}
	nativeCtx, nativeCancel := context.WithTimeout(context.Background(), time.Minute)
	nativeProcess, err := nativeFactory.Start(nativeCtx, nativePlan, nativeOutput)
	if err != nil {
		nativeCancel()
		t.Fatal(err)
	}
	nativeCode, nativeErr := nativeProcess.Wait()
	nativeCancel()
	nativeLogs, _ := nativeOutput.snapshot()
	if nativeErr != nil || nativeCode != 0 || !strings.Contains(string(nativeLogs), "pipeline verify") || !strings.Contains(string(nativeLogs), "step go-test") {
		t.Fatalf("native manifest pipeline failed: code=%d err=%v output=%q", nativeCode, nativeErr, nativeLogs)
	}
	pipelineResultProvider, ok := nativeProcess.(interface {
		PipelineResults() []workloadjobs.WorkloadPipelineResult
	})
	if !ok {
		t.Fatal("native pipeline process does not expose pipeline results")
	}
	pipelineResults := pipelineResultProvider.PipelineResults()
	if len(pipelineResults) != 1 || pipelineResults[0].Name != "verify" || pipelineResults[0].Status != "succeeded" {
		t.Fatalf("native pipeline results = %+v", pipelineResults)
	}

	// A pipeline must stop when its execution context expires, even when its
	// configured per-pipeline timeout is longer. This covers the runtime
	// enforcement path without making the test wait for the manifest's 30s
	// minimum timeout.
	timeoutPlan := nativePlan
	timeoutPlan.FactoryPipelines = append([]factoryconfig.ExecutionPlan(nil), nativePlan.FactoryPipelines...)
	timeoutPlan.FactoryPipelines[0].Commands = []factoryconfig.Command{{
		Step: "go-test", Executable: "/usr/local/go/bin/go", Args: []string{"test", "./..."},
	}}
	hangingGo := filepath.Join(t.TempDir(), "go-wrapper")
	if err := os.WriteFile(hangingGo, []byte("#!/bin/sh\nsleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	timeoutFactory := GoTestProcessFactory{GoBinary: hangingGo, GoCache: filepath.Join(t.TempDir(), "timeout-gocache")}
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	timeoutProcess, err := timeoutFactory.Start(timeoutCtx, timeoutPlan, io.Discard)
	if err != nil {
		timeoutCancel()
		t.Fatalf("start expired native pipeline: %v", err)
	}
	timeoutCode, timeoutErr := timeoutProcess.Wait()
	timeoutCancel()
	timeoutResultProvider, ok := timeoutProcess.(interface {
		PipelineResults() []workloadjobs.WorkloadPipelineResult
	})
	if !ok {
		t.Fatal("timed-out native pipeline does not expose pipeline results")
	}
	timeoutResults := timeoutResultProvider.PipelineResults()
	if timeoutCode != 124 || !errors.Is(timeoutErr, context.DeadlineExceeded) || len(timeoutResults) != 1 ||
		timeoutResults[0].Status != "timed_out" || timeoutResults[0].ErrorCode != "pipeline_timeout" {
		t.Fatalf("expired native pipeline = code %d, err %v, results %+v", timeoutCode, timeoutErr, timeoutResults)
	}

	cancelRoot := t.TempDir()
	readyPath := filepath.Join(cancelRoot, "started")
	hangingGo = filepath.Join(cancelRoot, "go-wrapper")
	if err := os.WriteFile(hangingGo, []byte("#!/bin/sh\n: > \""+readyPath+"\"\nsleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cancelPlan := nativePlan
	cancelPlan.FactoryPipelines = append([]factoryconfig.ExecutionPlan(nil), nativePlan.FactoryPipelines...)
	cancelPlan.FactoryPipelines = append(cancelPlan.FactoryPipelines, factoryconfig.ExecutionPlan{
		Pipeline: "verify-next", RuntimeClass: "standard", TimeoutSecond: 30, MemoryMB: 2048, CPUMillis: 1000,
		Network: "registry-only", Commands: []factoryconfig.Command{{Step: "go-vet", Executable: "/usr/local/go/bin/go", Args: []string{"vet", "./..."}}},
	})
	cancelFactory := GoTestProcessFactory{GoBinary: hangingGo, GoCache: filepath.Join(cancelRoot, "cancel-gocache")}
	cancelCtx, cancelRun := context.WithCancel(context.Background())
	cancelProcess, err := cancelFactory.Start(cancelCtx, cancelPlan, io.Discard)
	if err != nil {
		cancelRun()
		t.Fatalf("start cancellable native pipeline: %v", err)
	}
	readyDeadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if time.Now().After(readyDeadline) {
			cancelRun()
			_, _ = cancelProcess.Wait()
			t.Fatal("native pipeline command did not start before cancellation test deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancelRun()
	cancelCode, cancelErr := cancelProcess.Wait()
	cancelResultProvider, ok := cancelProcess.(interface {
		PipelineResults() []workloadjobs.WorkloadPipelineResult
	})
	if !ok {
		t.Fatal("cancelled native pipeline does not expose pipeline results")
	}
	cancelResults := cancelResultProvider.PipelineResults()
	if cancelCode != 130 || !errors.Is(cancelErr, context.Canceled) || len(cancelResults) != 2 ||
		cancelResults[0].Name != "verify" || cancelResults[0].Status != "cancelled" || cancelResults[0].ErrorCode != "cancelled" ||
		cancelResults[1].Name != "verify-next" || cancelResults[1].Status != "skipped" {
		t.Fatalf("cancelled native pipeline = code %d, err %v, results %+v", cancelCode, cancelErr, cancelResults)
	}

	failingGo := filepath.Join(t.TempDir(), "go-wrapper")
	if err := os.WriteFile(failingGo, []byte("#!/bin/sh\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	failureFactory := GoTestProcessFactory{GoBinary: failingGo, GoCache: filepath.Join(t.TempDir(), "failure-gocache")}
	failureProcess, err := failureFactory.Start(context.Background(), cancelPlan, io.Discard)
	if err != nil {
		t.Fatalf("start failing native pipeline: %v", err)
	}
	failureCode, failureErr := failureProcess.Wait()
	failureResultProvider, ok := failureProcess.(interface {
		PipelineResults() []workloadjobs.WorkloadPipelineResult
	})
	if !ok {
		t.Fatal("failed native pipeline does not expose pipeline results")
	}
	failureResults := failureResultProvider.PipelineResults()
	if failureCode != 7 || failureErr == nil || len(failureResults) != 2 ||
		failureResults[0].Name != "verify" || failureResults[0].Status != "failed" || failureResults[0].ErrorCode != "step_failed" ||
		failureResults[1].Name != "verify-next" || failureResults[1].Status != "skipped" {
		t.Fatalf("failed native pipeline = code %d, err %v, results %+v", failureCode, failureErr, failureResults)
	}

	plan.CommitSHA = strings.Repeat("f", 40)
	if _, err := factory.Start(ctx, plan, io.Discard); !errors.Is(err, ErrExecutionPlan) {
		t.Fatalf("mismatched installed commit accepted: %v", err)
	}
	plan.CommitSHA = commit
	if err := os.WriteFile(filepath.Join(checkout, "sample_test.go"), []byte("package sample\n\nimport \"testing\"\n\nfunc TestTampered(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := factory.Start(ctx, plan, io.Discard); !errors.Is(err, ErrExecutionPlan) {
		t.Fatalf("modified extracted source was accepted: %v", err)
	}
}

func TestPipelineContextExitKeepsTimeoutAndCancellationDistinct(t *testing.T) {
	tests := []struct {
		name       string
		cause      error
		wantCode   int
		wantStatus string
		wantError  string
	}{
		{name: "deadline", cause: context.DeadlineExceeded, wantCode: 124, wantStatus: "timed_out", wantError: "pipeline_timeout"},
		{name: "cancel", cause: context.Canceled, wantCode: 130, wantStatus: "cancelled", wantError: "cancelled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			exit, result, ok := pipelineContextExit("verify", test.cause)
			if !ok || exit.code != test.wantCode || !errors.Is(exit.err, test.cause) ||
				result.Name != "verify" || result.Status != test.wantStatus || result.ExitCode != test.wantCode || result.ErrorCode != test.wantError {
				t.Fatalf("pipelineContextExit() = (%+v, %+v, %t)", exit, result, ok)
			}
		})
	}
	if _, _, ok := pipelineContextExit("verify", errors.New("other failure")); ok {
		t.Fatal("ordinary command failure was treated as a context termination")
	}
}

func TestProtocolServerUsesProcessBackendEndToEnd(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	id := testIdentity()
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, err := workloadsource.CreateArchive(context.Background(), sourceRoot, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	plan := ExecutionPlan{Identity: id, RepositoryPath: filepath.Join(t.TempDir(), "checkout"), SourceDigest: archive.SHA256(), CommitSHA: strings.Repeat("b", 40),
		CorrelationID: "req_eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", Deadline: now.Add(time.Hour)}
	factory := &fakeProcessFactory{output: []byte("all package tests passed\n")}
	agentRuntime, err := NewFilesystemAgent(FilesystemAgentConfig{SourceDirectory: plan.RepositoryPath,
		TrustedKeys: map[string]ed25519.PublicKey{"rp-v1": publicKey}, ProcessFactory: factory, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer agentRuntime.Close(context.Background())
	client := NewClient(handlerRoundTripper{agentRuntime.Handler()})
	issuer := &Ed25519TicketIssuer{KeyID: "rp-v1", PrivateKey: privateKey}
	issue := func(op Operation) string {
		t.Helper()
		var ticket string
		var err error
		if op == OperationStart || op == OperationSource {
			ticket, err = issuer.IssueExecution("microvm_1", id, op, ExecutionBinding{SourceDigest: plan.SourceDigest, CommitSHA: plan.CommitSHA,
				CorrelationID: plan.CorrelationID, Deadline: plan.Deadline.UTC()})
		} else {
			ticket, err = issuer.Issue("microvm_1", id, op)
		}
		if err != nil {
			t.Fatal(err)
		}
		return ticket
	}
	endpoint := "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws"
	request := StartRequest{Identity: IdentityFromCommand(id), SourceDigest: plan.SourceDigest, CommitSHA: plan.CommitSHA, Deadline: plan.Deadline}
	if err := client.UploadSource(context.Background(), endpoint, "aws-edge-token", issue(OperationSource), id, archive.SHA256(), plan.CommitSHA, archive.Size(), archive.Reader()); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background(), endpoint, "aws-edge-token", issue(OperationStart), request); err != nil {
		t.Fatal(err)
	}
	factory.process.finish(0, nil)
	waitForAgentTerminal(t, agentRuntime.backend, id)
	state, err := client.Status(context.Background(), endpoint, "aws-edge-token", issue(OperationStatus), id)
	if err != nil || state != workloadjournal.Succeeded {
		t.Fatalf("protocol status = %q, %v", state, err)
	}
	result, err := client.Result(context.Background(), endpoint, "aws-edge-token", issue(OperationResult), id)
	if err != nil || result.Status != domain.WorkloadSucceeded || result.CorrelationID != plan.CorrelationID || result.LogArtifact != nil {
		t.Fatalf("protocol result = %+v, %v", result, err)
	}
	logs, digest, err := client.Logs(context.Background(), endpoint, "aws-edge-token", issue(OperationLogs), id)
	if err != nil || !strings.Contains(string(logs), "all package tests passed") || digest == "" {
		t.Fatalf("redacted Agent logs = %q digest=%q err=%v", logs, digest, err)
	}
}

func waitForAgentTerminal(t *testing.T, backend *ProcessBackend, id workloadjournal.Command) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		state, err := backend.Status(context.Background(), TicketClaims{MicroVMID: "microvm_1", Identity: IdentityFromCommand(id), Operation: OperationStatus})
		if err == nil && state != workloadjournal.Running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("agent process did not reach a terminal state")
}
