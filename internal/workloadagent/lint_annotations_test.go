package workloadagent

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mo3789530/hako/internal/factoryconfig"
	"github.com/mo3789530/hako/internal/workloadjobs"
)

func TestExtractLintAnnotationsAcceptsOnlySafeRepositoryDiagnostics(t *testing.T) {
	root := t.TempDir()
	secret := "synthetic-lint-secret"
	output := strings.Join([]string{
		"internal/api/handler.go:42:7: found " + secret,
		"../outside.go:9: unsafe path",
		"/tmp/outside.go:3:4: outside repository",
		"internal/api/handler.go:0: invalid line",
		"internal/api/handler.go:43: valid second diagnostic",
		"not a diagnostic",
	}, "\n")
	annotations := extractLintAnnotations(root, []byte(output), []string{secret})
	if len(annotations) != 2 {
		t.Fatalf("expected two safe annotations, got %+v", annotations)
	}
	if annotations[0].Path != "internal/api/handler.go" || annotations[0].StartLine != 42 || annotations[0].StartColumn != 7 ||
		strings.Contains(annotations[0].Message, secret) || !strings.Contains(annotations[0].Message, "[REDACTED]") {
		t.Fatalf("diagnostic identity or secret redaction failed: %+v", annotations[0])
	}
	if annotations[1].Path != "internal/api/handler.go" || annotations[1].StartLine != 43 {
		t.Fatalf("line-only diagnostic was not accepted: %+v", annotations[1])
	}
}

func TestExtractLintAnnotationsBoundsCountAndRejectsTruncatedCapture(t *testing.T) {
	root := t.TempDir()
	var output strings.Builder
	for i := 1; i <= 80; i++ {
		output.WriteString("internal/file.go:")
		output.WriteString(strconv.Itoa(i))
		output.WriteString(": diagnostic\n")
	}
	annotations := extractLintAnnotations(root, []byte(output.String()), nil)
	if len(annotations) != 50 {
		t.Fatalf("annotation limit not applied: got %d", len(annotations))
	}
	if got := extractLintAnnotations(root, make([]byte, maxLintDiagnosticOutput+1), nil); len(got) != 0 {
		t.Fatalf("oversized output produced annotations: %+v", got)
	}
	if got := extractLintAnnotations(root, []byte("internal/file.go:1: diagnostic"), []string{"diagnostic"}); len(got) != 0 {
		t.Fatalf("fully redacted message produced an annotation: %+v", got)
	}
}

func TestExtractLintAnnotationsRespectsEncodedResultByteLimit(t *testing.T) {
	root := t.TempDir()
	message := strings.Repeat("diagnostic ", 90)
	var output strings.Builder
	for i := 1; i <= 50; i++ {
		output.WriteString("internal/file.go:")
		output.WriteString(strconv.Itoa(i))
		output.WriteString(": ")
		output.WriteString(message)
		output.WriteByte('\n')
	}
	annotations := extractLintAnnotations(root, []byte(output.String()), nil)
	encoded, err := json.Marshal(annotations)
	if err != nil {
		t.Fatal(err)
	}
	if len(annotations) == 0 || len(annotations) > 50 || len(encoded) > maxLintAnnotationJSON {
		t.Fatalf("annotation result exceeded downstream bound: count=%d bytes=%d", len(annotations), len(encoded))
	}
}

