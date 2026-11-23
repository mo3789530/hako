package workloadagentcore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot, nil
}
func (s *testStore) CompareAndSwap(_ context.Context, old workloadjournal.Snapshot, next workloadjournal.Journal) (workloadjournal.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old.ETag != s.snapshot.ETag || workloadjournal.ValidateUpdate(old, next) != nil {
		return workloadjournal.Snapshot{}, workloadjournal.ErrConflict
	}
	s.snapshot = workloadjournal.Snapshot{Journal: next, ETag: fmt.Sprintf("rev-%d", next.Revision)}
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
	if got.Journal.ExecutionState != workloadjournal.Launching || got.Journal.RuntimeHandle == "" || len(got.Journal.RuntimeSpecPayload) == 0 || got.Journal.Validate() != nil {
		t.Fatalf("invalid prepared journal: %+v", got.Journal)
	}
	if _, err := Prepare(context.Background(), store, initial, runtime, now); !errors.Is(err, workloadjournal.ErrConflict) {
		t.Fatalf("stale writer: %v", err)
	}
	if _, err := Prepare(context.Background(), store, got, runtime, now); !errors.Is(err, ErrNotReady) {
		t.Fatalf("accepted already prepared attempt: %v", err)
	}
}

func TestPrepareKeepsWorkloadSpecificationSeparateFromRuntimeTarget(t *testing.T) {
	now := time.Now().UTC()
	command := workloadjournal.Command{TenantID: "tenant", WorkloadRunID: "run", AttemptID: "attempt",
		Generation: 1, ResourcePlaneID: "plane", IdempotencyToken: "token"}
	journal, err := workloadjournal.New(command, now)
	if err != nil {
		t.Fatal(err)
	}
	workloadSpec := json.RawMessage(`{"kind":"agent","source_digest":"pinned"}`)
	digest := sha256.Sum256(workloadSpec)
	journal.SpecificationPayload = workloadSpec
	journal.SpecificationHash = hex.EncodeToString(digest[:])
	initial := workloadjournal.Snapshot{Journal: journal, ETag: "initial"}
	store := &testStore{snapshot: initial}
	runtime := Runtime{ARN: "arn:aws:bedrock-agentcore:us-east-1:123456789012:runtime/agent", Qualifier: "DEFAULT"}
	prepared, err := Prepare(context.Background(), store, initial, runtime, now)
	if err != nil {
		t.Fatal(err)
	}
	if string(prepared.Journal.SpecificationPayload) != string(workloadSpec) || prepared.Journal.SpecificationHash != hex.EncodeToString(digest[:]) ||
		len(prepared.Journal.RuntimeSpecPayload) == 0 || prepared.Journal.RuntimeSpecHash == "" {
		t.Fatalf("Workload and Runtime specs did not remain separately pinned: %+v", prepared.Journal)
	}
	if _, err := PreparedInvocationFromJournal(prepared.Journal); err != nil {
		t.Fatalf("restore Runtime spec alongside Workload spec: %v", err)
	}
}

func TestPreparedInvocationFromJournalStrictlyRestoresPinnedTarget(t *testing.T) {
	now := time.Now().UTC()
	command := workloadjournal.Command{TenantID: "tenant", WorkloadRunID: "run", AttemptID: "attempt",
		Generation: 1, ResourcePlaneID: "plane", IdempotencyToken: "token"}
	journal, err := workloadjournal.New(command, now)
	if err != nil {
		t.Fatal(err)
	}
	store := &testStore{snapshot: workloadjournal.Snapshot{Journal: journal, ETag: "initial"}}
	runtime := Runtime{ARN: "arn:aws:bedrock-agentcore:us-east-1:123456789012:runtime/agent", Qualifier: "DEFAULT"}
	prepared, err := Prepare(context.Background(), store, store.snapshot, runtime, now)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := PreparedInvocationFromJournal(prepared.Journal)
	if err != nil || decoded.SessionID != prepared.Journal.RuntimeHandle || decoded.Runtime != runtime {
		t.Fatalf("restored invocation = %+v, err=%v", decoded, err)
	}

	var base map[string]any
	if err := json.Unmarshal(prepared.Journal.RuntimeSpecPayload, &base); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"unknown field":   func(value map[string]any) { value["unexpected"] = true },
		"foreign session": func(value map[string]any) { value["session_id"] = "hako-foreign" },
		"wrong backend":   func(value map[string]any) { value["backend"] = "other" },
		"invalid target": func(value map[string]any) {
			value["runtime"] = map[string]any{"runtime_arn": "not-an-arn", "qualifier": "DEFAULT"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			payload := make(map[string]any, len(base)+1)
			for key, value := range base {
				payload[key] = value
			}
			mutate(payload)
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			candidate := prepared.Journal
			candidate.RuntimeSpecPayload = encoded
			digest := sha256.Sum256(encoded)
			candidate.RuntimeSpecHash = hex.EncodeToString(digest[:])
			if _, err := PreparedInvocationFromJournal(candidate); !errors.Is(err, ErrNotReady) {
				t.Fatalf("invalid frozen invocation = %v, want ErrNotReady", err)
			}
		})
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

func TestClaimInvocationIsOneDurableAtMostOnceFence(t *testing.T) {
	now := time.Now().UTC()
	j, err := workloadjournal.New(workloadjournal.Command{TenantID: "tenant", WorkloadRunID: "run", AttemptID: "attempt",
		Generation: 1, ResourcePlaneID: "plane", IdempotencyToken: "token"}, now)
	if err != nil {
		t.Fatal(err)
	}
	initial := workloadjournal.Snapshot{Journal: j, ETag: "initial"}
	store := &testStore{snapshot: initial}
	runtime := Runtime{ARN: "arn:aws:bedrock-agentcore:us-east-1:123456789012:runtime/agent", Qualifier: "DEFAULT"}
	prepared, err := Prepare(context.Background(), store, initial, runtime, now)
	if err != nil {
		t.Fatal(err)
	}
	type claimResult struct {
		snapshot workloadjournal.Snapshot
		err      error
	}
	results := make(chan claimResult, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			claimed, claimErr := ClaimInvocation(context.Background(), store, prepared, now.Add(time.Second))
			results <- claimResult{snapshot: claimed, err: claimErr}
		}()
	}
	close(start)
	var claimed workloadjournal.Snapshot
	wins, conflicts := 0, 0
	for range 2 {
		result := <-results
		switch {
		case result.err == nil:
			claimed = result.snapshot
			wins++
		case errors.Is(result.err, workloadjournal.ErrConflict):
			conflicts++
		default:
			t.Fatalf("claim error = %v", result.err)
		}
	}
	if wins != 1 || conflicts != 1 || claimed.Journal.ExecutionState != workloadjournal.Running || claimed.Journal.Revision != prepared.Journal.Revision+1 {
		t.Fatalf("concurrent claim outcome: winner=%+v wins=%d conflicts=%d", claimed.Journal, wins, conflicts)
	}
	if _, err := ClaimInvocation(context.Background(), store, claimed, now.Add(2*time.Second)); !errors.Is(err, ErrNotReady) {
		t.Fatalf("already claimed invocation = %v, want not-ready", err)
	}
}

