package actionsrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunnerProcessConsumerUsesOneShotConfigAndPassesModelKeyOnlyToRunner(t *testing.T) {
	root := t.TempDir()
	writeRunnerScript(t, filepath.Join(root, "config.sh"), "#!/bin/sh\nprintf '%s' \"$*\" > config.args\ntouch .credentials\n")
	writeRunnerScript(t, filepath.Join(root, "run.sh"), "#!/bin/sh\nenv > runner.env\nmkdir -p _work/repo _temp _diag\nprintf source > _work/repo/file\nprintf '%s\\n' 'registration_token=registration-token-synthetic' 'ANTHROPIC_API_KEY=sk-ant-synthetic-secret' 'Authorization: Bearer ghp_abcdefghijklmnopqrstuvwxyz1234567890' > _diag/Worker_1.log\n")
	exited := make(chan RunnerProcessResult, 1)
	consumer, err := NewRunnerProcessConsumer(RunnerProcessConfig{RootDirectory: root, SetupTimeout: time.Second, MaximumRun: time.Minute,
		OnExit: func(result RunnerProcessResult) { exited <- result }})
	if err != nil {
		t.Fatal(err)
	}
	claims := validRunnerProcessClaims()
	claims.ModelCredential = []byte("sk-ant-synthetic-secret")
	if err := consumer.Consume(context.Background(), claims); err != nil {
		t.Fatalf("Consume() failed: %v", err)
	}
	result := awaitRunnerProcessResult(t, exited)
	if result.IntegrationID != claims.IntegrationID || result.WorkflowJobID != claims.WorkflowJobID || result.WorkflowRunID != claims.WorkflowRunID ||
		result.RunAttempt != claims.RunAttempt || result.ExitCode != 0 || result.TimedOut {
		t.Fatalf("unexpected runner result: %+v", result)
	}
	if result.DiagnosticsTruncated || len(result.Diagnostics) == 0 || result.DiagnosticsSHA256 == "" {
		t.Fatalf("runner diagnostics were not collected with a digest: %+v", result)
	}
	for _, secret := range []string{"registration-token-synthetic", "sk-ant-synthetic-secret", "ghp_abcdefghijklmnopqrstuvwxyz1234567890"} {
		if strings.Contains(string(result.Diagnostics), secret) {
			t.Fatalf("runner diagnostics contain secret %q", secret)
		}
	}
	if !strings.Contains(string(result.Diagnostics), "[REDACTED]") || !strings.Contains(result.DiagnosticSummary(1024), "[runner diagnostic file]") {
		t.Fatalf("redacted diagnostics summary missing expected content: %q", result.Diagnostics)
	}
	digest := sha256.Sum256(result.Diagnostics)
	if result.DiagnosticsSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("runner diagnostic digest does not match the collected content")
	}
	configArgs, err := os.ReadFile(filepath.Join(root, "config.args"))
	if err != nil {
		t.Fatal(err)
	}
	args := string(configArgs)
	for _, expected := range []string{"--unattended", "--disableupdate", "--ephemeral", "--url https://github.com/acme/api", "--token registration-token-synthetic", "--name hako-420-2", "--labels self-hosted,hako,arm64"} {
		if !strings.Contains(args, expected) {
			t.Errorf("config args missing %q: %s", expected, args)
		}
	}
	if strings.Contains(args, "sk-ant-synthetic-secret") {
		t.Fatal("model credential leaked to config.sh arguments")
	}
	runnerEnv, err := os.ReadFile(filepath.Join(root, "runner.env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runnerEnv), "ANTHROPIC_API_KEY=sk-ant-synthetic-secret") || strings.Contains(string(runnerEnv), "AWS_ACCESS_KEY_ID=") {
		t.Fatalf("runner environment lacks scoped key or contains cloud credentials: %s", runnerEnv)
	}
	for _, path := range []string{"_work", "_temp", "_diag", ".credentials"} {
		if _, err := os.Lstat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Errorf("runner local state %q was not removed after completion", path)
		}
	}
	clear(claims.ModelCredential)
	if err := consumer.Consume(context.Background(), validRunnerProcessClaims()); !errors.Is(err, ErrRunnerProcess) {
		t.Fatal("second runner job reused a one-shot process consumer")
	}
}

func TestRunnerProcessConsumerStopTerminatesProcessGroup(t *testing.T) {
	root := t.TempDir()
	writeRunnerScript(t, filepath.Join(root, "config.sh"), "#!/bin/sh\nexit 0\n")
	writeRunnerScript(t, filepath.Join(root, "run.sh"), "#!/bin/sh\nsleep 60 &\nwait\n")
	exited := make(chan RunnerProcessResult, 1)
	consumer, err := NewRunnerProcessConsumer(RunnerProcessConfig{RootDirectory: root, SetupTimeout: time.Second, MaximumRun: time.Minute,
		OnExit: func(result RunnerProcessResult) { exited <- result }})
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.Consume(context.Background(), validRunnerProcessClaims()); err != nil {
		t.Fatal(err)
	}
	consumer.Stop()
	result := awaitRunnerProcessResult(t, exited)
	if result.ExitCode == 0 || result.TimedOut || !result.Cancelled || result.FailurePhase != "run" {
		t.Fatalf("stopped process returned success or timeout: %+v", result)
	}
}

