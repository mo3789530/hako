//go:build linux

package workspaceagent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
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
type workspaceExecFactory func(WorkspaceMountSpec) (*ExecManager, error)

// WorkspaceShellAgent is the guest-side, one-Workspace endpoint. It accepts
// only short-lived Gateway-signed Agent tickets and starts fixed PTY shells;
// it never accepts a command, argv, environment, or destination from a client.
type WorkspaceShellAgent struct {
	mu            sync.RWMutex
	keys          map[string]ed25519.PublicKey
	gatewayOrigin string
	factory       workspaceShellFactory
	execFactory   workspaceExecFactory
	replay        AgentTicketReplayGuard
	identity      AgentTicket
	shells        *ShellManager
	files         *WorkspaceFiles
	vscodeDial    func(context.Context) (net.Conn, error)
	sshDial       func(context.Context) (net.Conn, error)
	portDial      func(context.Context, uint16) (net.Conn, error)
	service       http.Handler
	execs         *ExecManager
	previewApps   *PreviewApplicationManager
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
	return &WorkspaceShellAgent{keys: keyCopy, gatewayOrigin: origin, factory: factory,
		execFactory: func(spec WorkspaceMountSpec) (*ExecManager, error) { return NewExecManager(spec.MountPath) },
		vscodeDial: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{Timeout: workspaceAgentConnectTimeout}).DialContext(ctx, "tcp", "127.0.0.1:8081")
		}, sshDial: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{Timeout: workspaceAgentConnectTimeout}).DialContext(ctx, "tcp", "127.0.0.1:22")
		}, portDial: func(ctx context.Context, port uint16) (net.Conn, error) {
			return (&net.Dialer{Timeout: workspaceAgentConnectTimeout}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
		}}, nil
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
	execs, err := a.execFactory(spec)
	if err != nil {
		_ = shells.Close()
		return errors.New("configure Workspace exec runtime")
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
	a.identity, a.shells, a.execs = binding, shells, execs
	return nil
}

