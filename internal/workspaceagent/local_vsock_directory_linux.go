//go:build linux

package workspaceagent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacejournal"
	"github.com/mo3789530/hako/internal/workspacesession"
	"golang.org/x/net/websocket"
)

var localFirecrackerProviderIDPattern = regexp.MustCompile(`^fc_[a-f0-9]{32}$`)

type WorkspaceGuestVsockDialer func(context.Context, string) (net.Conn, error)

// FirecrackerWorkspaceVsockStreamDirectory resolves a running allocation from
// the local durable journal and opens the fixed Guest Agent vsock service. It
// does not accept an endpoint, allocation ID, or port from the public caller.
type FirecrackerWorkspaceVsockStreamDirectory struct {
	ResourcePlaneID domain.ResourcePlaneID
	Journal         WorkspaceRuntimeJournalReader
	StateDirectory  string
	Tickets         WorkspaceAgentTicketIssuer
	Origin          string
	Dial            WorkspaceGuestVsockDialer
}

func (d *FirecrackerWorkspaceVsockStreamDirectory) OpenShell(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
	return d.open(ctx, claims, workspacesession.PermissionShell, 0)
}

func (d *FirecrackerWorkspaceVsockStreamDirectory) OpenExec(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
	return d.open(ctx, claims, workspacesession.PermissionExec, 0)
}

func (d *FirecrackerWorkspaceVsockStreamDirectory) OpenFileTransfer(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
	return d.open(ctx, claims, workspacesession.PermissionFileTransfer, 0)
}

func (d *FirecrackerWorkspaceVsockStreamDirectory) OpenVSCode(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
	return d.open(ctx, claims, workspacesession.PermissionVSCode, 0)
}

func (d *FirecrackerWorkspaceVsockStreamDirectory) OpenPort(ctx context.Context, claims workspacesession.Claims, permission workspacesession.Permission, port uint16) (io.ReadWriteCloser, error) {
	if (permission == workspacesession.PermissionSSHProxy && (port != 22 || !hasWorkspacePermission(claims.Permissions, permission))) ||
		(permission == workspacesession.PermissionPortForward && (port == 0 || !hasWorkspacePermission(claims.Permissions, permission))) ||
		(permission != workspacesession.PermissionSSHProxy && permission != workspacesession.PermissionPortForward) {
		return nil, ErrInteractiveChannelUnavailable
	}
	return d.open(ctx, claims, permission, port)
}

func (d *FirecrackerWorkspaceVsockStreamDirectory) open(ctx context.Context, claims workspacesession.Claims, permission workspacesession.Permission, port uint16) (io.ReadWriteCloser, error) {
	if d == nil || ctx == nil || claims.UserID == "" || claims.TenantID == "" || claims.WorkspaceID == "" ||
		claims.ResourcePlane == "" || d.ResourcePlaneID == "" || claims.ResourcePlane != d.ResourcePlaneID ||
		!hasWorkspacePermission(claims.Permissions, permission) || !localWorkspacePermission(permission) ||
		(permission == workspacesession.PermissionPortForward && port == 0) ||
		(permission == workspacesession.PermissionSSHProxy && port != 22) ||
		(permission != workspacesession.PermissionPortForward && permission != workspacesession.PermissionSSHProxy && port != 0) ||
		d.Journal == nil || d.Tickets == nil || d.Dial == nil || !filepath.IsAbs(d.StateDirectory) ||
		filepath.Clean(d.StateDirectory) != d.StateDirectory || !validGatewayOrigin(d.Origin) {
		return nil, ErrInteractiveChannelUnavailable
	}
	connectCtx, cancel := context.WithTimeout(ctx, workspaceAgentConnectTimeout)
	defer cancel()
	snapshot, err := d.Journal.Get(connectCtx, claims.TenantID, claims.WorkspaceID, claims.ResourcePlane)
	if err != nil || snapshot.ETag == "" || snapshot.Journal.Validate() != nil {
		return nil, ErrInteractiveChannelUnavailable
	}
	j := snapshot.Journal
	if j.TenantID != claims.TenantID || j.WorkspaceID != claims.WorkspaceID || j.ResourcePlaneID != claims.ResourcePlane ||
		j.State != workspacejournal.Running || j.Tombstone || j.ProviderID == "" ||
		!localFirecrackerProviderIDPattern.MatchString(j.ProviderID) {
		return nil, ErrInteractiveChannelUnavailable
	}
	ticketPort := port
	if permission == workspacesession.PermissionSSHProxy {
		ticketPort = 0
	}
	ticket, _, err := d.Tickets.Issue(AgentTicket{UserID: claims.UserID, TenantID: claims.TenantID,
		WorkspaceID: claims.WorkspaceID, ResourcePlane: claims.ResourcePlane, MicroVMID: j.ProviderID,
		Permission: permission, Port: ticketPort})
	if err != nil || !validWorkspaceHeaderCredential(ticket, maxAgentTicketBytes) {
		return nil, ErrInteractiveChannelUnavailable
	}
	connection, err := d.Dial(connectCtx, filepath.Join(d.StateDirectory, j.ProviderID))
	if err != nil || connection == nil {
		if connection != nil {
			_ = connection.Close()
		}
		return nil, ErrInteractiveChannelUnavailable
	}
	stopConnectCancel := context.AfterFunc(connectCtx, func() { _ = connection.Close() })
	if deadline, ok := connectCtx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	config, err := localWorkspaceAgentWebSocketConfig(string(claims.WorkspaceID), d.Origin, ticket, permission, port)
	if err != nil {
		stopConnectCancel()
		_ = connection.Close()
		return nil, ErrInteractiveChannelUnavailable
	}
	stream, err := websocket.NewClient(config, connection)
	stopConnectCancel()
	if err != nil || stream == nil {
		_ = connection.Close()
		return nil, ErrInteractiveChannelUnavailable
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		_ = stream.Close()
		return nil, ErrInteractiveChannelUnavailable
	}
	stream.PayloadType = websocket.BinaryFrame
	return newContextWorkspaceStream(ctx, stream), nil
}

