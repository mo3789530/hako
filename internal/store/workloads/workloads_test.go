package workloads

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubwebhook"
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

func TestWebhookDeliveryIdempotencyIsStableAndIntegrationScoped(t *testing.T) {
	deliveryID := "f1b2c3d4-e5f6-4789-8abc-def012345678"
	first := WebhookDeliveryIdempotencyKey("integration_one", deliveryID)
	if first == "" || len(first) > 128 || first != WebhookDeliveryIdempotencyKey("integration_one", deliveryID) {
		t.Fatalf("delivery idempotency key is not stable: %q", first)
	}
	if first == WebhookDeliveryIdempotencyKey("integration_two", deliveryID) || first == WebhookDeliveryIdempotencyKey("integration_one", "a1b2c3d4-e5f6-4789-8abc-def012345678") {
		t.Fatal("distinct integration/delivery identities shared an idempotency key")
	}
}

func TestEligibleWebhookRunTriggersAreExplicit(t *testing.T) {
	for _, eventType := range []string{"repository.push", "repository.pull_request.opened", "repository.pull_request.reopened", "repository.pull_request.synchronize"} {
		if !eligibleRunTrigger(githubwebhook.RepositoryEvent{Type: eventType}) {
			t.Errorf("supported trigger %q rejected", eventType)
		}
	}
	for _, eventType := range []string{"repository.pull_request.closed", "repository.pull_request.edited", "repository.workflow_job.queued", "repository.workflow_job.in_progress", "repository.workflow_job.completed", "github.installation.deleted", "arbitrary.command"} {
		if eligibleRunTrigger(githubwebhook.RepositoryEvent{Type: eventType}) {
			t.Errorf("unsupported trigger %q accepted", eventType)
		}
	}
	if eligibleRunTrigger(githubwebhook.RepositoryEvent{GitHubEvent: "push", Type: "repository.push", Deleted: true}) {
		t.Fatal("branch deletion push should not create a verification run")
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

func TestExpiredArtifactStoreOperationsRequireBoundedInputs(t *testing.T) {
	if _, err := ListExpiredArtifacts(t.Context(), nil, time.Now(), 1); err == nil {
		t.Fatal("expired artifact listing accepted a nil transaction")
	}
	for _, limit := range []int{0, MaxExpiredArtifactBatchSize + 1} {
		if _, err := ListExpiredArtifacts(t.Context(), nil, time.Now(), limit); err == nil {
			t.Errorf("expired artifact listing accepted invalid batch size %d", limit)
		}
	}
	if _, err := ListExpiredArtifacts(t.Context(), nil, time.Time{}, 1); err == nil {
		t.Fatal("expired artifact listing accepted a zero time")
	}
	if _, err := DeleteExpiredArtifact(t.Context(), nil, domain.WorkloadArtifact{}, time.Now()); err == nil {
		t.Fatal("expired artifact deletion accepted incomplete identity")
	}
}
