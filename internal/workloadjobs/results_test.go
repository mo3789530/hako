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
	}
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
}
