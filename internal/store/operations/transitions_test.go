package operations

import (
	"testing"

	"github.com/mo3789530/hako/internal/domain"
)

func TestOperationStateHelpers(t *testing.T) {
	transitions := []struct {
		from domain.OperationStatus
		to   domain.OperationStatus
		want bool
	}{
		{domain.OperationPending, domain.OperationRunning, true},
		{domain.OperationPending, domain.OperationFailed, true},
		{domain.OperationPending, domain.OperationCancelled, true},
		{domain.OperationRunning, domain.OperationPending, true},
		{domain.OperationRunning, domain.OperationSucceeded, true},
		{domain.OperationRunning, domain.OperationFailed, true},
		{domain.OperationRunning, domain.OperationCancelled, true},
		{domain.OperationSucceeded, domain.OperationPending, false},
		{domain.OperationFailed, domain.OperationRunning, false},
		{"unknown", domain.OperationFailed, false},
	}
	for _, test := range transitions {
		if got := validTransition(test.from, test.to); got != test.want {
			t.Errorf("validTransition(%q, %q) = %t, want %t", test.from, test.to, got, test.want)
		}
	}
	if attemptIncrement(domain.OperationRunning) != 1 || attemptIncrement(domain.OperationSucceeded) != 0 {
		t.Fatal("only entering running should increment attempts")
	}

	for _, state := range []domain.ObservedWorkspaceState{
		domain.ObservedWorkspacePending, domain.ObservedWorkspaceProvisioning, domain.ObservedWorkspaceRunning,
		domain.ObservedWorkspaceSuspending, domain.ObservedWorkspaceSuspended, domain.ObservedWorkspaceDeleting,
		domain.ObservedWorkspaceDeleted, domain.ObservedWorkspaceFailed,
	} {
		if !validObservedState(state) {
			t.Errorf("expected observed state %q to be valid", state)
		}
	}
	if validObservedState("invalid") {
		t.Fatal("unknown observed state should be rejected")
	}
}

func TestExpectedObservedState(t *testing.T) {
	for _, test := range []struct {
		typeName domain.OperationType
		want     domain.ObservedWorkspaceState
	}{
		{domain.OperationEnsureRunning, domain.ObservedWorkspaceRunning},
		{domain.OperationResume, domain.ObservedWorkspaceRunning},
		{domain.OperationSuspend, domain.ObservedWorkspaceSuspended},
		{domain.OperationDelete, domain.ObservedWorkspaceDeleted},
		{"unknown", ""},
	} {
		if got := expectedObservedState(test.typeName); got != test.want {
			t.Errorf("expectedObservedState(%q) = %q, want %q", test.typeName, got, test.want)
		}
	}
}