func localWorkspacePermission(permission workspacesession.Permission) bool {
	switch permission {
	case workspacesession.PermissionShell, workspacesession.PermissionExec, workspacesession.PermissionVSCode,
		workspacesession.PermissionSSHProxy, workspacesession.PermissionPortForward, workspacesession.PermissionFileTransfer:
		return true
	default:
		return false
	}
}

func localWorkspaceAgentWebSocketConfig(workspaceID, origin, ticket string, permission workspacesession.Permission, port uint16) (*websocket.Config, error) {
	if !validWorkspaceHookVMID(workspaceID) || !validGatewayOrigin(origin) ||
		!validWorkspaceHeaderCredential(ticket, maxAgentTicketBytes) || !localWorkspacePermission(permission) ||
		(permission == workspacesession.PermissionPortForward && port == 0) ||
		(permission == workspacesession.PermissionSSHProxy && port != 22) ||
		(permission != workspacesession.PermissionPortForward && permission != workspacesession.PermissionSSHProxy && port != 0) {
		return nil, ErrInteractiveChannelUnavailable
	}
	channel := "shell"
	switch permission {
	case workspacesession.PermissionExec:
		channel = "exec"
	case workspacesession.PermissionVSCode:
		channel = "vscode"
	case workspacesession.PermissionSSHProxy:
		channel = "ssh"
	case workspacesession.PermissionPortForward:
		channel = "ports/" + strconv.Itoa(int(port))
	case workspacesession.PermissionFileTransfer:
		channel = "files"
	}
	endpoint := "ws://workspace-agent.vsock/v1/workspaces/" + url.PathEscape(workspaceID) + "/" + channel
	config, err := websocket.NewConfig(endpoint, origin)
	if err != nil {
		return nil, errors.New("configure local Workspace Agent WebSocket")
	}
	config.Protocol = []string{workspaceAgentWebSocketSubprotocol}
	config.Header.Set("Authorization", "Bearer "+ticket)
	return config, nil
}

type contextWorkspaceStream struct {
	stream io.ReadWriteCloser
	stop   func() bool
}

func newContextWorkspaceStream(ctx context.Context, stream io.ReadWriteCloser) io.ReadWriteCloser {
	bound := &contextWorkspaceStream{stream: stream}
	bound.stop = context.AfterFunc(ctx, func() { _ = stream.Close() })
	return bound
}

func (s *contextWorkspaceStream) Read(p []byte) (int, error)  { return s.stream.Read(p) }
func (s *contextWorkspaceStream) Write(p []byte) (int, error) { return s.stream.Write(p) }
func (s *contextWorkspaceStream) Close() error {
	if s.stop != nil {
		s.stop()
	}
	return s.stream.Close()
}

var _ interface {
	OpenShell(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
	OpenExec(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
	OpenFileTransfer(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
	OpenVSCode(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
	OpenPort(context.Context, workspacesession.Claims, workspacesession.Permission, uint16) (io.ReadWriteCloser, error)
} = (*FirecrackerWorkspaceVsockStreamDirectory)(nil)