func TestClaimInvocationRejectsCancellationAndForeignSession(t *testing.T) {
	now := time.Now().UTC()
	j, err := workloadjournal.New(workloadjournal.Command{TenantID: "tenant", WorkloadRunID: "run", AttemptID: "attempt",
		Generation: 1, ResourcePlaneID: "plane", IdempotencyToken: "token"}, now)
	if err != nil {
		t.Fatal(err)
	}
	initial := workloadjournal.Snapshot{Journal: j, ETag: "initial"}
	store := &testStore{snapshot: initial}
	runtime := Runtime{ARN: "arn:aws:bedrock-agentcore:us-east-1:123456789012:runtime/agent", Qualifier: "DEFAULT"}
	prepared, err := Prepare(context.Background(), store, initial, runtime, now)
	if err != nil {
		t.Fatal(err)
	}
	cancelled := prepared
	cancelled.Journal.CancelRequested = true
	if _, err := ClaimInvocation(context.Background(), store, cancelled, now.Add(time.Second)); !errors.Is(err, ErrNotReady) {
		t.Fatalf("cancelled attempt claim = %v, want not-ready", err)
	}
	foreign := prepared
	foreign.Journal.RuntimeHandle = "hako-foreign"
	if _, err := ClaimInvocation(context.Background(), store, foreign, now.Add(time.Second)); !errors.Is(err, ErrNotReady) {
		t.Fatalf("foreign session claim = %v, want not-ready", err)
	}
}

func TestClaimInvocationLostResponseCannotBeReplayedAfterRecovery(t *testing.T) {
	now := time.Now().UTC()
	command := workloadjournal.Command{TenantID: "tenant", WorkloadRunID: "run", AttemptID: "attempt",
		Generation: 1, ResourcePlaneID: "plane", IdempotencyToken: "token"}
	initialJournal, err := workloadjournal.New(command, now)
	if err != nil {
		t.Fatal(err)
	}
	initial := workloadjournal.Snapshot{Journal: initialJournal, ETag: "initial"}
	store := &testStore{snapshot: initial}
	runtime := Runtime{ARN: "arn:aws:bedrock-agentcore:us-east-1:123456789012:runtime/agent", Qualifier: "DEFAULT"}
	prepared, err := Prepare(context.Background(), store, initial, runtime, now)
	if err != nil {
		t.Fatal(err)
	}

	// Re-read the durable state as if the dispatcher restarted after making an
	// invocation whose response may have been lost.
	claimed, err := ClaimInvocation(context.Background(), store, prepared, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Journal.ExecutionState != workloadjournal.Running {
		t.Fatalf("claimed invocation state = %s", claimed.Journal.ExecutionState)
	}

	recovered, err := store.Get(context.Background(), command.TenantID, command.WorkloadRunID, command.ResourcePlaneID)
	if err != nil || recovered.Journal.ExecutionState != workloadjournal.Running {
		t.Fatalf("recovered journal = %+v, err=%v", recovered.Journal, err)
	}
	_, retryErr := ClaimInvocation(context.Background(), store, recovered, now.Add(2*time.Second))
	if !errors.Is(retryErr, ErrNotReady) {
		t.Fatalf("ambiguous invocation replay claim = %v, want ErrNotReady", retryErr)
	}
}
