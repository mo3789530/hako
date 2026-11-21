package workloadcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/workloadjournal"
)

type memoryJournal struct {
	mu       sync.Mutex
	snapshot workloadjournal.Snapshot
	err      error
	creates  int
}

func (s *memoryJournal) Create(_ context.Context, j workloadjournal.Journal) (workloadjournal.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return workloadjournal.Snapshot{}, s.err
	}
	if s.snapshot.ETag != "" {
		return workloadjournal.Snapshot{}, workloadjournal.ErrConflict
	}
	s.creates++
	s.snapshot = workloadjournal.Snapshot{Journal: j, ETag: "etag"}
	return s.snapshot, nil
}
func (s *memoryJournal) Get(context.Context, string, string, string) (workloadjournal.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot, s.err
}
func (*memoryJournal) CompareAndSwap(context.Context, workloadjournal.Snapshot, workloadjournal.Journal) (workloadjournal.Snapshot, error) {
	panic("acceptance must not mutate existing checkpoints")
}
func envelope(t *testing.T, mutate func(*Command)) []byte {
	t.Helper()
	c := Command{SchemaVersion: 1, TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1, ResourcePlaneID: "rp_1", IdempotencyToken: "token_1"}
	if mutate != nil {
		mutate(&c)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestAcceptConcurrentDuplicates(t *testing.T) {
	s := &memoryJournal{}
	a, err := NewAcceptor(s, "rp_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	data := envelope(t, nil)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Accept(context.Background(), data); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if s.creates != 1 {
		t.Fatalf("created %d journals", s.creates)
	}
}
func TestAcceptRetainsCancellationAndCleanup(t *testing.T) {
	s := &memoryJournal{}
	a, _ := NewAcceptor(s, "rp_1", nil)
	first, err := a.Accept(context.Background(), envelope(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	s.snapshot.Journal.CancelRequested = true
	s.snapshot.Journal.ExecutionState = workloadjournal.Cancelled
	s.snapshot.Journal.CleanupState = workloadjournal.CleanupPending
	s.snapshot.Journal.Revision++
	s.snapshot.Journal.UpdatedAt = first.Journal.UpdatedAt.Add(time.Second)
	got, err := a.Accept(context.Background(), envelope(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Journal.CancelRequested || got.Journal.CleanupState != workloadjournal.CleanupPending {
		t.Fatal("checkpoint overwritten")
	}
}
func TestAcceptRejectsMismatchedCheckpoint(t *testing.T) {
	for _, field := range []string{"tenant", "run", "attempt", "generation", "plane", "token"} {
		t.Run(field, func(t *testing.T) {
			s := &memoryJournal{}
			a, _ := NewAcceptor(s, "rp_1", nil)
			if _, err := a.Accept(context.Background(), envelope(t, nil)); err != nil {
				t.Fatal(err)
			}
			data := envelope(t, func(c *Command) {
				switch field {
				case "tenant":
					c.TenantID = "tenant_2"
				case "run":
					c.WorkloadRunID = "run_2"
				case "attempt":
					c.AttemptID = "attempt_2"
				case "generation":
					c.Generation++
				case "plane":
					c.ResourcePlaneID = "rp_2"
				case "token":
					c.IdempotencyToken = "token_2"
				}
			})
			if _, err := a.Accept(context.Background(), data); err == nil {
				t.Fatal("accepted changed identity")
			}
		})
	}
}
func TestDecodeInvalid(t *testing.T) {
	valid := envelope(t, nil)
	for i, data := range [][]byte{nil, []byte("{}"), []byte("null"), append(append([]byte{}, valid...), valid...), []byte(`{"schema_version":1,"command":"sh"}`), envelope(t, func(c *Command) { c.Generation = 0 }), envelope(t, func(c *Command) { c.SchemaVersion = 2 }), envelope(t, func(c *Command) { c.TenantID = "../tenant" }), make([]byte, 65537)} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if _, err := Decode(data); !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestDecodeRejectsAmbiguousFields(t *testing.T) {
	valid := string(envelope(t, nil))
	for _, data := range []string{
		strings.Replace(valid, `"generation":1`, `"generation":1,"generation":2`, 1),
		strings.Replace(valid, `"tenant_id"`, `"Tenant_ID"`, 1),
		strings.Replace(valid, `"schema_version":1`, `"schema_version":null`, 1),
		`{"schema_version":`,
	} {
		if _, err := Decode([]byte(data)); !errors.Is(err, ErrInvalidCommand) {
			t.Errorf("ambiguous envelope accepted: %v", err)
		}
	}
}
func TestAcceptStoreFailure(t *testing.T) {
	failure := errors.New("storage unavailable")
	a, _ := NewAcceptor(&memoryJournal{err: failure}, "rp_1", nil)
	if _, err := a.Accept(context.Background(), envelope(t, nil)); !errors.Is(err, failure) {
		t.Fatalf("error = %v", err)
	}
}
func TestNewAcceptorInvalid(t *testing.T) {
	for _, id := range []string{"", "../rp"} {
		if _, err := NewAcceptor(&memoryJournal{}, id, nil); err == nil {
			t.Fatal("accepted invalid plane")
		}
	}
	if _, err := NewAcceptor(nil, "rp_1", nil); err == nil {
		t.Fatal("accepted nil store")
	}
}
