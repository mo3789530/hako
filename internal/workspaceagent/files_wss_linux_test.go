//go:build linux

package workspaceagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/workspaceprotocol"
	"github.com/mo3789530/hako/internal/workspacesession"
)

func TestWorkspaceFileRouteUsesOneUseTicketAndStreamsBoundedFiles(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	uid, gid := testShellIdentity(t, root)
	if err := os.Mkdir(filepath.Join(root, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	agent, err := newWorkspaceShellAgent(map[string]ed25519.PublicKey{"rp-key-files": public}, "https://gateway.hako.test",
		func(spec WorkspaceMountSpec) (*ShellManager, error) {
			return NewShellManager(ShellConfig{WorkspaceRoot: root, RunAsUID: uid, RunAsGID: gid})
		})
	if err != nil {
		t.Fatal(err)
	}
	bindWorkspaceTestExecRoot(agent, root)
	if err := agent.BindWorkspaceRuntime("microvm-files", testMountSpec()); err != nil {
		t.Fatal(err)
	}
	files, err := newWorkspaceFilesAt(root, uid, gid)
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.BindWorkspaceFileRuntime(files); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	server := httptest.NewServer(agent)
	defer server.Close()
	issuer := &AgentTicketIssuer{KeyID: "rp-key-files", PrivateKey: private, TTL: time.Minute}
	issue := func(permission workspacesession.Permission) string {
		t.Helper()
		ticket, _, err := issuer.Issue(AgentTicket{UserID: "user-1", TenantID: "tenant-1", WorkspaceID: "workspace-1",
			ResourcePlane: "rp-1", MicroVMID: "microvm-files", Permission: permission})
		if err != nil {
			t.Fatal(err)
		}
		return ticket
	}
	client, err := dialWorkspaceAgentChannel(server.URL, "https://gateway.hako.test", issue(workspacesession.PermissionFileTransfer), "files")
	if err != nil {
		t.Fatalf("connect using file-transfer ticket: %v", err)
	}
	const body = "workspace file payload\n"
	if err := workspaceprotocol.WriteFileRequest(client, workspaceprotocol.FileRequest{Operation: "upload", Path: "/workspace/src/data.txt", Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(client, body); err != nil {
		t.Fatal(err)
	}
	response, err := workspaceprotocol.ReadFileResponse(client)
	if err != nil || !response.OK || response.Size != int64(len(body)) {
		t.Fatalf("upload response=%+v error=%v", response, err)
	}
	_ = client.Close()
	client, err = dialWorkspaceAgentChannel(server.URL, "https://gateway.hako.test", issue(workspacesession.PermissionFileTransfer), "files")
	if err != nil {
		t.Fatalf("connect for download: %v", err)
	}
	if err := workspaceprotocol.WriteFileRequest(client, workspaceprotocol.FileRequest{Operation: "download", Path: "/workspace/src/data.txt"}); err != nil {
		t.Fatal(err)
	}
	response, err = workspaceprotocol.ReadFileResponse(client)
	if err != nil || !response.OK || response.Size != int64(len(body)) {
		t.Fatalf("download response=%+v error=%v", response, err)
	}
	downloaded := make([]byte, response.Size)
	if _, err := io.ReadFull(client, downloaded); err != nil || string(downloaded) != body {
		t.Fatalf("download body=%q error=%v", downloaded, err)
	}
	_ = client.Close()
	if replay, err := dialWorkspaceAgentChannel(server.URL, "https://gateway.hako.test", issue(workspacesession.PermissionFileTransfer), "files"); err != nil {
		t.Fatal(err)
	} else {
		_ = replay.Close()
	}
	replayTicket := issue(workspacesession.PermissionFileTransfer)
	first, err := dialWorkspaceAgentChannel(server.URL, "https://gateway.hako.test", replayTicket, "files")
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	if replay, err := dialWorkspaceAgentChannel(server.URL, "https://gateway.hako.test", replayTicket, "files"); err == nil {
		_ = replay.Close()
		t.Fatal("replayed file-transfer ticket was accepted")
	}
	if unauthorized, err := dialWorkspaceAgentChannel(server.URL, "https://gateway.hako.test", issue(workspacesession.PermissionShell), "files"); err == nil {
		_ = unauthorized.Close()
		t.Fatal("shell ticket was accepted on file-transfer endpoint")
	}
	if _, err := os.Stat(filepath.Join(root, "src", "data.txt")); err != nil {
		t.Fatalf("uploaded file missing: %v", err)
	}
}
