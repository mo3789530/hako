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