// BindWorkspaceServiceRuntime enables the private, signed command endpoint
// after the MicroVM identity has been pinned by the Lambda /run hook.
func (a *WorkspaceShellAgent) BindWorkspaceServiceRuntime(engine ServiceEngine) error {
	if a == nil || engine == nil {
		return errors.New("Workspace Service engine is required")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.identity.WorkspaceID == "" || !validWorkspaceHookVMID(a.identity.MicroVMID) {
		return errors.New("Workspace Agent runtime must be bound before Service runtime")
	}
	if a.service != nil {
		return errors.New("Workspace Service runtime is already bound")
	}
	keys := make(map[string][]byte, len(a.keys))
	for keyID, key := range a.keys {
		keys[keyID] = append([]byte(nil), key...)
	}
	handler, err := NewWorkspaceServiceHandler(WorkspaceServiceIdentity{TenantID: a.identity.TenantID,
		WorkspaceID: a.identity.WorkspaceID, ResourcePlaneID: a.identity.ResourcePlane, MicroVMID: a.identity.MicroVMID}, keys, engine, nil)
	if err != nil {
		return err
	}
	a.service = handler
	return nil
}

func (a *WorkspaceShellAgent) BindWorkspaceFileRuntime(files *WorkspaceFiles) error {
	if a == nil || files == nil {
		return errors.New("Workspace file runtime is required")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.identity.WorkspaceID == "" || !validWorkspaceHookVMID(a.identity.MicroVMID) || a.files != nil {
		return errors.New("Workspace Agent must be bound before file runtime")
	}
	previewApps, err := NewPreviewApplicationManager(files.root)
	if err != nil {
		return errors.New("configure Preview application runtime")
	}
	a.files = files
	a.previewApps = previewApps
	return nil
}

func (a *WorkspaceShellAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/internal/heartbeat" {
		if r.Method != http.MethodGet || r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength > 0 {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.mu.RLock()
		identity, ready := a.identity, a.shells != nil && a.identity.WorkspaceID != ""
		a.mu.RUnlock()
		if !ready {
			http.Error(w, "Workspace Agent is not ready", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(workspaceAgentHeartbeat{ProtocolVersion: workspaceAgentProtocolVersion,
			TenantID: identity.TenantID, WorkspaceID: identity.WorkspaceID, ResourcePlaneID: identity.ResourcePlane, MicroVMID: identity.MicroVMID})
		return
	}
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
	switch r.URL.Path {
	case "/v1/workspaces/" + string(a.workspaceID()) + "/shell":
		a.serveShell(w, r)
	case "/v1/workspaces/" + string(a.workspaceID()) + "/exec":
		a.serveExec(w, r)
	case "/v1/workspaces/" + string(a.workspaceID()) + "/services/reconcile":
		a.mu.RLock()
		service := a.service
		a.mu.RUnlock()
		if service == nil {
			http.Error(w, "Workspace Service runtime is unavailable", http.StatusServiceUnavailable)
			return
		}
		service.ServeHTTP(w, r)
	case "/v1/workspaces/" + string(a.workspaceID()) + "/vscode":
		a.serveVSCode(w, r)
	case "/v1/workspaces/" + string(a.workspaceID()) + "/ssh":
		a.serveSSH(w, r)
	case "/v1/workspaces/" + string(a.workspaceID()) + "/files":
		a.serveFiles(w, r)
	default:
		const portSuffix = "/ports/"
		prefix := "/v1/workspaces/" + string(a.workspaceID()) + portSuffix
		if strings.HasPrefix(r.URL.Path, prefix) {
			a.servePort(w, r, strings.TrimPrefix(r.URL.Path, prefix))
			return
		}
		http.NotFound(w, r)
	}
}

func (a *WorkspaceShellAgent) serveExec(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	identity, execs := a.identity, a.execs
	a.mu.RUnlock()
	if execs == nil || identity.WorkspaceID == "" || r.Method != http.MethodGet || r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength > 0 {
		http.Error(w, "Workspace exec runtime is unavailable", http.StatusServiceUnavailable)
		return
	}
	token := bearerAgentTicket(r.Header.Get("Authorization"))
	server := websocket.Server{
		Config: websocket.Config{Protocol: []string{workspaceAgentWebSocketProtocol}},
		Handshake: func(config *websocket.Config, request *http.Request) error {
			if request.Method != http.MethodGet || request.URL.Path != r.URL.Path || request.URL.RawQuery != "" ||
				request.Header.Get("Origin") != a.gatewayOrigin || !offeredAgentProtocol(request.Header.Get("Sec-WebSocket-Protocol")) || token == "" {
				return ErrAgentTicket
			}
			expected := AgentTicket{TenantID: identity.TenantID, WorkspaceID: identity.WorkspaceID,
				ResourcePlane: identity.ResourcePlane, MicroVMID: identity.MicroVMID, Permission: workspacesession.PermissionExec}
			verified, err := VerifyAgentTicket(token, a.keys, expected, time.Now().UTC())
			if err != nil || a.replay.Consume(verified, time.Now().UTC()) != nil {
				return ErrAgentTicket
			}
			config.Protocol = []string{workspaceAgentWebSocketProtocol}
			return nil
		},
		Handler: func(conn *websocket.Conn) {
			conn.MaxPayloadBytes = workspaceprotocol.MaxExecRequest + 1024
			conn.PayloadType = websocket.BinaryFrame
			var payload []byte
			if err := websocket.Message.Receive(conn, &payload); err != nil || len(payload) == 0 || len(payload) > workspaceprotocol.MaxExecRequest {
				_ = conn.Close()
				return
			}
			var request workspaceprotocol.ExecRequest
			decoder := json.NewDecoder(bytes.NewReader(payload))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || workspaceprotocol.ValidateExecRequest(request) != nil {
				_ = conn.Close()
				return
			}
			result, err := execs.Run(r.Context(), request)
			if err != nil {
				result = workspaceprotocol.ExecResult{ExitCode: -1, Code: "exec_failed"}
			}
			response, marshalErr := json.Marshal(result)
			if marshalErr != nil || workspaceprotocol.ValidateExecResult(result) != nil || websocket.Message.Send(conn, response) != nil {
				_ = conn.Close()
			}
		},
	}
	server.ServeHTTP(w, r)
}

func (a *WorkspaceShellAgent) serveFiles(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	identity, files, previewApps := a.identity, a.files, a.previewApps
	a.mu.RUnlock()
	if files == nil || r.Method != http.MethodGet || r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength > 0 {
		http.Error(w, "Workspace file transfer is unavailable", http.StatusServiceUnavailable)
		return
	}
	token := bearerAgentTicket(r.Header.Get("Authorization"))
	server := websocket.Server{
		Config: websocket.Config{Protocol: []string{workspaceAgentWebSocketProtocol}},
		Handshake: func(config *websocket.Config, request *http.Request) error {
			if request.Method != http.MethodGet || request.URL.Path != r.URL.Path || request.URL.RawQuery != "" ||
				request.Header.Get("Origin") != a.gatewayOrigin || !offeredAgentProtocol(request.Header.Get("Sec-WebSocket-Protocol")) || token == "" {
				return ErrAgentTicket
			}
			expected := AgentTicket{TenantID: identity.TenantID, WorkspaceID: identity.WorkspaceID,
				ResourcePlane: identity.ResourcePlane, MicroVMID: identity.MicroVMID, Permission: workspacesession.PermissionFileTransfer}
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
			request, err := workspaceprotocol.ReadFileRequest(conn)
			if err != nil {
				_ = conn.Close()
				return
			}
			switch request.Operation {
			case "install_archive":
				err := files.InstallSourceArchive(r.Context(), request.Size, request.Digest, io.LimitReader(conn, request.Size))
				response := workspaceprotocol.FileResponse{OK: err == nil, Size: request.Size}
				if err != nil {
					response = workspaceprotocol.FileResponse{Code: "transfer_failed"}
				}
				_ = workspaceprotocol.WriteFileResponse(conn, response)
			case "start_preview":
				port, err := previewApps.Start(r.Context(), files)
				response := workspaceprotocol.FileResponse{OK: err == nil, Port: port}
				if err != nil {
					response = workspaceprotocol.FileResponse{Code: "transfer_failed"}
				}
				_ = workspaceprotocol.WriteFileResponse(conn, response)
			case "preview_logs":
				if previewApps == nil {
					_ = workspaceprotocol.WriteFileResponse(conn, workspaceprotocol.FileResponse{Code: "not_found"})
					return
				}
				logs := previewApps.Logs()
				if err := workspaceprotocol.WriteFileResponse(conn, workspaceprotocol.FileResponse{OK: true, Size: int64(len(logs))}); err != nil {
					_ = conn.Close()
					return
				}
				_, _ = conn.Write(logs)
			case "upload":
				err := files.Write(r.Context(), request.Path, request.Size, conn)
				response := workspaceprotocol.FileResponse{OK: err == nil, Size: request.Size}
				if err != nil {
					response = workspaceprotocol.FileResponse{Code: "transfer_failed"}
				}
				_ = workspaceprotocol.WriteFileResponse(conn, response)
			case "download":
				size, err := files.Size(request.Path)
				if err != nil {
					_ = workspaceprotocol.WriteFileResponse(conn, workspaceprotocol.FileResponse{Code: "not_found"})
					return
				}
				if err := workspaceprotocol.WriteFileResponse(conn, workspaceprotocol.FileResponse{OK: true, Size: size}); err != nil {
					_ = conn.Close()
					return
				}
				if _, err := files.Read(r.Context(), request.Path, conn); err != nil {
					_ = conn.Close()
				}
			}
		},
	}
	server.ServeHTTP(w, r)
}

func (a *WorkspaceShellAgent) servePort(w http.ResponseWriter, r *http.Request, rawPort string) {
	portValue, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || portValue == 0 || strconv.FormatUint(portValue, 10) != rawPort {
		http.Error(w, "invalid Workspace route", http.StatusBadRequest)
		return
	}
	a.mu.RLock()
	identity, dial := a.identity, a.portDial
	a.mu.RUnlock()
	port := uint16(portValue)
	if identity.WorkspaceID == "" || dial == nil || r.Method != http.MethodGet || r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength > 0 {
		http.Error(w, "Workspace port-forward bridge is not ready", http.StatusServiceUnavailable)
		return
	}
	token := bearerAgentTicket(r.Header.Get("Authorization"))
	var verified AgentTicket
	server := websocket.Server{
		Config: websocket.Config{Protocol: []string{workspaceAgentWebSocketProtocol}},
		Handshake: func(config *websocket.Config, request *http.Request) error {
			if request.Method != http.MethodGet || request.URL.Path != r.URL.Path || request.URL.RawQuery != "" ||
				request.Header.Get("Origin") != a.gatewayOrigin || !offeredAgentProtocol(request.Header.Get("Sec-WebSocket-Protocol")) || token == "" {
				return ErrAgentTicket
			}
			expected := AgentTicket{TenantID: identity.TenantID, WorkspaceID: identity.WorkspaceID,
				ResourcePlane: identity.ResourcePlane, MicroVMID: identity.MicroVMID,
				Permission: workspacesession.PermissionPortForward, Port: port}
			var verifyErr error
			verified, verifyErr = VerifyAgentTicket(token, a.keys, expected, time.Now().UTC())
			if verifyErr != nil || a.replay.Consume(verified, time.Now().UTC()) != nil {
				return ErrAgentTicket
			}
			config.Protocol = []string{workspaceAgentWebSocketProtocol}
			return nil
		},
		Handler: func(conn *websocket.Conn) {
			conn.MaxPayloadBytes = maxTerminalFrame
			conn.PayloadType = websocket.BinaryFrame
			upstream, err := dial(r.Context(), verified.Port)
			if err != nil {
				_ = conn.Close()
				return
			}
			proxyWorkspaceRawStream(r.Context(), conn, upstream)
		},
	}
	server.ServeHTTP(w, r)
}

func (a *WorkspaceShellAgent) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	shells := a.shells
	files := a.files
	previewApps := a.previewApps
	a.shells = nil
	a.files = nil
	a.execs = nil
	a.previewApps = nil
	a.mu.Unlock()
	var err error
	if shells != nil {
		err = shells.Close()
	}
	if files != nil {
		if fileErr := files.Close(); err == nil {
			err = fileErr
		}
	}
	if previewApps != nil {
		if stopErr := previewApps.Stop(context.Background()); err == nil {
			err = stopErr
		}
	}
	return err
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
	var ticketUser string
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
			ticketUser = string(verified.UserID)
			config.Protocol = []string{workspaceAgentWebSocketProtocol}
			return nil
		},
		Handler: func(conn *websocket.Conn) {
			conn.MaxPayloadBytes = maxTerminalFrame
			conn.PayloadType = websocket.BinaryFrame
			var first []byte
			if err := websocket.Message.Receive(conn, &first); err != nil {
				_ = conn.Close()
				return
			}
			frames, err := new(workspaceprotocol.Decoder).Feed(first)
			if err != nil || len(frames) != 1 || !frames[0].Attach {
				_ = conn.Close()
				return
			}
			var session *ShellSession
			if frames[0].Session == "" {
				session, err = shells.OpenOwned(context.Background(), TerminalSize{Columns: 120, Rows: 40}, ticketUser)
			} else {
				session, err = shells.AttachOwned(frames[0].Session, ticketUser)
			}
			if err != nil {
				_ = conn.Close()
				return
			}
			ack, err := workspaceprotocol.EncodeSession(session.ID())
			if err != nil || websocket.Message.Send(conn, ack) != nil {
				session.Detach()
				_ = conn.Close()
				return
			}
			proxyWorkspaceShell(r.Context(), conn, session.Attachment())
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

func (a *WorkspaceShellAgent) serveVSCode(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	identity, dial := a.identity, a.vscodeDial
	a.mu.RUnlock()
	if identity.WorkspaceID == "" || dial == nil || r.Method != http.MethodGet || r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength > 0 {
		http.Error(w, "Workspace VS Code bridge is not ready", http.StatusServiceUnavailable)
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
				ResourcePlane: identity.ResourcePlane, MicroVMID: identity.MicroVMID, Permission: workspacesession.PermissionVSCode}
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
			upstream, err := dial(r.Context())
			if err != nil {
				_ = conn.Close()
				return
			}
			proxyWorkspaceRawStream(r.Context(), conn, upstream)
		},
	}
	server.ServeHTTP(w, r)
}

// serveSSH bridges a one-use SSH-proxy ticket to the fixed guest loopback SSH
// endpoint. The client never chooses a destination and port 22 is never
// exposed on the MicroVM's external interface.
func (a *WorkspaceShellAgent) serveSSH(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	identity, dial := a.identity, a.sshDial
	a.mu.RUnlock()
	if identity.WorkspaceID == "" || dial == nil || r.Method != http.MethodGet || r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength > 0 {
		http.Error(w, "Workspace SSH bridge is not ready", http.StatusServiceUnavailable)
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
				ResourcePlane: identity.ResourcePlane, MicroVMID: identity.MicroVMID, Permission: workspacesession.PermissionSSHProxy}
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
			upstream, err := dial(r.Context())
			if err != nil {
				_ = conn.Close()
				return
			}
			proxyWorkspaceRawStream(r.Context(), conn, upstream)
		},
	}
	server.ServeHTTP(w, r)
}

