//go:build linux

package workspaceagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/workspaceprotocol"
	"github.com/mo3789530/hako/internal/workspacesession"
	"golang.org/x/net/websocket"
)

func TestWorkspaceShellAgentRequiresPinnedTicketAndRelaysToPTY(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	origin := "https://gateway.hako.test"
	root := t.TempDir()
	uid, gid := testShellIdentity(t, root)
	agent, err := newWorkspaceShellAgent(map[string]ed25519.PublicKey{"rp-key-1": public}, origin,
		func(spec WorkspaceMountSpec) (*ShellManager, error) {
			return NewShellManager(ShellConfig{WorkspaceRoot: root, RunAsUID: uid, RunAsGID: gid, MaxSessions: 1, SessionTTL: time.Minute})
		})
	if err != nil {
		t.Fatal(err)
	}
	bindWorkspaceTestExecRoot(agent, root)
	spec := testMountSpec()
	if err := agent.BindWorkspaceRuntime("microvm-1", spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.mu.RLock(); manager := agent.shells; agent.mu.RUnlock(); _ = manager.Close() })
	server := httptest.NewServer(agent)
	defer server.Close()
	issuer := &AgentTicketIssuer{KeyID: "rp-key-1", PrivateKey: private, TTL: time.Minute}
	input := AgentTicket{UserID: "user-1", TenantID: "tenant-1", WorkspaceID: "workspace-1", ResourcePlane: "rp-1",
		MicroVMID: "microvm-1", Permission: workspacesession.PermissionShell}
	ticket, _, err := issuer.Issue(input)
	if err != nil {
		t.Fatal(err)
	}
	client, err := dialWorkspaceAgent(server.URL, origin, ticket)
	if err != nil {
		t.Fatalf("connect with valid single-use Agent ticket: %v", err)
	}
	attach, err := workspaceprotocol.EncodeAttach("")
	if err != nil || websocket.Message.Send(client, attach) != nil {
		t.Fatalf("send terminal attach: %v", err)
	}
	var sessionFrame []byte
	if err := websocket.Message.Receive(client, &sessionFrame); err != nil {
		t.Fatalf("receive terminal session ID: %v", err)
	}
	sessionAck, err := workspaceprotocol.Decode(sessionFrame)
	if err != nil || sessionAck.Session == "" {
		t.Fatalf("terminal session acknowledgement = %+v, %v", sessionAck, err)
	}
	resizeFrame, err := workspaceprotocol.EncodeResize(100, 32)
	if err != nil {
		t.Fatal(err)
	}
	if err := websocket.Message.Send(client, resizeFrame); err != nil {
		t.Fatal(err)
	}
	dataFrame, err := workspaceprotocol.EncodeData([]byte("stty size; sleep 0.3; printf 'hako-agent-pty-ok\\n'; exit\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := websocket.Message.Send(client, dataFrame); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	resumedTicket, _, err := issuer.Issue(input)
	if err != nil {
		t.Fatal(err)
	}
	client, err = dialWorkspaceAgent(server.URL, origin, resumedTicket)
	if err != nil {
		t.Fatalf("reconnect with a fresh single-use ticket: %v", err)
	}
	defer client.Close()
	resumeFrame, err := workspaceprotocol.EncodeAttach(sessionAck.Session)
	if err != nil || websocket.Message.Send(client, resumeFrame) != nil {
		t.Fatalf("send terminal resume: %v", err)
	}
	var resumedAck []byte
	if err := websocket.Message.Receive(client, &resumedAck); err != nil {
		t.Fatalf("receive terminal resume acknowledgement: %v", err)
	}
	ack, err := workspaceprotocol.Decode(resumedAck)
	if err != nil || ack.Session != sessionAck.Session {
		t.Fatalf("resumed terminal session = %+v, %v", ack, err)
	}
	var output strings.Builder
	deadline := time.After(3 * time.Second)
	for !strings.Contains(output.String(), "hako-agent-pty-ok") {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for detached PTY output: %q", output.String())
		default:
		}
		var frame []byte
		if err := websocket.Message.Receive(client, &frame); err != nil {
			t.Fatalf("receive resumed PTY frame: %v output=%q", err, output.String())
		}
		frames, err := new(workspaceprotocol.Decoder).Feed(frame)
		if err != nil {
			t.Fatalf("decode resumed PTY frame: %v", err)
		}
		for _, decoded := range frames {
			output.Write(decoded.Data)
		}
	}
	if !strings.Contains(output.String(), "hako-agent-pty-ok") {
		t.Fatalf("detached shell output was not retained: %q", output.String())
	}
	if replay, err := dialWorkspaceAgent(server.URL, origin, ticket); err == nil {
		_ = replay.Close()
		t.Fatal("replayed Agent ticket was accepted")
	}
}

