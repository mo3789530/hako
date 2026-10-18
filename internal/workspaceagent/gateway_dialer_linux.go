//go:build linux

package workspaceagent

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacesession"
)

var ErrInteractiveChannelUnavailable = errors.New("Workspace interactive channel is unavailable")

// GatewayDialer adapts Workspace-local shell and port streams to the Gateway
// contract. It is bound to one Workspace and refuses claims for any other
// Workspace; the Resource Plane router is responsible for selecting this
// adapter from the signed Workspace claim. OpenPort always dials loopback and
// relies on Gateway PortAuthorizer to allow the port from trusted state.
type GatewayDialer struct {
	WorkspaceID domain.WorkspaceID
	Shells      *ShellManager
}

func (d *GatewayDialer) OpenShell(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
	if d == nil || d.Shells == nil || d.WorkspaceID == "" || claims.WorkspaceID != d.WorkspaceID {
		return nil, ErrInteractiveChannelUnavailable
	}
	return d.Shells.Open(ctx, TerminalSize{Columns: 120, Rows: 40})
}

func (d *GatewayDialer) OpenVSCode(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error) {
	return nil, ErrInteractiveChannelUnavailable
}

func (d *GatewayDialer) OpenPort(ctx context.Context, claims workspacesession.Claims, permission workspacesession.Permission, port uint16) (io.ReadWriteCloser, error) {
	if d == nil || d.WorkspaceID == "" || claims.WorkspaceID != d.WorkspaceID || ctx == nil || port == 0 {
		return nil, ErrInteractiveChannelUnavailable
	}
	if permission == workspacesession.PermissionSSHProxy {
		if port != 22 || !hasWorkspacePermission(claims.Permissions, permission) {
			return nil, ErrInteractiveChannelUnavailable
		}
	} else if permission != workspacesession.PermissionPortForward || !hasWorkspacePermission(claims.Permissions, permission) {
		return nil, ErrInteractiveChannelUnavailable
	}
	connection, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
	if err != nil {
		return nil, ErrInteractiveChannelUnavailable
	}
	return connection, nil
}

var _ interface {
	OpenShell(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
	OpenVSCode(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
	OpenPort(context.Context, workspacesession.Claims, workspacesession.Permission, uint16) (io.ReadWriteCloser, error)
} = (*GatewayDialer)(nil)
