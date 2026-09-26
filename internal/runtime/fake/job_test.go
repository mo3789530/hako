package fake

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

const testCommitSHA = "0123456789abcdef0123456789abcdef01234567"

type recordingJobRunner struct {
	commit   string
	jobErr   error
	jobLogs  string
	commands []recordedCommand
}

type recordedCommand struct {
	name string
	args []string
}

func (r *recordingJobRunner) Run(_ context.Context, name string, args []string, output io.Writer) error {
	r.commands = append(r.commands, recordedCommand{name: name, args: append([]string(nil), args...)})
	if name == "git" {
		_, _ = io.WriteString(output, r.commit+"\n")
		return nil
	}
	_, _ = io.WriteString(output, r.jobLogs)
	return r.jobErr
}

func newTestJobRuntime(t *testing.T, runner CommandRunner, maxLogBytes int) *JobRuntime {
	t.Helper()
	runtime, err := NewJobRuntime(JobRuntimeConfig{
		PodmanPath:    "/usr/bin/podman",
		GoImage:       "docker.io/library/golang:1.27-alpine",
		MaxLogBytes:   maxLogBytes,
		CommandRunner: runner,
	})
	if err != nil {
		t.Fatalf("NewJobRuntime() error = %v", err)
	}
	return runtime
}

func testJobSpec(t *testing.T, runID string) JobSpec {
	t.Helper()
	repository := t.TempDir()
	if err := os.Mkdir(filepath.Join(repository, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	return JobSpec{
		TenantID:       domain.TenantID("tenant-1"),
		RunID:          domain.WorkloadRunID(runID),
		RepositoryPath: repository,
		CommitSHA:      testCommitSHA,
		Timeout:        time.Minute,
		SecretValues:   []string{"private-test-secret"},
	}
}

func TestRunGoTestIsolatedAndCachesRedactedResult(t *testing.T) {
	const rawLogs = "ghp_abcdefghijklmnopqrstuvwxyz1234567890 github_pat_abcdefghijklmnopqrstuvwxyz1234567890 AKIA1234567890ABCDEF " +
		"Bearer abc.def.ghi eyJabcdefgh.abcdefgh.abcdefgh private-test-secret"
	runner := &recordingJobRunner{commit: testCommitSHA, jobLogs: rawLogs}
	runtime := newTestJobRuntime(t, runner, 1024)
	spec := testJobSpec(t, "run-1")

	result, err := runtime.RunGoTest(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunGoTest() error = %v", err)
	}
	if result.ExitCode != 0 || result.TimedOut || result.LogTruncated {
		t.Fatalf("unexpected result: %+v", result)
	}
	if strings.Contains(string(result.RedactedLogs), "abcdefghijklmnopqrstuvwxyz") || strings.Contains(string(result.RedactedLogs), "private-test-secret") {
		t.Fatalf("result contains an unredacted secret: %s", result.RedactedLogs)
	}
	if strings.Count(string(result.RedactedLogs), "[REDACTED]") != 6 {
		t.Fatalf("expected all six secrets to be redacted, got %q", result.RedactedLogs)
	}
	if len(runner.commands) != 2 || runner.commands[0].name != "git" || runner.commands[1].name != "/usr/bin/podman" {
		t.Fatalf("unexpected commands: %+v", runner.commands)
	}
	args := strings.Join(runner.commands[1].args, " ")
	for _, required := range []string{"--network=none", "--read-only", "--cap-drop=ALL", "no-new-privileges", "--user=65532:65532", "ro=true", "go test ./..."} {
		if !strings.Contains(args, required) {
			t.Errorf("Podman arguments missing %q: %s", required, args)
		}
	}
	if strings.Contains(args, "noexec") {
		t.Fatal("Go test binaries need an executable temporary directory")
	}
	for _, forbidden := range []string{"private-test-secret", "AWS_ACCESS_KEY_ID", "GITHUB_TOKEN", "--privileged"} {
		if strings.Contains(args, forbidden) {
			t.Errorf("Podman arguments contain forbidden value %q: %s", forbidden, args)
		}
	}

	// The result remains available after execution, and a duplicate does not
	// execute a second container.
	again, ok := runtime.Result(spec.TenantID, spec.RunID)
	if !ok || again.LogsSHA256 != result.LogsSHA256 {
		t.Fatalf("Result() = (%+v, %v), want original result", again, ok)
	}
	if _, err := runtime.RunGoTest(context.Background(), spec); err != nil {
		t.Fatalf("idempotent RunGoTest() error = %v", err)
	}
	if len(runner.commands) != 2 {
		t.Fatalf("idempotent retry executed commands: %+v", runner.commands)
	}
	if _, ok := runtime.Result(domain.TenantID("another-tenant"), spec.RunID); ok {
		t.Fatal("Result() exposed another Tenant's run")
	}
}

func TestRunGoTestRejectsCommitMismatchBeforeContainer(t *testing.T) {
	runner := &recordingJobRunner{commit: strings.Repeat("f", 40)}
	runtime := newTestJobRuntime(t, runner, 1024)
	_, err := runtime.RunGoTest(context.Background(), testJobSpec(t, "run-1"))
	if !errors.Is(err, ErrCommitMismatch) {
		t.Fatalf("RunGoTest() error = %v, want ErrCommitMismatch", err)
	}
	if len(runner.commands) != 1 || runner.commands[0].name != "git" {
		t.Fatalf("container command ran before commit verification: %+v", runner.commands)
	}
}

func TestRunGoTestCapturesFailureAndTruncatesLogs(t *testing.T) {
	runner := &recordingJobRunner{
		commit:  testCommitSHA,
		jobLogs: "0123456789-more-output",
		jobErr:  &CommandExitError{Code: 17},
	}
	runtime := newTestJobRuntime(t, runner, 10)
	result, err := runtime.RunGoTest(context.Background(), testJobSpec(t, "run-1"))
	if err != nil {
		t.Fatalf("RunGoTest() error = %v", err)
	}
	if result.ExitCode != 17 || !result.LogTruncated || string(result.RedactedLogs) != "0123456789" {
		t.Fatalf("unexpected failed/truncated result: %+v logs=%q", result, result.RedactedLogs)
	}
}

func TestRunGoTestRejectsRunIDReuseAcrossTenantOrCommit(t *testing.T) {
	runner := &recordingJobRunner{commit: testCommitSHA}
	runtime := newTestJobRuntime(t, runner, 1024)
	spec := testJobSpec(t, "run-1")
	if _, err := runtime.RunGoTest(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	otherTenant := spec
	otherTenant.TenantID = "tenant-2"
	if _, err := runtime.RunGoTest(context.Background(), otherTenant); !errors.Is(err, ErrJobRunConflict) {
		t.Fatalf("different Tenant error = %v, want ErrJobRunConflict", err)
	}
	otherCommit := spec
	otherCommit.CommitSHA = strings.Repeat("a", 40)
	if _, err := runtime.RunGoTest(context.Background(), otherCommit); !errors.Is(err, ErrJobRunConflict) {
		t.Fatalf("different commit error = %v, want ErrJobRunConflict", err)
	}
}

func TestNewJobRuntimeValidatesImageAndLogLimit(t *testing.T) {
	for _, config := range []JobRuntimeConfig{
		{GoImage: "--privileged"},
		{GoImage: "golang:latest", MaxLogBytes: maxJobLogLimit + 1},
	} {
		if _, err := NewJobRuntime(config); err == nil {
			t.Errorf("NewJobRuntime(%+v) unexpectedly succeeded", config)
		}
	}
}
