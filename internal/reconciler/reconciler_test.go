package reconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

func TestOperationForDesiredObservedState(t *testing.T) {
	cases := []struct {
		name     string
		desired  domain.DesiredWorkspaceState
		observed domain.ObservedWorkspaceState
		want     domain.OperationType
		needed   bool
	}{
		{"already running", domain.DesiredWorkspaceRunning, domain.ObservedWorkspaceRunning, "", false},
		{"create or recover running", domain.DesiredWorkspaceRunning, domain.ObservedWorkspaceFailed, domain.OperationEnsureRunning, true},
		{"resume suspended", domain.DesiredWorkspaceRunning, domain.ObservedWorkspaceSuspended, domain.OperationResume, true},
		{"suspend active", domain.DesiredWorkspaceSuspended, domain.ObservedWorkspaceRunning, domain.OperationSuspend, true},
		{"delete active", domain.DesiredWorkspaceDeleted, domain.ObservedWorkspaceRunning, domain.OperationDelete, true},
		{"already deleted", domain.DesiredWorkspaceDeleted, domain.ObservedWorkspaceDeleted, "", false},
		{"unknown desired state", "unknown", domain.ObservedWorkspaceRunning, "", false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			got, needed := operationFor(item.desired, item.observed)
			if got != item.want || needed != item.needed {
				t.Fatalf("operationFor(%q, %q) = (%q, %t), want (%q, %t)", item.desired, item.observed, got, needed, item.want, item.needed)
			}
		})
	}
}

type testStore struct {
	expired      int
	expireErr    error
	candidates   []Candidate
	created      int
	expireCutoff time.Time
	expireNow    time.Time
	expireLimit  int
}

func (s *testStore) FailStaleOperations(_ context.Context, cutoff, now time.Time, limit int) (int, error) {
	s.expireCutoff, s.expireNow, s.expireLimit = cutoff, now, limit
	return s.expired, s.expireErr
}

func (s *testStore) Candidates(context.Context, int, time.Time, time.Duration) ([]Candidate, error) {
	return s.candidates, nil
}

func (s *testStore) EnsureOperation(context.Context, Candidate, domain.OperationType, time.Time, time.Duration) (bool, error) {
	s.created++
	return true, nil
}

func TestRunOnceExpiresStaleOperationsBeforeReconciling(t *testing.T) {
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	store := &testStore{
		expired:    2,
		candidates: []Candidate{{WorkspaceID: "ws_1", Desired: domain.DesiredWorkspaceRunning, Observed: domain.ObservedWorkspacePending}},
	}
	worker, err := New(store, Config{
		BatchSize: 7, FailureDelay: time.Minute, OperationTimeout: 20 * time.Minute,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := worker.RunOnce(context.Background())
	if err != nil || stats.TimedOut != 2 || stats.Checked != 1 || stats.Created != 1 || store.created != 1 {
		t.Fatalf("RunOnce stats=%+v created=%d err=%v", stats, store.created, err)
	}
	if !store.expireCutoff.Equal(now.Add(-20*time.Minute)) || !store.expireNow.Equal(now) || store.expireLimit != 7 {
		t.Fatalf("stale expiry args cutoff=%s now=%s limit=%d", store.expireCutoff, store.expireNow, store.expireLimit)
	}
}

func TestRunOnceReturnsExpiryFailureWithoutReconciling(t *testing.T) {
	store := &testStore{expireErr: errors.New("database unavailable"), candidates: []Candidate{{WorkspaceID: "ws_1"}}}
	worker, err := New(store, Config{})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := worker.RunOnce(context.Background())
	if err == nil || stats.Checked != 0 || store.created != 0 {
		t.Fatalf("expiry error should stop the pass: stats=%+v created=%d err=%v", stats, store.created, err)
	}
}

func TestOperationTimeoutConfiguration(t *testing.T) {
	store := &testStore{}
	worker, err := New(store, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if worker.config.OperationTimeout != 30*time.Minute {
		t.Fatalf("default Operation timeout=%s, want 30m", worker.config.OperationTimeout)
	}
	if _, err := New(store, Config{OperationTimeout: time.Millisecond}); err == nil {
		t.Fatal("Operation timeout below one second should be rejected")
	}
}
