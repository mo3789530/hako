// Package fake provides a process-local, idempotent Workspace Runtime for
// development and integration tests. It creates no real compute resources.
package fake

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/resourcecontroller"
)

var ErrOperationIDConflict = errors.New("Operation ID was reused for a different Runtime request")
var ErrStaleWorkspaceRevision = errors.New("Runtime rejected a stale Workspace revision")

type cachedResult struct {
	tenantID        domain.TenantID
	workspaceID     domain.WorkspaceID
	resourcePlaneID domain.ResourcePlaneID
	typeOf          domain.OperationType
	observed        domain.ObservedWorkspaceState
	err             error
}

type Runtime struct {
	mu         sync.Mutex
	states     map[domain.WorkspaceID]domain.ObservedWorkspaceState
	revisions  map[domain.WorkspaceID]int64
	operations map[domain.OperationID]cachedResult
}

func New() *Runtime {
	return &Runtime{states: make(map[domain.WorkspaceID]domain.ObservedWorkspaceState), revisions: make(map[domain.WorkspaceID]int64), operations: make(map[domain.OperationID]cachedResult)}
}

func (r *Runtime) Execute(ctx context.Context, command resourcecontroller.Command) (domain.ObservedWorkspaceState, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if command.OperationID == "" || command.WorkspaceID == "" {
		return "", errors.New("Operation and Workspace IDs are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if prior, exists := r.operations[command.OperationID]; exists {
		if prior.tenantID != command.TenantID || prior.workspaceID != command.WorkspaceID || prior.resourcePlaneID != command.ResourcePlaneID || prior.typeOf != command.Type {
			return "", ErrOperationIDConflict
		}
		return prior.observed, prior.err
	}
	currentRevision := r.revisions[command.WorkspaceID]
	if command.WorkspaceRevision < currentRevision ||
		(command.WorkspaceRevision == currentRevision && (command.WorkspaceRevision > 0 || currentRevision > 0)) {
		return "", ErrStaleWorkspaceRevision
	}
	var observed domain.ObservedWorkspaceState
	switch command.Type {
	case domain.OperationEnsureRunning, domain.OperationResume:
		observed = domain.ObservedWorkspaceRunning
	case domain.OperationSuspend:
		observed = domain.ObservedWorkspaceSuspended
	case domain.OperationDelete:
		observed = domain.ObservedWorkspaceDeleted
	default:
		return "", fmt.Errorf("unsupported fake Runtime Operation %q", command.Type)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	r.states[command.WorkspaceID] = observed
	if command.WorkspaceRevision > currentRevision {
		r.revisions[command.WorkspaceID] = command.WorkspaceRevision
	}
	r.operations[command.OperationID] = cachedResult{
		tenantID: command.TenantID, workspaceID: command.WorkspaceID,
		resourcePlaneID: command.ResourcePlaneID, typeOf: command.Type, observed: observed,
	}
	return observed, nil
}

func (r *Runtime) State(workspaceID domain.WorkspaceID) (domain.ObservedWorkspaceState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, exists := r.states[workspaceID]
	return state, exists
}

func (r *Runtime) ProcessedOperationCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.operations)
}
