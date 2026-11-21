package fake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/resourcecontroller"
)

func TestRuntimeAppliesWorkspaceActionsAndDeduplicatesOperationID(t *testing.T) {
	runtime := New()
	command := resourcecontroller.Command{OperationID: "op_1", WorkspaceID: "ws_1", Type: domain.OperationEnsureRunning}
	state, err := runtime.Execute(context.Background(), command)
	if err != nil || state != domain.ObservedWorkspaceRunning {
		t.Fatalf("ensure running: state=%s error=%v", state, err)
	}
	duplicate, err := runtime.Execute(context.Background(), command)
	if err != nil || duplicate != state {
		t.Fatalf("duplicate command must return cached state: state=%s error=%v", duplicate, err)
	}
	count := runtime.ProcessedOperationCount()
	if count != 1 {
		t.Fatalf("duplicate Operation should be applied once, got %d cached Operations", count)
	}
	command.OperationID = "op_2"
	command.Type = domain.OperationSuspend
	state, err = runtime.Execute(context.Background(), command)
	if err != nil || state != domain.ObservedWorkspaceSuspended {
		t.Fatalf("suspend: state=%s error=%v", state, err)
	}
	command.OperationID = "op_3"
	command.Type = domain.OperationResume
	state, err = runtime.Execute(context.Background(), command)
	if err != nil || state != domain.ObservedWorkspaceRunning {
		t.Fatalf("resume: state=%s error=%v", state, err)
	}
	command.OperationID = "op_4"
	command.Type = domain.OperationDelete
	state, err = runtime.Execute(context.Background(), command)
	if err != nil || state != domain.ObservedWorkspaceDeleted {
		t.Fatalf("delete: state=%s error=%v", state, err)
	}
	if state, ok := runtime.State("ws_1"); !ok || state != domain.ObservedWorkspaceDeleted {
		t.Fatalf("unexpected final fake Runtime state: %s, %t", state, ok)
	}
}

func TestRuntimeRejectsOperationIDReusedForDifferentRequest(t *testing.T) {
	runtime := New()
	command := resourcecontroller.Command{OperationID: "op_1", WorkspaceID: "ws_1", Type: domain.OperationSuspend}
	if _, err := runtime.Execute(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	command.Type = domain.OperationDelete
	if _, err := runtime.Execute(context.Background(), command); !errors.Is(err, ErrOperationIDConflict) {
		t.Fatalf("expected Operation ID conflict, got %v", err)
	}
	command.Type = domain.OperationSuspend
	command.TenantID = "tenant_other"
	if _, err := runtime.Execute(context.Background(), command); !errors.Is(err, ErrOperationIDConflict) {
		t.Fatalf("expected Operation ID tenant conflict, got %v", err)
	}
}

func TestRuntimeHonorsCancelledContext(t *testing.T) {
	runtime := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runtime.Execute(ctx, resourcecontroller.Command{OperationID: "op_1", WorkspaceID: "ws_1", Type: domain.OperationEnsureRunning, CreatedAt: time.Now()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled context, got %v", err)
	}
}

func TestRuntimeRejectsStaleAndReplayedWorkspaceRevisions(t *testing.T) {
	runtime := New()
	command := resourcecontroller.Command{
		SchemaVersion: 2, WorkspaceRevision: 4, OperationID: "op_4", TenantID: "tenant_1",
		WorkspaceID: "ws_fenced", ResourcePlaneID: "rp_1", Type: domain.OperationEnsureRunning,
	}
	if _, err := runtime.Execute(context.Background(), command); err != nil {
		t.Fatalf("apply current Workspace revision: %v", err)
	}
	newer := command
	newer.SchemaVersion = 2
	newer.WorkspaceRevision = 5
	newer.OperationID = "op_5"
	newer.Type = domain.OperationSuspend
	if _, err := runtime.Execute(context.Background(), newer); err != nil {
		t.Fatalf("apply newer Workspace revision: %v", err)
	}
	stale := command
	stale.OperationID = "op_stale"
	if _, err := runtime.Execute(context.Background(), stale); !errors.Is(err, ErrStaleWorkspaceRevision) {
		t.Fatalf("older Workspace revision should be fenced, got %v", err)
	}
	replayedGeneration := newer
	replayedGeneration.OperationID = "op_same_generation"
	if _, err := runtime.Execute(context.Background(), replayedGeneration); !errors.Is(err, ErrStaleWorkspaceRevision) {
		t.Fatalf("different Operation at an already-applied revision should be fenced, got %v", err)
	}
	if state, ok := runtime.State("ws_fenced"); !ok || state != domain.ObservedWorkspaceSuspended {
		t.Fatalf("stale command changed current Runtime state: %s, %t", state, ok)
	}
}
