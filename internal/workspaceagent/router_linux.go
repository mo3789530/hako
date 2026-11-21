//go:build linux

package workspaceagent

import (
	"context"
	"io"
	"reflect"
	"sync"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacesession"
)

// WorkspaceStreamDialer is the runtime-facing subset used by the Gateway.
// Implementations must be bound to exactly one Workspace and must not derive
// destinations from caller-controlled values.
type WorkspaceStreamDialer interface {
	OpenShell(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
	OpenExec(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
	OpenFileTransfer(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
	OpenVSCode(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
	OpenPort(context.Context, workspacesession.Claims, workspacesession.Permission, uint16) (io.ReadWriteCloser, error)
}

// WorkspaceRouter selects a Workspace-local runtime using the verified
// Workspace claim. Registration is an internal Resource Plane operation; the
// public Gateway never accepts runtime addresses or routing IDs from clients.
type WorkspaceRouter struct {
	mu         sync.RWMutex
	workspaces map[domain.WorkspaceID]WorkspaceStreamDialer
}

func NewWorkspaceRouter() *WorkspaceRouter {
	return &WorkspaceRouter{workspaces: make(map[domain.WorkspaceID]WorkspaceStreamDialer)}
}

// Register binds one runtime to its immutable Workspace identity. Replacing an
// existing binding is rejected so stale lifecycle events cannot redirect
// streams to a different runtime.
func (r *WorkspaceRouter) Register(workspaceID domain.WorkspaceID, dialer WorkspaceStreamDialer) bool {
	if r == nil || workspaceID == "" || dialer == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.workspaces == nil {
		r.workspaces = make(map[domain.WorkspaceID]WorkspaceStreamDialer)
	}
	if _, exists := r.workspaces[workspaceID]; exists {
		return false
	}
	r.workspaces[workspaceID] = dialer
	return true
}

// Unregister removes a binding only when it still points at the expected
// runtime. This compare-and-delete prevents a delayed cleanup from removing a
// newer registration after a Workspace allocation has been replaced.
func (r *WorkspaceRouter) Unregister(workspaceID domain.WorkspaceID, expected WorkspaceStreamDialer) bool {
	if r == nil || workspaceID == "" || expected == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current, exists := r.workspaces[workspaceID]
	if !exists || reflect.TypeOf(current) != reflect.TypeOf(expected) || !reflect.TypeOf(current).Comparable() || current != expected {
		return false
	}
	delete(r.workspaces, workspaceID)
	return true
}

func (r *WorkspaceRouter) OpenShell(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
	dialer, err := r.resolve(claims.WorkspaceID)
	if err != nil {
		return nil, err
	}
	return dialer.OpenShell(ctx, claims)
}

func (r *WorkspaceRouter) OpenExec(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
	dialer, err := r.resolve(claims.WorkspaceID)
	if err != nil {
		return nil, err
	}
	return dialer.OpenExec(ctx, claims)
}

func (r *WorkspaceRouter) OpenFileTransfer(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
	dialer, err := r.resolve(claims.WorkspaceID)
	if err != nil {
		return nil, err
	}
	return dialer.OpenFileTransfer(ctx, claims)
}

func (r *WorkspaceRouter) OpenVSCode(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
	dialer, err := r.resolve(claims.WorkspaceID)
	if err != nil {
		return nil, err
	}
	return dialer.OpenVSCode(ctx, claims)
}

func (r *WorkspaceRouter) OpenPort(ctx context.Context, claims workspacesession.Claims, permission workspacesession.Permission, port uint16) (io.ReadWriteCloser, error) {
	dialer, err := r.resolve(claims.WorkspaceID)
	if err != nil {
		return nil, err
	}
	return dialer.OpenPort(ctx, claims, permission, port)
}

func (r *WorkspaceRouter) resolve(workspaceID domain.WorkspaceID) (WorkspaceStreamDialer, error) {
	if r == nil || workspaceID == "" {
		return nil, ErrInteractiveChannelUnavailable
	}
	r.mu.RLock()
	dialer := r.workspaces[workspaceID]
	r.mu.RUnlock()
	if dialer == nil {
		return nil, ErrInteractiveChannelUnavailable
	}
	return dialer, nil
}

var _ WorkspaceStreamDialer = (*WorkspaceRouter)(nil)
