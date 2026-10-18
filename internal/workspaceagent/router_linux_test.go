//go:build linux

package workspaceagent

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacesession"
)

type routedDialer struct {
	mu       sync.Mutex
	claims   workspacesession.Claims
	port     uint16
	channels int
}

func (d *routedDialer) OpenShell(_ context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
	d.record(claims, 0)
	return nil, nil
}

func (d *routedDialer) OpenVSCode(_ context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
	d.record(claims, 0)
	return nil, nil
}

func (d *routedDialer) OpenPort(_ context.Context, claims workspacesession.Claims, _ workspacesession.Permission, port uint16) (io.ReadWriteCloser, error) {
	d.record(claims, port)
	return nil, nil
}

func (d *routedDialer) record(claims workspacesession.Claims, port uint16) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.claims = claims
	d.port = port
	d.channels++
}

func TestWorkspaceRouterRoutesByVerifiedWorkspaceClaim(t *testing.T) {
	router := NewWorkspaceRouter()
	first := &routedDialer{}
	second := &routedDialer{}
	if !router.Register("ws_alpha", first) || !router.Register("ws_beta", second) {
		t.Fatal("failed to register distinct Workspace runtimes")
	}

	claims := workspacesession.Claims{UserID: "usr_1", TenantID: "tenant_1", WorkspaceID: "ws_beta", TicketID: "ticket_1"}
	if _, err := router.OpenPort(context.Background(), claims, workspacesession.PermissionPortForward, 5432); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second.claims, claims) || second.port != 5432 || second.channels != 1 {
		t.Fatalf("selected runtime did not receive the exact claim/port: %+v", second)
	}
	if first.channels != 0 {
		t.Fatalf("wrong Workspace runtime was called: %+v", first)
	}
}

func TestWorkspaceRouterFailsClosedAndProtectsReplacementFromStaleCleanup(t *testing.T) {
	router := NewWorkspaceRouter()
	first := &routedDialer{}
	second := &routedDialer{}
	if !router.Register("ws_alpha", first) {
		t.Fatal("initial registration failed")
	}
	if router.Register("ws_alpha", second) {
		t.Fatal("duplicate registration replaced a live Workspace runtime")
	}
	if router.Unregister("ws_alpha", second) {
		t.Fatal("stale cleanup removed a different runtime")
	}
	if !router.Unregister("ws_alpha", first) || !router.Register("ws_alpha", second) {
		t.Fatal("could not replace runtime after expected cleanup")
	}
	if router.Unregister("ws_alpha", first) {
		t.Fatal("delayed cleanup removed the replacement runtime")
	}

	if _, err := router.OpenShell(context.Background(), workspacesession.Claims{WorkspaceID: "ws_missing"}); !errors.Is(err, ErrInteractiveChannelUnavailable) {
		t.Fatalf("unknown Workspace route error = %v", err)
	}
	if _, err := router.OpenShell(context.Background(), workspacesession.Claims{}); !errors.Is(err, ErrInteractiveChannelUnavailable) {
		t.Fatalf("empty Workspace claim error = %v", err)
	}
	claims := workspacesession.Claims{WorkspaceID: domain.WorkspaceID("ws_alpha")}
	if _, err := router.OpenShell(context.Background(), claims); err != nil || !reflect.DeepEqual(second.claims, claims) {
		t.Fatalf("replacement was not selected: claims=%+v error=%v", second.claims, err)
	}
}