func TestFactoryPipelineProducesRedactedVetAnnotations(t *testing.T) {
	secret := "synthetic-postgres-password"
	command := filepath.Join(t.TempDir(), "go-vet")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf '%s\\n' './internal/api.go:12:5: leaked "+secret+"'\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	session := &fixedJobServiceSession{environment: []string{"PGPASSWORD=" + secret}, secrets: []string{secret}}
	plan := ExecutionPlan{Identity: testIdentity(), RepositoryPath: root, FactoryPipelines: []factoryconfig.ExecutionPlan{{
		Pipeline: "verify", RuntimeClass: "standard", TimeoutSecond: 30, MemoryMB: 2048, CPUMillis: 1000,
		Network: "registry-only", Services: []string{"postgres"}, Commands: []factoryconfig.Command{{
			Step: "go-vet", Executable: "/usr/local/go/bin/go", Args: []string{"vet", "./..."},
		}},
	}}}
	process, err := (GoTestProcessFactory{GoBinary: command, JobServices: fixedJobServiceProvisioner{session: session}}).
		startFactoryPipelines(context.Background(), plan, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := process.Wait(); code != 1 || err == nil {
		t.Fatalf("expected vet failure, got code=%d err=%v", code, err)
	}
	results := process.(interface {
		PipelineResults() []workloadjobs.WorkloadPipelineResult
	}).PipelineResults()
	if len(results) != 1 || len(results[0].Annotations) != 1 {
		t.Fatalf("expected one lint annotation, got %+v", results)
	}
	annotation := results[0].Annotations[0]
	if annotation.Path != "internal/api.go" || annotation.StartLine != 12 || annotation.StartColumn != 5 ||
		strings.Contains(annotation.Message, secret) || !strings.Contains(annotation.Message, "[REDACTED]") {
		t.Fatalf("annotation was not safely projected: %+v", annotation)
	}
	// Callers receive a detached annotation slice and cannot mutate stored state.
	results[0].Annotations[0].Message = "caller mutation"
	secondRead := process.(interface {
		PipelineResults() []workloadjobs.WorkloadPipelineResult
	}).PipelineResults()
	if secondRead[0].Annotations[0].Message == "caller mutation" {
		t.Fatal("PipelineResults exposed mutable annotation storage")
	}
}

func TestFactoryPipelineDoesNotAnnotateNonLintOutput(t *testing.T) {
	command := filepath.Join(t.TempDir(), "go-test")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf '%s\\n' './internal/api.go:12:5: test output'\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	plan := ExecutionPlan{Identity: testIdentity(), RepositoryPath: t.TempDir(), FactoryPipelines: []factoryconfig.ExecutionPlan{{
		Pipeline: "verify", RuntimeClass: "standard", TimeoutSecond: 30, MemoryMB: 2048, CPUMillis: 1000,
		Network: "registry-only", Commands: []factoryconfig.Command{{Step: "go-test", Executable: "/usr/local/go/bin/go", Args: []string{"test", "./..."}}},
	}}}
	process, err := (GoTestProcessFactory{GoBinary: command}).startFactoryPipelines(context.Background(), plan, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := process.Wait(); err == nil {
		t.Fatal("expected test command failure")
	}
	results := process.(interface {
		PipelineResults() []workloadjobs.WorkloadPipelineResult
	}).PipelineResults()
	if len(results) != 1 || len(results[0].Annotations) != 0 {
		t.Fatalf("non-lint output was converted into annotations: %+v", results)
	}
}

func TestFactoryPipelineDropsAnnotationsWhenLintCaptureTruncates(t *testing.T) {
	command := filepath.Join(t.TempDir(), "go-vet")
	script := "#!/bin/sh\nhead -c " + strconv.Itoa(maxLintDiagnosticOutput+1) + " /dev/zero | tr '\\000' x\nprintf '%s\\n' './internal/api.go:12:5: diagnostic'\nexit 1\n"
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	plan := ExecutionPlan{Identity: testIdentity(), RepositoryPath: t.TempDir(), FactoryPipelines: []factoryconfig.ExecutionPlan{{
		Pipeline: "verify", RuntimeClass: "standard", TimeoutSecond: 30, MemoryMB: 2048, CPUMillis: 1000,
		Network: "registry-only", Commands: []factoryconfig.Command{{Step: "go-vet", Executable: "/usr/local/go/bin/go", Args: []string{"vet", "./..."}}},
	}}}
	process, err := (GoTestProcessFactory{GoBinary: command}).startFactoryPipelines(context.Background(), plan, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := process.Wait(); err == nil {
		t.Fatal("expected vet command failure")
	}
	results := process.(interface {
		PipelineResults() []workloadjobs.WorkloadPipelineResult
	}).PipelineResults()
	if len(results) != 1 || len(results[0].Annotations) != 0 {
		t.Fatalf("truncated diagnostic capture produced annotations: %+v", results)
	}
}