func TestRunnerProcessConsumerReportsRegistrationFailureAndCleansState(t *testing.T) {
	root := t.TempDir()
	writeRunnerScript(t, filepath.Join(root, "config.sh"), "#!/bin/sh\nmkdir -p _diag\ntouch .credentials\nexit 1\n")
	writeRunnerScript(t, filepath.Join(root, "run.sh"), "#!/bin/sh\nexit 0\n")
	exited := make(chan RunnerProcessResult, 1)
	consumer, err := NewRunnerProcessConsumer(RunnerProcessConfig{RootDirectory: root, SetupTimeout: time.Second, MaximumRun: time.Minute,
		OnExit: func(result RunnerProcessResult) { exited <- result }})
	if err != nil {
		t.Fatal(err)
	}
	claims := validRunnerProcessClaims()
	if err := consumer.Consume(context.Background(), claims); !errors.Is(err, ErrRunnerProcess) {
		t.Fatalf("failed runner registration returned %v", err)
	}
	result := awaitRunnerProcessResult(t, exited)
	if result.IntegrationID != claims.IntegrationID || result.WorkflowJobID != claims.WorkflowJobID || result.WorkflowRunID != claims.WorkflowRunID ||
		result.RunAttempt != claims.RunAttempt || result.ExitCode != -1 || result.FailurePhase != "configure" || result.CleanupFailed {
		t.Fatalf("unexpected failed registration result: %+v", result)
	}
	for _, path := range []string{"_diag", ".credentials"} {
		if _, err := os.Lstat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Errorf("runner registration failure retained %q", path)
		}
	}
}

func TestRunnerProcessConsumerRejectsInvalidClaimsBeforeRunnerRegistration(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "configured")
	writeRunnerScript(t, filepath.Join(root, "config.sh"), "#!/bin/sh\ntouch "+marker+"\n")
	writeRunnerScript(t, filepath.Join(root, "run.sh"), "#!/bin/sh\nexit 0\n")
	consumer, err := NewRunnerProcessConsumer(RunnerProcessConfig{RootDirectory: root})
	if err != nil {
		t.Fatal(err)
	}
	claims := validRunnerProcessClaims()
	claims.RepositoryURL = "https://attacker.invalid/acme/api"
	if err := consumer.Consume(context.Background(), claims); !errors.Is(err, ErrRunnerProcess) {
		t.Fatalf("foreign repository accepted: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("invalid identity reached config.sh")
	}
}

func TestRunnerDiagnosticCollectorBoundsFilesAndDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	writeRunnerScript(t, filepath.Join(root, "config.sh"), "#!/bin/sh\nexit 0\n")
	writeRunnerScript(t, filepath.Join(root, "run.sh"), "#!/bin/sh\nexit 0\n")
	consumer, err := NewRunnerProcessConsumer(RunnerProcessConfig{RootDirectory: root})
	if err != nil {
		t.Fatal(err)
	}
	diagnosticsDir := filepath.Join(root, "_diag")
	if err := os.MkdirAll(diagnosticsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(diagnosticsDir, "a.log"), []byte(strings.Repeat("x", maxRunnerDiagnosticBytes+1024)), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.log")
	if err := os.WriteFile(outside, []byte("outside-secret-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(diagnosticsDir, "b.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(diagnosticsDir, "c.log"), []byte("safe diagnostic"), 0o600); err != nil {
		t.Fatal(err)
	}
	collected := consumer.collectDiagnostics([]byte("outside-secret-value"))
	if len(collected.content) > maxRunnerDiagnosticBytes || !collected.truncated {
		t.Fatalf("diagnostics were not bounded: bytes=%d truncated=%t", len(collected.content), collected.truncated)
	}
	if strings.Contains(string(collected.content), "outside-secret-value") {
		t.Fatal("diagnostic collector followed a symlink or skipped exact-secret redaction")
	}
	if !strings.Contains(string(collected.content), "safe diagnostic") {
		t.Fatal("diagnostic collector discarded a bounded regular log after skipping an oversized file")
	}
	result := RunnerProcessResult{Diagnostics: collected.content, DiagnosticsTruncated: collected.truncated}
	if strings.Contains(result.String(), string(result.Diagnostics)) || strings.Contains(result.GoString(), string(result.Diagnostics)) {
		t.Fatal("runner process diagnostics are exposed by default formatting")
	}
}

func validRunnerProcessClaims() RunnerBootstrapClaims {
	return RunnerBootstrapClaims{SchemaVersion: 1, MicroVMID: "mvm-runner-1", IntegrationID: "integration_runner", WorkflowJobID: 420,
		WorkflowRunID: 430, RunAttempt: 2, RepositoryURL: "https://github.com/acme/api", RunnerName: "hako-420-2",
		Labels: []string{"self-hosted", "hako", "arm64"}, Token: "registration-token-synthetic", ExpiresAt: time.Now().Add(time.Minute).Unix()}
}

func writeRunnerScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func awaitRunnerProcessResult(t *testing.T, results <-chan RunnerProcessResult) RunnerProcessResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ephemeral runner process result")
		return RunnerProcessResult{}
	}
}
