package workloadjobs

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

func TestValidateWorkloadResult(t *testing.T) {
	valid := WorkloadResult{SchemaVersion: 1, TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1",
		Generation: 1, ResourcePlaneID: "rp_1", CommitSHA: strings.Repeat("a", 40), Status: domain.WorkloadSucceeded,
		CompletedAt: time.Now().UTC()}
	tests := []struct {
		name   string
		mutate func(*WorkloadResult)
	}{
		{name: "unknown schema", mutate: func(result *WorkloadResult) { result.SchemaVersion = 2 }},
		{name: "missing identity", mutate: func(result *WorkloadResult) { result.AttemptID = "" }},
		{name: "nonterminal status", mutate: func(result *WorkloadResult) { result.Status = domain.WorkloadRunning }},
		{name: "success with nonzero exit", mutate: func(result *WorkloadResult) { result.ExitCode = 1 }},
		{name: "unsafe error code", mutate: func(result *WorkloadResult) {
			result.Status = domain.WorkloadFailed
			result.ErrorCode = "secret\nvalue"
		}},
		{name: "unredacted logs", mutate: func(result *WorkloadResult) {
			result.LogArtifact = &WorkloadLogArtifact{StorageRef: "s3://bucket/key", SizeBytes: 1, SHA256: strings.Repeat("a", 64), Redacted: false}
		}},
		{name: "oversized logs", mutate: func(result *WorkloadResult) {
			result.LogArtifact = &WorkloadLogArtifact{StorageRef: "s3://bucket/key", SizeBytes: (100 << 20) + 1, SHA256: strings.Repeat("a", 64), Redacted: true}
		}},
		{name: "invalid digest", mutate: func(result *WorkloadResult) {
			result.LogArtifact = &WorkloadLogArtifact{StorageRef: "s3://bucket/key", SizeBytes: 1, SHA256: strings.Repeat("z", 64), Redacted: true}
		}},
		{name: "oversized Agent change bundle", mutate: func(result *WorkloadResult) {
			result.ChangeBundle = &WorkloadChangeArtifact{StorageRef: "s3://bucket/key", SizeBytes: (8 << 20) + 1, SHA256: strings.Repeat("a", 64)}
		}},
		{name: "invalid Agent change digest", mutate: func(result *WorkloadResult) {
			result.ChangeBundle = &WorkloadChangeArtifact{StorageRef: "s3://bucket/key", SizeBytes: 1, SHA256: strings.Repeat("z", 64)}
		}},
	}
	t.Run("valid bounded Agent change metadata", func(t *testing.T) {
		result := valid
		result.ChangeBundle = &WorkloadChangeArtifact{StorageRef: "s3://bucket/key", SizeBytes: 4, SHA256: strings.Repeat("a", 64)}
		if err := ValidateResult(result); err != nil {
			t.Fatalf("valid change bundle metadata rejected: %v", err)
		}
	})
	if err := validateWorkloadResult(valid); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := valid
			test.mutate(&result)
			if err := validateWorkloadResult(result); !errors.Is(err, ErrWorkloadResultInvalid) {
				t.Fatalf("validation error = %v, want ErrWorkloadResultInvalid", err)
			}
		})
	}

	t.Run("pipeline metadata is bounded and sorted", func(t *testing.T) {
		result := valid
		result.PipelineResults = []WorkloadPipelineResult{
			{Name: "build", Status: "succeeded"},
			{Name: "test", Status: "failed", ExitCode: 2, ErrorCode: "step_failed", Annotations: []domain.WorkloadCheckAnnotation{{
				Path: "internal/api/handler.go", StartLine: 12, EndLine: 12, AnnotationLevel: "failure", Message: "Expected value was not found.",
			}}},
		}
		result.Status, result.ExitCode, result.ErrorCode = domain.WorkloadFailed, 2, "test_failed"
		if err := ValidateResult(result); err != nil {
			t.Fatalf("valid pipeline metadata rejected: %v", err)
		}
	})

	for _, test := range []struct {
		name    string
		results []WorkloadPipelineResult
	}{
		{name: "duplicate or unsorted", results: []WorkloadPipelineResult{{Name: "test", Status: "succeeded"}, {Name: "build", Status: "succeeded"}}},
		{name: "unsafe name", results: []WorkloadPipelineResult{{Name: "../test", Status: "succeeded"}}},
		{name: "unsupported status", results: []WorkloadPipelineResult{{Name: "test", Status: "running"}}},
		{name: "failed without exit code", results: []WorkloadPipelineResult{{Name: "test", Status: "failed", ErrorCode: "step_failed"}}},
		{name: "timeout without exit code", results: []WorkloadPipelineResult{{Name: "test", Status: "timed_out"}}},
		{name: "unsafe annotation path", results: []WorkloadPipelineResult{{Name: "test", Status: "succeeded", Annotations: []domain.WorkloadCheckAnnotation{{
			Path: "../secret", StartLine: 1, EndLine: 1, AnnotationLevel: "failure", Message: "unsafe path",
		}}}}},
		{name: "too many annotations", results: []WorkloadPipelineResult{{Name: "test", Status: "succeeded", Annotations: make([]domain.WorkloadCheckAnnotation, 51)}}},
		{name: "annotation payload exceeds bound", results: []WorkloadPipelineResult{{Name: "test", Status: "succeeded", Annotations: []domain.WorkloadCheckAnnotation{
			{Path: "a.go", StartLine: 1, EndLine: 1, AnnotationLevel: "warning", Message: "x", RawDetails: strings.Repeat("x", 10000)},
			{Path: "b.go", StartLine: 1, EndLine: 1, AnnotationLevel: "warning", Message: "x", RawDetails: strings.Repeat("x", 10000)},
			{Path: "c.go", StartLine: 1, EndLine: 1, AnnotationLevel: "warning", Message: "x", RawDetails: strings.Repeat("x", 10000)},
			{Path: "d.go", StartLine: 1, EndLine: 1, AnnotationLevel: "warning", Message: "x", RawDetails: strings.Repeat("x", 10000)},
			{Path: "e.go", StartLine: 1, EndLine: 1, AnnotationLevel: "warning", Message: "x", RawDetails: strings.Repeat("x", 10000)},
		}}}},
	} {
		t.Run("reject pipeline "+test.name, func(t *testing.T) {
			result := valid
			result.PipelineResults = test.results
			if test.name == "duplicate or unsorted" {
				result.PipelineResults = []WorkloadPipelineResult{{Name: "test", Status: "succeeded"}, {Name: "test", Status: "succeeded"}}
			}
			if err := ValidateResult(result); !errors.Is(err, ErrWorkloadResultInvalid) {
				t.Fatalf("validation error = %v, want ErrWorkloadResultInvalid", err)
			}
		})
	}
}
