//go:build linux

package workspaceagent

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspaceprotocol"
	"github.com/mo3789530/hako/internal/workspacesession"
	"golang.org/x/net/websocket"
)

const workspaceAgentWebSocketProtocol = "hako.workspace.agent.v1"

type workspaceShellFactory func(WorkspaceMountSpec) (*ShellManager, error)

// WorkspaceShellAgent is the guest-side, one-Workspace endpoint. It accepts
// only short-lived Gateway-signed Agent tickets and starts fixed PTY shells;
// it never accepts a command, argv, environment, or destination from a client.
type WorkspaceShellAgent struct {
	mu            sync.RWMutex
	keys          map[string]ed25519.PublicKey
	gatewayOrigin string
	factory       workspaceShellFactory
	replay        AgentTicketReplayGuard
	identity      AgentTicket
	shells        *ShellManager
}

func NewWorkspaceShellAgent(keys map[string]ed25519.PublicKey, gatewayOrigin string) (*WorkspaceShellAgent, error) {
	return newWorkspaceShellAgent(keys, gatewayOrigin, func(spec WorkspaceMountSpec) (*ShellManager, error) {
		return NewShellManager(ShellConfig{WorkspaceRoot: spec.MountPath, RunAsUID: spec.UID, RunAsGID: spec.GID})
	})
}

func newWorkspaceShellAgent(keys map[string]ed25519.PublicKey, gatewayOrigin string, factory workspaceShellFactory) (*WorkspaceShellAgent, error) {
	origin := strings.TrimSpace(gatewayOrigin)
	if len(keys) == 0 || len(keys) > 32 || origin == "" || factory == nil {
		return nil, errors.New("Workspace Agent keys, Gateway origin, and shell factory are required")
	}
	keyCopy := make(map[string]ed25519.PublicKey, len(keys))
	for id, key := range keys {
		if !agentIdentityPattern.MatchString(id) || len(key) != ed25519.PublicKeySize {
			return nil, errors.New("Workspace Agent trusted key set is invalid")
		}
		keyCopy[id] = append(ed25519.PublicKey(nil), key...)
	}
	return &WorkspaceShellAgent{keys: keyCopy, gatewayOrigin: origin, factory: factory}, nil
}

// BindWorkspaceRuntime pins this guest to the provider ID and Workspace
// identity before the MicroVM /run hook is acknowledged.
func (a *WorkspaceShellAgent) BindWorkspaceRuntime(microVMID string, spec WorkspaceMountSpec) error {
	if a == nil || !validWorkspaceHookVMID(microVMID) || !validWorkspaceMountSpec(spec) {
		return errors.New("Workspace Agent runtime binding is invalid")
	}
	shells, err := a.factory(spec)
	if err != nil || shells == nil {
		return errors.New("configure Workspace PTY runtime")
	}
	binding := AgentTicket{TenantID: domain.TenantID(spec.TenantID), WorkspaceID: domain.WorkspaceID(spec.WorkspaceID),
		ResourcePlane: domain.ResourcePlaneID(spec.ResourcePlaneID), MicroVMID: microVMID}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.shells != nil {
		_ = shells.Close()
		if a.identity.TenantID != binding.TenantID || a.identity.WorkspaceID != binding.WorkspaceID ||
			a.identity.ResourcePlane != binding.ResourcePlane || a.identity.MicroVMID != binding.MicroVMID {
			return errors.New("Workspace Agent is already bound to another runtime")
		}
		return nil
	}
	a.identity, a.shells = binding, shells
	return nil
}

func (a *WorkspaceShellAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/internal/ready" {
		if r.Method != http.MethodGet || r.URL.RawQuery != "" || r.ContentLength > 0 {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.mu.RLock()
		ready := a.shells != nil && a.identity.WorkspaceID != ""
		a.mu.RUnlock()
		if !ready {
			http.Error(w, "Workspace Agent is not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path != "/v1/workspaces/"+string(a.workspaceID())+"/shell" {
		http.NotFound(w, r)
		return
	}
	a.serveShell(w, r)
}

func (a *WorkspaceShellAgent) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	shells := a.shells
	a.shells = nil
	a.mu.Unlock()
	if shells == nil {
		return nil
	}
	return shells.Close()
}

func (a *WorkspaceShellAgent) workspaceID() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return string(a.identity.WorkspaceID)
}

