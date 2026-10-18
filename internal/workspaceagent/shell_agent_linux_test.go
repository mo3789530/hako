//go:build linux

package workspaceagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
	resizeFrame, err := workspaceprotocol.EncodeResize(100, 32)
	if err != nil {
		t.Fatal(err)
	}
	if err := websocket.Message.Send(client, resizeFrame); err != nil {
		t.Fatal(err)
	}
	dataFrame, err := workspaceprotocol.EncodeData([]byte("stty size; printf 'hako-agent-pty-ok\\n'; exit\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := websocket.Message.Send(client, dataFrame); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	deadline := time.After(3 * time.Second)
	for !strings.Contains(output.String(), "32 100\r\n") {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for PTY output: %q", output.String())
		default:
		}
		var frame []byte
		if err := websocket.Message.Receive(client, &frame); err != nil {
			t.Fatalf("receive PTY frame: %v output=%q", err, output.String())
		}
		frames, err := new(workspaceprotocol.Decoder).Feed(frame)
		if err != nil {
			t.Fatalf("decode PTY frame: %v", err)
		}
		for _, decoded := range frames {
			output.Write(decoded.Data)
		}
	}
	if !strings.Contains(output.String(), "hako-agent-pty-ok") {
		t.Fatalf("PTY command did not complete after resize: %q", output.String())
	}
	_ = client.Close()
	if replay, err := dialWorkspaceAgent(server.URL, origin, ticket); err == nil {
		_ = replay.Close()
		t.Fatal("replayed Agent ticket was accepted")
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