func proxyWorkspaceRawStream(ctx context.Context, conn *websocket.Conn, upstream io.ReadWriteCloser) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan error, 2)
	go func() {
		for {
			if ctx.Err() != nil {
				finished <- ctx.Err()
				return
			}
			var frame []byte
			if err := websocket.Message.Receive(conn, &frame); err != nil {
				finished <- err
				return
			}
			for len(frame) > 0 {
				n, err := upstream.Write(frame)
				if err != nil || n <= 0 || n > len(frame) {
					if err != nil {
						finished <- err
					} else {
						finished <- io.ErrShortWrite
					}
					return
				}
				frame = frame[n:]
			}
		}
	}()
	go func() {
		buffer := make([]byte, maxTerminalFrame)
		for {
			if ctx.Err() != nil {
				finished <- ctx.Err()
				return
			}
			n, err := upstream.Read(buffer)
			if n > 0 {
				if sendErr := websocket.Message.Send(conn, buffer[:n]); sendErr != nil {
					finished <- sendErr
					return
				}
			}
			if err != nil {
				finished <- err
				return
			}
			if n == 0 {
				finished <- io.ErrNoProgress
				return
			}
		}
	}()
	<-finished
	cancel()
	_ = upstream.Close()
	_ = conn.Close()
	select {
	case <-finished:
	case <-time.After(500 * time.Millisecond):
	}
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
			if decoded.Attach || decoded.Session != "" || decoded.End {
				return workspaceprotocol.ErrInvalidFrame
			}
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
			if errors.Is(err, io.EOF) && ctx.Err() == nil {
				_ = websocket.Message.Send(conn, workspaceprotocol.EncodeEnd())
			}
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
}