func (a *WorkspaceShellAgent) serveShell(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	identity, shells := a.identity, a.shells
	a.mu.RUnlock()
	if shells == nil || r.Method != http.MethodGet || r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength > 0 {
		http.Error(w, "Workspace Agent is not ready", http.StatusServiceUnavailable)
		return
	}
	token := bearerAgentTicket(r.Header.Get("Authorization"))
	server := websocket.Server{
		Config: websocket.Config{Protocol: []string{workspaceAgentWebSocketProtocol}},
		Handshake: func(config *websocket.Config, request *http.Request) error {
			if request.Method != http.MethodGet || request.URL.RawQuery != "" || request.Header.Get("Origin") != a.gatewayOrigin ||
				!offeredAgentProtocol(request.Header.Get("Sec-WebSocket-Protocol")) || token == "" {
				return ErrAgentTicket
			}
			expected := AgentTicket{TenantID: identity.TenantID, WorkspaceID: identity.WorkspaceID,
				ResourcePlane: identity.ResourcePlane, MicroVMID: identity.MicroVMID, Permission: workspacesession.PermissionShell}
			verified, err := VerifyAgentTicket(token, a.keys, expected, time.Now().UTC())
			if err != nil || a.replay.Consume(verified, time.Now().UTC()) != nil {
				return ErrAgentTicket
			}
			config.Protocol = []string{workspaceAgentWebSocketProtocol}
			return nil
		},
		Handler: func(conn *websocket.Conn) {
			conn.MaxPayloadBytes = maxTerminalFrame
			conn.PayloadType = websocket.BinaryFrame
			session, err := shells.Open(r.Context(), TerminalSize{Columns: 120, Rows: 40})
			if err != nil {
				_ = conn.Close()
				return
			}
			proxyWorkspaceShell(r.Context(), conn, session)
			_ = session.Close()
		},
	}
	server.ServeHTTP(w, r)
}

func bearerAgentTicket(value string) string {
	parts := strings.Fields(value)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.ContainsAny(parts[1], "\r\n") {
		return ""
	}
	return parts[1]
}

func offeredAgentProtocol(value string) bool {
	for _, protocol := range strings.Split(value, ",") {
		if strings.TrimSpace(protocol) == workspaceAgentWebSocketProtocol {
			return true
		}
	}
	return false
}

type interactiveShell interface {
	io.ReadWriteCloser
	Resize(TerminalSize) error
}

func proxyWorkspaceShell(ctx context.Context, conn *websocket.Conn, shell interactiveShell) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan error, 2)
	go func() { finished <- copyAgentFramesToShell(ctx, conn, shell) }()
	go func() { finished <- copyShellToAgentFrames(ctx, shell, conn) }()
	select {
	case <-ctx.Done():
	case <-finished:
	}
	cancel()
	_ = shell.Close()
	_ = conn.Close()
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C:
	}
}

func copyAgentFramesToShell(ctx context.Context, conn *websocket.Conn, shell interactiveShell) error {
	var decoder workspaceprotocol.Decoder
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var frame []byte
		if err := websocket.Message.Receive(conn, &frame); err != nil {
			return err
		}
		decodedFrames, err := decoder.Feed(frame)
		if err != nil {
			return err
		}
		for _, decoded := range decodedFrames {
			if decoded.Resize {
				if err := shell.Resize(TerminalSize{Columns: decoded.Columns, Rows: decoded.Rows}); err != nil {
					return err
				}
				continue
			}
			for data := decoded.Data; len(data) > 0; {
				n, err := shell.Write(data)
				if err != nil || n <= 0 || n > len(data) {
					if err != nil {
						return err
					}
					return io.ErrShortWrite
				}
				data = data[n:]
			}
		}
	}
}

func copyShellToAgentFrames(ctx context.Context, shell io.Reader, conn *websocket.Conn) error {
	buffer := make([]byte, workspaceprotocol.MaxFrame-workspaceprotocol.HeaderSize)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := shell.Read(buffer)
		if n > 0 {
			frame, encodeErr := workspaceprotocol.EncodeData(buffer[:n])
			if encodeErr != nil {
				return encodeErr
			}
			if sendErr := websocket.Message.Send(conn, frame); sendErr != nil {
				return sendErr
			}
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
}