func TestWorkspaceShellAgentExecRequiresDedicatedOneUseTicket(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	origin := "https://gateway.hako.test"
	root := t.TempDir()
	uid, gid := testShellIdentity(t, root)
	agent, err := newWorkspaceShellAgent(map[string]ed25519.PublicKey{"rp-key-exec": public}, origin,
		func(spec WorkspaceMountSpec) (*ShellManager, error) {
			return NewShellManager(ShellConfig{WorkspaceRoot: root, RunAsUID: uid, RunAsGID: gid})
		})
	if err != nil {
		t.Fatal(err)
	}
	bindWorkspaceTestExecRoot(agent, root)
	if err := agent.BindWorkspaceRuntime("microvm-exec", testMountSpec()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	heartbeat := httptest.NewRecorder()
	agent.ServeHTTP(heartbeat, httptest.NewRequest(http.MethodGet, "http://agent/internal/heartbeat", nil))
	var heartbeatPayload workspaceAgentHeartbeat
	if heartbeat.Code != http.StatusOK || json.Unmarshal(heartbeat.Body.Bytes(), &heartbeatPayload) != nil ||
		heartbeatPayload.ProtocolVersion != workspaceAgentProtocolVersion || heartbeatPayload.TenantID != "tenant-1" ||
		heartbeatPayload.WorkspaceID != "workspace-1" || heartbeatPayload.ResourcePlaneID != "rp-1" || heartbeatPayload.MicroVMID != "microvm-exec" {
		t.Fatalf("Guest Agent heartbeat did not report its pinned identity: status=%d body=%q heartbeat=%+v", heartbeat.Code, heartbeat.Body.String(), heartbeatPayload)
	}
	server := httptest.NewServer(agent)
	defer server.Close()
	issuer := &AgentTicketIssuer{KeyID: "rp-key-exec", PrivateKey: private, TTL: time.Minute}
	issue := func(permission workspacesession.Permission) string {
		t.Helper()
		ticket, _, err := issuer.Issue(AgentTicket{UserID: "user-exec", TenantID: "tenant-1", WorkspaceID: "workspace-1",
			ResourcePlane: "rp-1", MicroVMID: "microvm-exec", Permission: permission})
		if err != nil {
			t.Fatal(err)
		}
		return ticket
	}
	connection, err := dialWorkspaceAgentChannel(server.URL, origin, issue(workspacesession.PermissionExec), "exec")
	if err != nil {
		t.Fatalf("connect with dedicated exec ticket: %v", err)
	}
	request, err := json.Marshal(workspaceprotocol.ExecRequest{Argv: []string{"/usr/bin/printf", "guest-exec-ok"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := websocket.Message.Send(connection, request); err != nil {
		t.Fatalf("send exec request: %v", err)
	}
	var payload []byte
	if err := websocket.Message.Receive(connection, &payload); err != nil {
		t.Fatalf("receive exec result: %v", err)
	}
	var result workspaceprotocol.ExecResult
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode exec result: %v", err)
	}
	if result.ExitCode != 0 || string(result.Stdout) != "guest-exec-ok" || len(result.Stderr) != 0 || result.Code != "" {
		t.Fatalf("unexpected exec result: %+v stdout=%q stderr=%q", result, result.Stdout, result.Stderr)
	}
	_ = connection.Close()

	if unauthorized, err := dialWorkspaceAgentChannel(server.URL, origin, issue(workspacesession.PermissionShell), "exec"); err == nil {
		_ = unauthorized.Close()
		t.Fatal("shell ticket was accepted on the exec route")
	}
	replayTicket := issue(workspacesession.PermissionExec)
	first, err := dialWorkspaceAgentChannel(server.URL, origin, replayTicket, "exec")
	if err != nil {
		t.Fatalf("first use of exec ticket: %v", err)
	}
	_ = first.Close()
	if replay, err := dialWorkspaceAgentChannel(server.URL, origin, replayTicket, "exec"); err == nil {
		_ = replay.Close()
		t.Fatal("replayed exec ticket was accepted")
	}
}

func TestWorkspaceShellAgentStartsPreviewThroughOneUseFileTransferTicket(t *testing.T) {
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("/usr/bin/python3 is required for the Preview Agent integration test")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src", ".hako"), 0700); err != nil {
		t.Fatal(err)
	}
	manifest := "version: 1\ncommand:\n  - python3\n  - -c\n  - |\n    import socket\n    import sys\n    print('preview-ready', flush=True)\n    s = socket.socket()\n    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)\n    s.bind(('127.0.0.1', 3000))\n    s.listen()\n    while True:\n      c, _ = s.accept()\n      c.close()\n"
	if err := os.WriteFile(filepath.Join(root, "src", ".hako", "preview.yaml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	uid, gid := testShellIdentity(t, root)
	agent, err := newWorkspaceShellAgent(map[string]ed25519.PublicKey{"rp-preview": public}, "https://gateway.hako.test",
		func(spec WorkspaceMountSpec) (*ShellManager, error) {
			return NewShellManager(ShellConfig{WorkspaceRoot: root, RunAsUID: uid, RunAsGID: gid})
		})
	if err != nil {
		t.Fatal(err)
	}
	bindWorkspaceTestExecRoot(agent, root)
	if err := agent.BindWorkspaceRuntime("microvm-preview", testMountSpec()); err != nil {
		t.Fatal(err)
	}
	files, err := newWorkspaceFilesAt(root, uid, gid)
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.BindWorkspaceFileRuntime(files); err != nil {
		_ = files.Close()
		t.Fatal(err)
	}
	server := httptest.NewServer(agent)
	defer server.Close()
	issuer := &AgentTicketIssuer{KeyID: "rp-preview", PrivateKey: private, TTL: time.Minute}
	ticket, _, err := issuer.Issue(AgentTicket{UserID: "controller", TenantID: "tenant-1", WorkspaceID: "workspace-1",
		ResourcePlane: "rp-1", MicroVMID: "microvm-preview", Permission: workspacesession.PermissionFileTransfer})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := dialWorkspaceAgentChannel(server.URL, "https://gateway.hako.test", ticket, "files")
	if err != nil {
		t.Fatal(err)
	}
	port, err := StartPreviewApplication(context.Background(), stream)
	_ = stream.Close()
	if err != nil || port != PreviewApplicationPort {
		_ = agent.Close()
		t.Fatalf("start Guest Preview application port=%d error=%v", port, err)
	}
	logTicket, _, err := issuer.Issue(AgentTicket{UserID: "controller", TenantID: "tenant-1", WorkspaceID: "workspace-1",
		ResourcePlane: "rp-1", MicroVMID: "microvm-preview", Permission: workspacesession.PermissionFileTransfer})
	if err != nil {
		_ = agent.Close()
		t.Fatal(err)
	}
	logStream, err := dialWorkspaceAgentChannel(server.URL, "https://gateway.hako.test", logTicket, "files")
	if err != nil {
		_ = agent.Close()
		t.Fatal(err)
	}
	if err := workspaceprotocol.WriteFileRequest(logStream, workspaceprotocol.FileRequest{Operation: "preview_logs", Path: "/workspace/src/.hako/preview.log"}); err != nil {
		_ = logStream.Close()
		_ = agent.Close()
		t.Fatalf("request Preview logs: %v", err)
	}
	logResponse, err := workspaceprotocol.ReadFileResponse(logStream)
	if err != nil || !logResponse.OK || logResponse.Size == 0 || logResponse.Size > workspaceprotocol.PreviewLogsMaxBytes {
		_ = logStream.Close()
		_ = agent.Close()
		t.Fatalf("Preview log response=%+v error=%v", logResponse, err)
	}
	logBytes := make([]byte, int(logResponse.Size))
	if _, err := io.ReadFull(logStream, logBytes); err != nil || !strings.Contains(string(logBytes), "preview-ready") {
		_ = logStream.Close()
		_ = agent.Close()
		t.Fatalf("Preview logs=%q error=%v", logBytes, err)
	}
	_ = logStream.Close()
	connection, err := net.DialTimeout("tcp", "127.0.0.1:3000", time.Second)
	if err != nil {
		_ = agent.Close()
		t.Fatalf("Guest Preview did not listen on the authorized application port: %v", err)
	}
	_ = connection.Close()
	_ = agent.Close()
	if connection, err := net.DialTimeout("tcp", "127.0.0.1:3000", 100*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatal("Guest Agent close left Preview application running")
	}
}

func TestWorkspaceShellAgentSSHBridgeRequiresSSHProxyTicketAndRelaysRawBytes(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	uid, gid := testShellIdentity(t, root)
	agent, err := newWorkspaceShellAgent(map[string]ed25519.PublicKey{"rp-key-ssh": public}, "https://gateway.hako.test",
		func(spec WorkspaceMountSpec) (*ShellManager, error) {
			return NewShellManager(ShellConfig{WorkspaceRoot: root, RunAsUID: uid, RunAsGID: gid})
		})
	if err != nil {
		t.Fatal(err)
	}
	bindWorkspaceTestExecRoot(agent, root)
	if err := agent.BindWorkspaceRuntime("microvm-1", testMountSpec()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		agent.mu.RLock()
		manager := agent.shells
		agent.mu.RUnlock()
		_ = manager.Close()
	})
	var dials atomic.Int32
	agent.sshDial = func(context.Context) (net.Conn, error) {
		dials.Add(1)
		guest, peer := net.Pipe()
		go func() { _, _ = io.Copy(peer, peer); _ = peer.Close() }()
		return guest, nil
	}
	server := httptest.NewServer(agent)
	defer server.Close()
	issuer := &AgentTicketIssuer{KeyID: "rp-key-ssh", PrivateKey: private, TTL: time.Minute}
	sshTicket, _, err := issuer.Issue(AgentTicket{UserID: "user-1", TenantID: "tenant-1", WorkspaceID: "workspace-1",
		ResourcePlane: "rp-1", MicroVMID: "microvm-1", Permission: workspacesession.PermissionSSHProxy})
	if err != nil {
		t.Fatal(err)
	}
	client, err := dialWorkspaceAgentChannel(server.URL, "https://gateway.hako.test", sshTicket, "ssh")
	if err != nil {
		t.Fatalf("connect with SSH-proxy ticket: %v", err)
	}
	if err := websocket.Message.Send(client, []byte("ssh-handshake-bytes")); err != nil {
		t.Fatal(err)
	}
	var echoed []byte
	if err := websocket.Message.Receive(client, &echoed); err != nil || string(echoed) != "ssh-handshake-bytes" {
		t.Fatalf("SSH raw byte relay = %q, %v", echoed, err)
	}
	_ = client.Close()
	if replay, err := dialWorkspaceAgentChannel(server.URL, "https://gateway.hako.test", sshTicket, "ssh"); err == nil {
		_ = replay.Close()
		t.Fatal("replayed SSH Agent ticket was accepted")
	}
	shellTicket, _, err := issuer.Issue(AgentTicket{UserID: "user-1", TenantID: "tenant-1", WorkspaceID: "workspace-1",
		ResourcePlane: "rp-1", MicroVMID: "microvm-1", Permission: workspacesession.PermissionShell})
	if err != nil {
		t.Fatal(err)
	}
	if unauthorized, err := dialWorkspaceAgentChannel(server.URL, "https://gateway.hako.test", shellTicket, "ssh"); err == nil {
		_ = unauthorized.Close()
		t.Fatal("shell ticket was accepted on SSH bridge")
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("SSH bridge opened guest sshd %d times, want exactly one authorized stream", got)
	}
}

func TestWorkspaceShellAgentPortForwardTicketPinsLoopbackPort(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	uid, gid := testShellIdentity(t, root)
	agent, err := newWorkspaceShellAgent(map[string]ed25519.PublicKey{"rp-key-port": public}, "https://gateway.hako.test",
		func(spec WorkspaceMountSpec) (*ShellManager, error) {
			return NewShellManager(ShellConfig{WorkspaceRoot: root, RunAsUID: uid, RunAsGID: gid})
		})
	if err != nil {
		t.Fatal(err)
	}
	bindWorkspaceTestExecRoot(agent, root)
	if err := agent.BindWorkspaceRuntime("microvm-1", testMountSpec()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		agent.mu.RLock()
		manager := agent.shells
		agent.mu.RUnlock()
		_ = manager.Close()
	})
	var dialedPort atomic.Uint32
	agent.portDial = func(_ context.Context, port uint16) (net.Conn, error) {
		dialedPort.Store(uint32(port))
		guest, peer := net.Pipe()
		go func() { _, _ = io.Copy(peer, peer); _ = peer.Close() }()
		return guest, nil
	}
	server := httptest.NewServer(agent)
	defer server.Close()
	issuer := &AgentTicketIssuer{KeyID: "rp-key-port", PrivateKey: private, TTL: time.Minute}
	wrongTicket, _, err := issuer.Issue(AgentTicket{UserID: "user-1", TenantID: "tenant-1", WorkspaceID: "workspace-1",
		ResourcePlane: "rp-1", MicroVMID: "microvm-1", Permission: workspacesession.PermissionPortForward, Port: 5432})
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := dialWorkspaceAgentChannel(server.URL, "https://gateway.hako.test", wrongTicket, "ports/5433"); err == nil {
		_ = conn.Close()
		t.Fatal("port-5432 ticket was accepted on port-5433 route")
	}
	if got := dialedPort.Load(); got != 0 {
		t.Fatalf("mismatched port ticket reached loopback dialer at %d", got)
	}
	ticket, _, err := issuer.Issue(AgentTicket{UserID: "user-1", TenantID: "tenant-1", WorkspaceID: "workspace-1",
		ResourcePlane: "rp-1", MicroVMID: "microvm-1", Permission: workspacesession.PermissionPortForward, Port: 5432})
	if err != nil {
		t.Fatal(err)
	}
	client, err := dialWorkspaceAgentChannel(server.URL, "https://gateway.hako.test", ticket, "ports/5432")
	if err != nil {
		t.Fatalf("connect with port-bound Agent ticket: %v", err)
	}
	if err := websocket.Message.Send(client, []byte("postgres-protocol-bytes")); err != nil {
		t.Fatal(err)
	}
	var echoed []byte
	if err := websocket.Message.Receive(client, &echoed); err != nil || string(echoed) != "postgres-protocol-bytes" {
		t.Fatalf("port-forward raw-byte relay=%q err=%v", echoed, err)
	}
	_ = client.Close()
	if got := dialedPort.Load(); got != 5432 {
		t.Fatalf("guest dialed port %d, want fixed loopback port 5432", got)
	}
	for _, path := range []string{"ports/0", "ports/05432", "ports/65536", "ports/5432/extra"} {
		if conn, err := dialWorkspaceAgentChannel(server.URL, "https://gateway.hako.test", ticket, path); err == nil {
			_ = conn.Close()
			t.Errorf("accepted invalid Workspace port route %q", path)
		}
	}
}

func TestWorkspaceShellAgentRejectsForeignIdentityAndOrigin(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	uid, gid := testShellIdentity(t, root)
	agent, err := newWorkspaceShellAgent(map[string]ed25519.PublicKey{"rp-key": public}, "https://gateway.hako.test",
		func(WorkspaceMountSpec) (*ShellManager, error) {
			return NewShellManager(ShellConfig{WorkspaceRoot: root, RunAsUID: uid, RunAsGID: gid})
		})
	if err != nil {
		t.Fatal(err)
	}
	bindWorkspaceTestExecRoot(agent, root)
	if err := agent.BindWorkspaceRuntime("microvm-1", testMountSpec()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.mu.RLock(); manager := agent.shells; agent.mu.RUnlock(); _ = manager.Close() })
	server := httptest.NewServer(agent)
	defer server.Close()
	issuer := &AgentTicketIssuer{KeyID: "rp-key", PrivateKey: private, TTL: time.Minute}
	for name, input := range map[string]AgentTicket{
		"foreign Workspace": {UserID: "user-1", TenantID: "tenant-1", WorkspaceID: "workspace-2", ResourcePlane: "rp-1", MicroVMID: "microvm-1", Permission: workspacesession.PermissionShell},
		"foreign MicroVM":   {UserID: "user-1", TenantID: "tenant-1", WorkspaceID: "workspace-1", ResourcePlane: "rp-1", MicroVMID: "microvm-2", Permission: workspacesession.PermissionShell},
	} {
		t.Run(name, func(t *testing.T) {
			ticket, _, err := issuer.Issue(input)
			if err != nil {
				t.Fatal(err)
			}
			if conn, err := dialWorkspaceAgent(server.URL, "https://gateway.hako.test", ticket); err == nil {
				_ = conn.Close()
				t.Fatal("foreign identity accepted")
			}
		})
	}
	valid := AgentTicket{UserID: "user-1", TenantID: "tenant-1", WorkspaceID: "workspace-1", ResourcePlane: "rp-1", MicroVMID: "microvm-1", Permission: workspacesession.PermissionShell}
	ticket, _, err := issuer.Issue(valid)
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := dialWorkspaceAgent(server.URL, "https://attacker.hako.test", ticket); err == nil {
		_ = conn.Close()
		t.Fatal("foreign Gateway origin accepted")
	}
}

func TestWorkspaceAgentVSCodeRouteUsesDedicatedTicketAndFixedLoopbackBridge(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	uid, gid := testShellIdentity(t, root)
	agent, err := newWorkspaceShellAgent(map[string]ed25519.PublicKey{"rp-vscode": public}, "https://gateway.hako.test",
		func(WorkspaceMountSpec) (*ShellManager, error) {
			return NewShellManager(ShellConfig{WorkspaceRoot: root, RunAsUID: uid, RunAsGID: gid})
		})
	if err != nil {
		t.Fatal(err)
	}
	bindWorkspaceTestExecRoot(agent, root)
	if err := agent.BindWorkspaceRuntime("microvm-vscode", testMountSpec()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	local, codeServer := net.Pipe()
	defer codeServer.Close()
	var dialCalls atomic.Int32
	agent.vscodeDial = func(ctx context.Context) (net.Conn, error) {
		dialCalls.Add(1)
		return local, nil
	}
	go func() {
		defer codeServer.Close()
		request := make([]byte, len("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"))
		if _, err := io.ReadFull(codeServer, request); err == nil {
			_, _ = codeServer.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
		}
	}()
	server := httptest.NewServer(agent)
	defer server.Close()
	issuer := &AgentTicketIssuer{KeyID: "rp-vscode", PrivateKey: private, TTL: time.Minute}
	claims := AgentTicket{UserID: "user-1", TenantID: "tenant-1", WorkspaceID: "workspace-1", ResourcePlane: "rp-1",
		MicroVMID: "microvm-vscode", Permission: workspacesession.PermissionVSCode}
	ticket, _, err := issuer.Issue(claims)
	if err != nil {
		t.Fatal(err)
	}
	client, err := dialWorkspaceAgentChannel(server.URL, "https://gateway.hako.test", ticket, "vscode")
	if err != nil {
		t.Fatalf("connect with VS Code ticket: %v", err)
	}
	defer client.Close()
	request := []byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
	if err := websocket.Message.Send(client, request); err != nil {
		t.Fatal(err)
	}
	var response []byte
	if err := websocket.Message.Receive(client, &response); err != nil || !strings.Contains(string(response), "200 OK") || !strings.HasSuffix(string(response), "ok") {
		t.Fatalf("VS Code bridge response=%q err=%v", response, err)
	}
	if dialCalls.Load() != 1 {
		t.Fatalf("fixed local VS Code bridge dial count=%d", dialCalls.Load())
	}
	if replay, err := dialWorkspaceAgentChannel(server.URL, "https://gateway.hako.test", ticket, "vscode"); err == nil {
		_ = replay.Close()
		t.Fatal("replayed VS Code ticket was accepted")
	}
	if shellTicket, _, err := issuer.Issue(AgentTicket{UserID: "user-1", TenantID: "tenant-1", WorkspaceID: "workspace-1",
		ResourcePlane: "rp-1", MicroVMID: "microvm-vscode", Permission: workspacesession.PermissionShell}); err != nil {
		t.Fatal(err)
	} else if wrongChannel, err := dialWorkspaceAgentChannel(server.URL, "https://gateway.hako.test", shellTicket, "vscode"); err == nil {
		_ = wrongChannel.Close()
		t.Fatal("shell ticket opened the VS Code route")
	}
}

func TestWorkspaceShellAgentBindsOnlyOnceAndOnlyAfterMount(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	uid, gid := testShellIdentity(t, root)
	agent, err := newWorkspaceShellAgent(map[string]ed25519.PublicKey{"k": public}, "https://gateway.hako.test",
		func(WorkspaceMountSpec) (*ShellManager, error) {
			return NewShellManager(ShellConfig{WorkspaceRoot: root, RunAsUID: uid, RunAsGID: gid})
		})
	if err != nil {
		t.Fatal(err)
	}
	bindWorkspaceTestExecRoot(agent, root)
	if err := agent.BindWorkspaceRuntime("microvm-1", testMountSpec()); err != nil {
		t.Fatal(err)
	}
	if err := agent.BindWorkspaceRuntime("microvm-1", testMountSpec()); err != nil {
		t.Fatalf("same runtime binding retry failed: %v", err)
	}
	other := testMountSpec()
	other.WorkspaceID = "workspace-2"
	if err := agent.BindWorkspaceRuntime("microvm-1", other); err == nil {
		t.Fatal("Agent accepted a conflicting Workspace rebinding")
	}
	if err := agent.shells.Close(); err != nil {
		t.Fatal(err)
	}
}

func bindWorkspaceTestExecRoot(agent *WorkspaceShellAgent, root string) {
	agent.execFactory = func(WorkspaceMountSpec) (*ExecManager, error) {
		return NewExecManager(root)
	}
}

func dialWorkspaceAgent(endpoint, origin, token string) (*websocket.Conn, error) {
	return dialWorkspaceAgentChannel(endpoint, origin, token, "shell")
}

func dialWorkspaceAgentChannel(endpoint, origin, token, channel string) (*websocket.Conn, error) {
	wsURL := "ws" + strings.TrimPrefix(endpoint, "http") + "/v1/workspaces/workspace-1/" + channel
	config, err := websocket.NewConfig(wsURL, origin)
	if err != nil {
		return nil, err
	}
	config.Protocol = []string{workspaceAgentWebSocketProtocol}
	config.Header.Set("Authorization", "Bearer "+token)
	return config.DialContext(context.Background())
}

var _ http.Handler = (*WorkspaceShellAgent)(nil)
var _ WorkspaceRuntimeBinder = (*WorkspaceShellAgent)(nil)
var _ io.Closer = (*ShellManager)(nil)
