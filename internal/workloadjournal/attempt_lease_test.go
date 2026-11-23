package workloadjournal

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type leaseMemoryStore struct {
	mu       sync.Mutex
	snapshot Snapshot
}

func (s *leaseMemoryStore) Create(_ context.Context, journal Journal) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot.ETag != "" {
		return Snapshot{}, ErrConflict
	}
	s.snapshot = Snapshot{Journal: journal, ETag: "rev-1"}
	return s.snapshot, nil
}

func (s *leaseMemoryStore) Get(context.Context, string, string, string) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot.ETag == "" {
		return Snapshot{}, ErrNotFound
	}
	return s.snapshot, nil
}

func (s *leaseMemoryStore) CompareAndSwap(_ context.Context, current Snapshot, next Journal) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current.ETag == "" || current.ETag != s.snapshot.ETag || ValidateUpdate(s.snapshot, next) != nil {
		return Snapshot{}, ErrConflict
	}
	s.snapshot = Snapshot{Journal: next, ETag: fmt.Sprintf("rev-%d", next.Revision)}
	return s.snapshot, nil
}

func newLeaseTestJournal(t *testing.T) (*leaseMemoryStore, Command, time.Time) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	command := Command{TenantID: "tenant_lease", WorkloadRunID: "run_lease", AttemptID: "attempt_lease",
		Generation: 3, ResourcePlaneID: "rp_lease", IdempotencyToken: "token_lease"}
	journal, err := New(command, now)
	if err != nil {
		t.Fatal(err)
	}
	store := &leaseMemoryStore{}
	if _, err := store.Create(context.Background(), journal); err != nil {
		t.Fatal(err)
	}
	return store, command, now
}

func TestAttemptLeaseAcquireRenewReleaseAndFenceStaleOwner(t *testing.T) {
	store, command, now := newLeaseTestJournal(t)
	ttl := time.Minute
	first, firstSnapshot, err := AcquireAttemptLease(context.Background(), store, command, "owner_first_123", now, ttl)
	if err != nil || !AttemptLeaseHeld(firstSnapshot, first, now) {
		t.Fatalf("acquire first lease=%+v snapshot=%+v err=%v", first, firstSnapshot.Journal, err)
	}
	if _, _, err := AcquireAttemptLease(context.Background(), store, command, "owner_second_123", now.Add(time.Second), ttl); !errors.Is(err, ErrAttemptLeaseHeld) {
		t.Fatalf("overlapping lease acquired: %v", err)
	}
	renewed, renewedSnapshot, err := RenewAttemptLease(context.Background(), store, first, now.Add(30*time.Second), ttl)
	if err != nil || !AttemptLeaseHeld(renewedSnapshot, renewed, now.Add(30*time.Second)) || renewed.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("renew lease=%+v snapshot=%+v err=%v", renewed, renewedSnapshot.Journal, err)
	}
	if err := ReleaseAttemptLease(context.Background(), store, first, now.Add(31*time.Second)); !errors.Is(err, ErrAttemptLeaseLost) {
		t.Fatalf("stale lease owner released renewed lease: %v", err)
	}
	if err := ReleaseAttemptLease(context.Background(), store, renewed, now.Add(32*time.Second)); err != nil {
		t.Fatalf("release current lease: %v", err)
	}
	second, secondSnapshot, err := AcquireAttemptLease(context.Background(), store, command, "owner_second_123", now.Add(33*time.Second), ttl)
	if err != nil || second.Owner != "owner_second_123" || !AttemptLeaseHeld(secondSnapshot, second, now.Add(33*time.Second)) {
		t.Fatalf("acquire after release=%+v snapshot=%+v err=%v", second, secondSnapshot.Journal, err)
	}
	if _, _, err := RenewAttemptLease(context.Background(), store, renewed, now.Add(34*time.Second), ttl); !errors.Is(err, ErrAttemptLeaseLost) {
		t.Fatalf("stale owner renewed successor lease: %v", err)
	}
}

func TestAttemptLeaseExpiredOwnerCanBeReplacedWithCAS(t *testing.T) {
	store, command, now := newLeaseTestJournal(t)
	first, _, err := AcquireAttemptLease(context.Background(), store, command, "owner_first_123", now, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	second, snapshot, err := AcquireAttemptLease(context.Background(), store, command, "owner_second_123", now.Add(5*time.Second), time.Minute)
	if err != nil || second.Owner != "owner_second_123" || !AttemptLeaseHeld(snapshot, second, now.Add(5*time.Second)) {
		t.Fatalf("expired lease was not replaced: lease=%+v journal=%+v err=%v", second, snapshot.Journal, err)
	}
	if err := ReleaseAttemptLease(context.Background(), store, first, now.Add(6*time.Second)); !errors.Is(err, ErrAttemptLeaseLost) {
		t.Fatalf("expired owner released successor lease: %v", err)
	}
}

func TestAttemptLeaseConcurrentAcquireHasOneWinner(t *testing.T) {
	store, command, now := newLeaseTestJournal(t)
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, owner := range []string{"owner_first_123", "owner_second_123"} {
		go func(owner string) {
			<-start
			_, _, err := AcquireAttemptLease(context.Background(), store, command, owner, now, time.Minute)
			errs <- err
		}(owner)
	}
	close(start)
	first, second := <-errs, <-errs
	wins, held := 0, 0
	for _, err := range []error{first, second} {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrAttemptLeaseHeld) || errors.Is(err, ErrConflict) {
			held++
		} else {
			t.Fatalf("unexpected acquire error: %v", err)
		}
	}
	if wins != 1 || held != 1 {
		t.Fatalf("lease acquisition winners=%d rejected=%d, errors=%v/%v", wins, held, first, second)
	}
}
