package actionsrunner

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
)

func TestCleanupWorkerTerminatesBeforeDeregisterAndAcknowledges(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	job := cleanupJob("rp-tokyo-01", "lambda-microvm-1", 1234)
	queue := &fakeCleanupQueue{job: job, claimed: true}
	var calls []string
	worker := cleanupWorker(t, queue, func(_ context.Context, planeID, vmID string) error {
		calls = append(calls, "terminate:"+planeID+":"+vmID)
		return nil
	}, func(_ context.Context, got githubwebhook.ActionsRunnerCleanupJob) error {
		calls = append(calls, "deregister")
		if got.TenantID != "tenant_one" || got.IntegrationID != "integration_one" || got.Installation != 77 || got.Repository != 88 || *got.RunnerID != 1234 {
			t.Fatalf("deregistration received wrong scoped identity: %+v", got)
		}
		return nil
	}, func() time.Time { return now })
	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = %v, %v", processed, err)
	}
	if !reflect.DeepEqual(calls, []string{"terminate:rp-tokyo-01:lambda-microvm-1", "deregister"}) {
		t.Fatalf("external cleanup order = %v", calls)
	}
	if queue.claimPlane != "rp-tokyo-01" || !queue.completed || queue.retryCode != "" {
		t.Fatalf("queue state = %+v", queue)
	}
}

func TestCleanupWorkerCanDeregisterWithoutRuntimeAndRequiresMicroVMPlanePin(t *testing.T) {
	job := cleanupJob("", "", 1234)
	queue := &fakeCleanupQueue{job: job, claimed: true}
	var terminated, deregistered bool
	worker := cleanupWorker(t, queue, func(context.Context, string, string) error {
		terminated = true
		return nil
	}, func(context.Context, githubwebhook.ActionsRunnerCleanupJob) error {
		deregistered = true
		return nil
	}, time.Now)
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOne() = %v, %v", processed, err)
	}
	if terminated || !deregistered || !queue.completed {
		t.Fatalf("no-VM cleanup terminated=%v deregistered=%v completed=%v", terminated, deregistered, queue.completed)
	}

	wrongPlane := "rp-other-01"
	job = cleanupJob(wrongPlane, "lambda-microvm-1", 1234)
	queue = &fakeCleanupQueue{job: job, claimed: true}
	terminated, deregistered = false, false
	worker = cleanupWorker(t, queue, func(context.Context, string, string) error {
		terminated = true
		return nil
	}, func(context.Context, githubwebhook.ActionsRunnerCleanupJob) error {
		deregistered = true
		return nil
	}, time.Now)
	if processed, err := worker.ProcessOne(context.Background()); !processed || !errors.Is(err, ErrCleanupDeferred) {
		t.Fatalf("wrong-Plane ProcessOne() = %v, %v", processed, err)
	}
	if terminated || deregistered || queue.retryCode != "provider_unavailable" {
		t.Fatalf("wrong-Plane record was acted on: terminated=%v deregistered=%v retry=%q", terminated, deregistered, queue.retryCode)
	}
}

func TestCleanupWorkerRetriesFailuresWithSafeCodeAndBoundedBackoff(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	job := cleanupJob("rp-tokyo-01", "lambda-microvm-1", 1234)
	job.AttemptCount = 3
	queue := &fakeCleanupQueue{job: job, claimed: true}
	var deregistered bool
	worker := cleanupWorker(t, queue, func(context.Context, string, string) error { return errors.New("private provider detail") }, func(context.Context, githubwebhook.ActionsRunnerCleanupJob) error {
		deregistered = true
		return nil
	}, func() time.Time { return now })
	if processed, err := worker.ProcessOne(context.Background()); !processed || !errors.Is(err, ErrCleanupDeferred) {
		t.Fatalf("ProcessOne() = %v, %v", processed, err)
	}
	if queue.retryCode != "microvm_terminate_failed" || !queue.retryAt.Equal(now.Add(60*time.Second)) || queue.completed || deregistered {
		t.Fatalf("failure handling = retry=%q at=%s complete=%v deregistered=%v", queue.retryCode, queue.retryAt, queue.completed, deregistered)
	}
}

