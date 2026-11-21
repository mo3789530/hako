package workloadjournal

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestJournalUpdateFencesExecutionAndCleanup(t *testing.T) {
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	journal, err := New(Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1",
		Generation: 1, ResourcePlaneID: "rp_1", IdempotencyToken: "attempt_token_1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{Journal: journal, ETag: `"etag-1"`}

	launching := journal
	launching.ExecutionState = Launching
	launching.Revision++
	launching.UpdatedAt = now.Add(time.Second)
	if err := ValidateUpdate(snapshot, launching); err != nil {
		t.Fatalf("accepted -> launching rejected: %v", err)
	}

	cancelled := journal
	cancelled.CancelRequested = true
	cancelled.Revision++
	cancelled.UpdatedAt = now.Add(time.Second)
	if err := ValidateUpdate(snapshot, cancelled); err != nil {
		t.Fatalf("cancel request rejected: %v", err)
	}
	launchAfterCancel := launching
	launchAfterCancel.CancelRequested = true
	if err := ValidateUpdate(Snapshot{Journal: cancelled, ETag: `"etag-2"`}, launchAfterCancel); !errors.Is(err, ErrConflict) {
		t.Fatalf("launch after cancellation should be fenced: %v", err)
	}

	running := launching
	running.ExecutionState = Running
	running.Revision++
	running.UpdatedAt = now.Add(2 * time.Second)
	if err := ValidateUpdate(Snapshot{Journal: launching, ETag: `"etag-3"`}, running); err != nil {
		t.Fatalf("launching -> running rejected: %v", err)
	}
	terminal := running
	terminal.ExecutionState = Succeeded
	terminal.CleanupState = CleanupPending
	terminal.Revision++
	terminal.UpdatedAt = now.Add(3 * time.Second)
	if err := ValidateUpdate(Snapshot{Journal: running, ETag: `"etag-4"`}, terminal); err != nil {
		t.Fatalf("terminal update with pending cleanup rejected: %v", err)
	}
	confirmed := terminal
	confirmed.CleanupState = CleanupConfirmed
	confirmed.Revision++
	confirmed.UpdatedAt = now.Add(4 * time.Second)
	if err := ValidateUpdate(Snapshot{Journal: terminal, ETag: `"etag-5"`}, confirmed); err != nil {
		t.Fatalf("cleanup confirmation rejected: %v", err)
	}
	mutated := confirmed
	mutated.IdempotencyToken = "attacker-token"
	mutated.Revision++
	if err := ValidateUpdate(Snapshot{Journal: confirmed, ETag: `"etag-6"`}, mutated); !errors.Is(err, ErrConflict) {
		t.Fatalf("immutable client token mutation accepted: %v", err)
	}
}

func TestJournalEncodingValidatesIdentityAndPayloadLimit(t *testing.T) {
	now := time.Now().UTC()
	journal, err := New(Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1",
		Generation: 2, ResourcePlaneID: "rp_1", IdempotencyToken: "attempt_token_2"}, now)
	if err != nil {
		t.Fatal(err)
	}
	data, err := Encode(journal)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(data)
	if err != nil || decoded.AttemptID != journal.AttemptID || decoded.Generation != 2 {
		t.Fatalf("round trip = %+v, %v", decoded, err)
	}
	unsafe := journal
	unsafe.TenantID = "../tenant"
	if _, err := Encode(unsafe); err == nil {
		t.Fatal("path traversal Tenant ID accepted")
	}
	if _, err := Decode([]byte(strings.Repeat("x", (64<<10)+1))); err == nil {
		t.Fatal("oversized journal accepted")
	}
}

func TestJournalValidateRejectsInvalidIdentityAndLifecycle(t *testing.T) {
	now := time.Now().UTC()
	valid, err := New(Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1",
		Generation: 1, ResourcePlaneID: "rp_1", IdempotencyToken: "attempt_token_1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Journal)
	}{
		{"version", func(j *Journal) { j.SchemaVersion = 2 }},
		{"tenant path", func(j *Journal) { j.TenantID = "../tenant" }},
		{"generation", func(j *Journal) { j.Generation = 0 }},
		{"revision", func(j *Journal) { j.Revision = 0 }},
		{"timestamp ordering", func(j *Journal) { j.UpdatedAt = now.Add(-time.Second) }},
		{"execution state", func(j *Journal) { j.ExecutionState = "unknown" }},
		{"cleanup state", func(j *Journal) { j.CleanupState = "unknown" }},
		{"runtime handle path", func(j *Journal) { j.RuntimeHandle = "../runtime" }},
		{"terminal without cleanup", func(j *Journal) { j.ExecutionState = Succeeded }},
		{"active with cleanup", func(j *Journal) { j.CleanupState = CleanupPending }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			journal := valid
			test.mutate(&journal)
			if err := journal.Validate(); err == nil {
				t.Fatal("invalid journal accepted")
			}
		})
	}
}

func TestValidateUpdateRejectsStaleOrIllegalChanges(t *testing.T) {
	now := time.Now().UTC()
	journal, err := New(Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1",
		Generation: 1, ResourcePlaneID: "rp_1", IdempotencyToken: "attempt_token_1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	current := Snapshot{Journal: journal, ETag: `"etag"`}
	validNext := journal
	validNext.ExecutionState = Launching
	validNext.Revision++
	validNext.UpdatedAt = now.Add(time.Second)
	tests := []struct {
		name    string
		current Snapshot
		mutate  func(*Journal)
	}{
		{"missing etag", Snapshot{Journal: journal}, func(*Journal) {}},
		{"changed identity", current, func(j *Journal) { j.AttemptID = "attempt_2" }},
		{"wrong revision", current, func(j *Journal) { j.Revision++ }},
		{"older timestamp", current, func(j *Journal) { j.UpdatedAt = now.Add(-time.Second) }},
		{"illegal execution edge", current, func(j *Journal) { j.ExecutionState = Succeeded; j.CleanupState = CleanupPending }},
		{"launch after cancel", Snapshot{Journal: func() Journal { j := journal; j.CancelRequested = true; return j }(), ETag: `"cancel"`}, func(j *Journal) { j.ExecutionState = Launching; j.CancelRequested = true }},
		{"invalid cleanup edge", func() Snapshot {
			j := journal
			j.ExecutionState = Succeeded
			j.CleanupState = CleanupConfirmed
			return Snapshot{Journal: j, ETag: `"done"`}
		}(), func(j *Journal) { j.ExecutionState = Succeeded; j.CleanupState = CleanupPending }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			next := validNext
			test.mutate(&next)
			if err := ValidateUpdate(test.current, next); !errors.Is(err, ErrConflict) {
				t.Fatalf("ValidateUpdate() = %v, want ErrConflict", err)
			}
		})
	}
	changedVM := validNext
	changedVM.RuntimeHandle = "runtime_123"
	err = ValidateUpdate(current, changedVM)
	if err != nil {
		t.Fatalf("first VM binding should be allowed: %v", err)
	}
	mutatedVM := changedVM
	mutatedVM.Revision++
	mutatedVM.UpdatedAt = now.Add(2 * time.Second)
	mutatedVM.RuntimeHandle = "runtime_456"
	if err := ValidateUpdate(Snapshot{Journal: changedVM, ETag: `"vm"`}, mutatedVM); !errors.Is(err, ErrConflict) {
		t.Fatalf("VM binding changed after creation: %v", err)
	}
}
