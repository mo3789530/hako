package workloadagentcore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/workloadjournal"
)

type testStore struct {
	mu       sync.Mutex
	snapshot workloadjournal.Snapshot
}

func (s *testStore) Create(context.Context, workloadjournal.Journal) (workloadjournal.Snapshot, error) {
	panic("unexpected Create")
}
func (s *testStore) Get(context.Context, string, string, string) (workloadjournal.Snapshot, error) {
	panic("unexpected Get")
}
func (s *testStore) CompareAndSwap(_ context.Context, old workloadjournal.Snapshot, next workloadjournal.Journal) (workloadjournal.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old.ETag != s.snapshot.ETag || workloadjournal.ValidateUpdate(old, next) != nil {
		return workloadjournal.Snapshot{}, workloadjournal.ErrConflict
	}
	s.snapshot = workloadjournal.Snapshot{Journal: next, ETag: "next"}
	return s.snapshot, nil
}

func TestPrepareFreezesIdentityAndRejectsStaleWriter(t *testing.T) {
	now := time.Now().UTC()
	j, err := workloadjournal.New(workloadjournal.Command{TenantID: "tenant", WorkloadRunID: "run", AttemptID: "attempt", Generation: 1, ResourcePlaneID: "plane", IdempotencyToken: "token"}, now)
	if err != nil {
		t.Fatal(err)
	}
	initial := workloadjournal.Snapshot{Journal: j, ETag: "initial"}
	store := &testStore{snapshot: initial}
	runtime := Runtime{ARN: "arn:aws:bedrock-agentcore:us-east-1:123456789012:runtime/agent", Qualifier: "DEFAULT"}
	got, err := Prepare(context.Background(), store, initial, runtime, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Journal.ExecutionState != workloadjournal.Launching || got.Journal.RuntimeHandle == "" || len(got.Journal.SpecificationPayload) == 0 || got.Journal.Validate() != nil {
		t.Fatalf("invalid prepared journal: %+v", got.Journal)
	}
	if _, err := Prepare(context.Background(), store, initial, runtime, now); !errors.Is(err, workloadjournal.ErrConflict) {
		t.Fatalf("stale writer: %v", err)
	}
	if _, err := Prepare(context.Background(), store, got, runtime, now); !errors.Is(err, ErrNotReady) {
		t.Fatalf("accepted already prepared attempt: %v", err)
	}
}

func TestPrepareRejectsCancellationAndInvalidRuntime(t *testing.T) {
	now := time.Now().UTC()
	j, err := workloadjournal.New(workloadjournal.Command{TenantID: "tenant", WorkloadRunID: "run", AttemptID: "attempt", Generation: 1, ResourcePlaneID: "plane", IdempotencyToken: "token"}, now)
	if err != nil {
		t.Fatal(err)
	}
	initial := workloadjournal.Snapshot{Journal: j, ETag: "initial"}
	store := &testStore{snapshot: initial}
	runtime := Runtime{ARN: "arn:aws:bedrock-agentcore:us-east-1:123456789012:runtime/agent", Qualifier: "DEFAULT"}
	if _, err := Prepare(context.Background(), store, initial, Runtime{ARN: "bad", Qualifier: "DEFAULT"}, now); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	initial.Journal.CancelRequested = true
	if _, err := Prepare(context.Background(), store, initial, runtime, now); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
}
