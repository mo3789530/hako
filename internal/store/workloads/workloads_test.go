package workloads

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

func validRun(now time.Time) domain.WorkloadRun {
	return domain.WorkloadRun{ID: "run_1", TenantID: "tenant_1", Kind: domain.WorkloadJob,
		IdempotencyKey: "job-1", RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted,
		CreatedAt: now, TimeoutSeconds: 60, TimeoutAt: now.Add(time.Minute)}
}

func TestValidateRunRequiresSafeRepositoryIdentityAndTimeout(t *testing.T) {
	now := time.Now().UTC()
	if err := validateRun(validRun(now)); err != nil {
		t.Fatalf("valid run rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*domain.WorkloadRun)
	}{
		{"repository without installation", func(run *domain.WorkloadRun) { run.GitHubRepository = 42 }},
		{"repository without commit", func(run *domain.WorkloadRun) { run.GitHubInstallation, run.GitHubRepository = 7, 42 }},
		{"invalid commit", func(run *domain.WorkloadRun) { run.CommitSHA = strings.Repeat("z", 40) }},
		{"oversized timeout", func(run *domain.WorkloadRun) { run.TimeoutSeconds = int64(MaxTimeout/time.Second) + 1 }},
		{"invalid trust", func(run *domain.WorkloadRun) { run.TrustLevel = "admin" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run := validRun(now)
			test.mutate(&run)
			if err := validateRun(run); err == nil {
				t.Fatal("invalid Workload Run was accepted")
			}
		})
	}
}

func TestWorkloadStateTransitionsAreForwardOnly(t *testing.T) {
	if !transitionAllowed(domain.WorkloadPending, domain.WorkloadQueued) ||
		!transitionAllowed(domain.WorkloadRunning, domain.WorkloadSucceeded) ||
		!transitionAllowed(domain.WorkloadQueued, domain.WorkloadTimedOut) {
		t.Fatal("expected valid forward transitions were rejected")
	}
	if transitionAllowed(domain.WorkloadSucceeded, domain.WorkloadRunning) ||
		transitionAllowed(domain.WorkloadPending, domain.WorkloadSucceeded) || validTransition(domain.WorkloadPending) {
		t.Fatal("terminal, skipped, or invalid transitions should be rejected")
	}
}

func TestValidateArtifactStorageAndRedaction(t *testing.T) {
	for _, reference := range []string{"s3://hako-artifacts/tenant/run/logs.txt", "ecr://123456789012.dkr.ecr.ap-northeast-1.amazonaws.com/api@sha256:abc", "https://artifacts.example/run/report.xml"} {
		if err := validateStorageRef(reference); err != nil {
			t.Errorf("valid reference %q rejected: %v", reference, err)
		}
	}
	for _, reference := range []string{"s3://user:secret@bucket/key", "https://example.test/log?X-Amz-Signature=secret", "file:///etc/passwd", "s3://bucket/key#fragment"} {
		if err := validateStorageRef(reference); err == nil {
			t.Errorf("unsafe reference %q accepted", reference)
		}
	}
	artifact := domain.WorkloadArtifact{ID: "artifact_1", TenantID: "tenant_1", RunID: "run_1", Kind: "logs",
		StorageRef: "s3://hako-artifacts/run/logs.txt", SHA256: strings.Repeat("a", 64), CreatedAt: time.Now().UTC()}
	if err := validateArtifact(artifact); !errors.Is(err, errLogsMustBeRedacted) {
		t.Fatalf("unredacted logs error = %v", err)
	}
	artifact.Redacted = true
	if err := validateArtifact(artifact); err != nil {
		t.Fatalf("redacted logs rejected: %v", err)
	}
}