func TestCleanupWorkerReportsLostLeaseAfterIdempotentSideEffects(t *testing.T) {
	job := cleanupJob("rp-tokyo-01", "lambda-microvm-1", 1234)
	notCompleted := false
	queue := &fakeCleanupQueue{job: job, claimed: true, completeResult: &notCompleted}
	worker := cleanupWorker(t, queue, func(context.Context, string, string) error { return nil }, func(context.Context, githubwebhook.ActionsRunnerCleanupJob) error { return nil }, time.Now)
	if processed, err := worker.ProcessOne(context.Background()); !processed || !errors.Is(err, ErrCleanupLeaseLost) {
		t.Fatalf("ProcessOne() = %v, %v", processed, err)
	}
}

func TestCleanupWorkerValidatesPlaneAndWorkerIdentity(t *testing.T) {
	queue := &fakeCleanupQueue{}
	terminator := fakeTerminator(func(context.Context, string, string) error { return nil })
	deregistrator := fakeDeregistrator(func(context.Context, githubwebhook.ActionsRunnerCleanupJob) error { return nil })
	for _, config := range []CleanupWorkerConfig{
		{ResourcePlaneID: "RP_TOKYO", WorkerID: "cleanup-worker-1", LeaseDuration: time.Minute, OperationTimeout: time.Minute},
		{ResourcePlaneID: "rp-tokyo-01", WorkerID: "short", LeaseDuration: time.Minute, OperationTimeout: time.Minute},
	} {
		if _, err := NewCleanupWorker(config, queue, deregistrator, terminator, time.Now); !errors.Is(err, ErrInvalidCleanupConfig) {
			t.Errorf("NewCleanupWorker(%+v) error = %v", config, err)
		}
	}
}

func cleanupWorker(t *testing.T, queue CleanupQueue, terminate func(context.Context, string, string) error, deregister func(context.Context, githubwebhook.ActionsRunnerCleanupJob) error, now func() time.Time) *CleanupWorker {
	t.Helper()
	worker, err := NewCleanupWorker(CleanupWorkerConfig{ResourcePlaneID: "rp-tokyo-01", WorkerID: "cleanup-worker-1", LeaseDuration: time.Minute, OperationTimeout: time.Minute},
		queue, fakeDeregistrator(deregister), fakeTerminator(terminate), now)
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func cleanupJob(plane, microVM string, runnerID int64) githubwebhook.ActionsRunnerCleanupJob {
	job := githubwebhook.ActionsRunnerCleanupJob{IntegrationID: "integration_one", TenantID: domain.TenantID("tenant_one"), Installation: 77, Repository: 88,
		WorkflowJobID: 99, RunAttempt: 1, Reason: "completed", State: "claimed", ClaimID: "cleanup-worker-1", AttemptCount: 1}
	if plane != "" {
		job.ResourcePlaneID = &plane
	}
	if microVM != "" {
		job.LambdaMicroVMID = &microVM
	}
	if runnerID > 0 {
		job.RunnerID = &runnerID
	}
	return job
}

type fakeCleanupQueue struct {
	job            githubwebhook.ActionsRunnerCleanupJob
	claimed        bool
	claimPlane     string
	completed      bool
	completeResult *bool
	retryCode      string
	retryAt        time.Time
}

func (q *fakeCleanupQueue) Claim(_ context.Context, planeID, _ string, _ time.Time, _ time.Duration) (githubwebhook.ActionsRunnerCleanupJob, bool, error) {
	q.claimPlane = planeID
	return q.job, q.claimed, nil
}

func (q *fakeCleanupQueue) Complete(_ context.Context, _ githubwebhook.ActionsRunnerCleanupJob, _ time.Time) (bool, error) {
	if q.completeResult != nil {
		return *q.completeResult, nil
	}
	q.completed = true
	return true, nil
}

func (q *fakeCleanupQueue) Retry(_ context.Context, _ githubwebhook.ActionsRunnerCleanupJob, _, retryAt time.Time, code string) (bool, error) {
	q.retryCode, q.retryAt = code, retryAt
	return true, nil
}

type fakeTerminator func(context.Context, string, string) error

func (f fakeTerminator) Terminate(ctx context.Context, planeID, microVMID string) error {
	return f(ctx, planeID, microVMID)
}

type fakeDeregistrator func(context.Context, githubwebhook.ActionsRunnerCleanupJob) error

func (f fakeDeregistrator) Deregister(ctx context.Context, job githubwebhook.ActionsRunnerCleanupJob) error {
	return f(ctx, job)
}
