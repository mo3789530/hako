//go:build linux

package workspaceagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	wsURL := "ws" + strings.TrimPrefix(endpoint, "http") + "/v1/workspaces/workspace-1/shell"
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
