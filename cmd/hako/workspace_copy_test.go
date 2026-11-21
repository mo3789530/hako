package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/api"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacegateway"
	"github.com/mo3789530/hako/internal/workspaceprotocol"
	"github.com/mo3789530/hako/internal/workspacesession"
)

type copyTestDialer struct {
	seenPath         chan string
	uploadOK         chan []byte
	download         []byte
	uploadFailed     bool
	truncateDownload bool
}

func (d copyTestDialer) OpenShell(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error) {
	return nil, workspacegateway.ErrClientConfig
}
func (d copyTestDialer) OpenExec(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error) {
	return nil, workspacegateway.ErrClientConfig
}
func (d copyTestDialer) OpenVSCode(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error) {
	return nil, workspacegateway.ErrClientConfig
}
func (d copyTestDialer) OpenPort(context.Context, workspacesession.Claims, workspacesession.Permission, uint16) (io.ReadWriteCloser, error) {
	return nil, workspacegateway.ErrClientConfig
}
func (d copyTestDialer) OpenFileTransfer(_ context.Context, _ workspacesession.Claims) (io.ReadWriteCloser, error) {
	client, peer := net.Pipe()
	go func() {
		defer peer.Close()
		request, err := workspaceprotocol.ReadFileRequest(peer)
		if err != nil {
			return
		}
		if d.seenPath != nil {
			d.seenPath <- request.Path
		}
		switch request.Operation {
		case "upload":
			body := make([]byte, request.Size)
			if _, err := io.ReadFull(peer, body); err != nil {
				return
			}
			if d.uploadOK != nil {
				d.uploadOK <- body
			}
			if d.uploadFailed {
				_ = workspaceprotocol.WriteFileResponse(peer, workspaceprotocol.FileResponse{OK: false, Code: "transfer_failed"})
				return
			}
			_ = workspaceprotocol.WriteFileResponse(peer, workspaceprotocol.FileResponse{OK: true, Size: request.Size})
		case "download":
			_ = workspaceprotocol.WriteFileResponse(peer, workspaceprotocol.FileResponse{OK: true, Size: int64(len(d.download))})
			payload := d.download
			if d.truncateDownload && len(payload) > 0 {
				payload = payload[:len(payload)-1]
			}
			_, _ = peer.Write(payload)
		}
	}()
	return client, nil
}

func TestCopyWorkspacePathUploadAndDownloadOverAuthenticatedGateway(t *testing.T) {
	cases := []struct {
		name             string
		download         bool
		failed           bool
		truncateDownload bool
	}{
		{name: "upload"},
		{name: "download", download: true},
		{name: "upload failure", failed: true},
		{name: "incomplete download", download: true, failed: true, truncateDownload: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			download := testCase.download
			previousGetter := cliCredentialGetter
			cliCredentialGetter = workspaceShellCredentials(fmt.Sprintf(`{"access_token":"copy-access","token_type":"Bearer","expires_in":3600,"stored_at":"%s"}`,
				time.Now().UTC().Format(time.RFC3339)))
			t.Cleanup(func() { cliCredentialGetter = previousGetter })
			publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			issuer := &workspacesession.Ed25519Issuer{KeyID: "copy-test", PrivateKey: privateKey, TTL: time.Minute}
			ticket, claims, err := issuer.Issue(workspacesession.Claims{UserID: "usr_cli", TenantID: "tenant_cli", WorkspaceID: "ws_cli",
				ResourcePlane: "rp_cli", Permissions: []workspacesession.Permission{workspacesession.PermissionFileTransfer}})
			if err != nil {
				t.Fatal(err)
			}
			var created, revoked atomic.Int32
			apiServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer copy-access" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				if r.Method == http.MethodDelete {
					revoked.Add(1)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				var request struct {
					Permissions []workspacesession.Permission `json:"permissions"`
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/tenants/tenant_cli/workspaces/ws_cli/sessions" ||
					json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Permissions) != 1 || request.Permissions[0] != workspacesession.PermissionFileTransfer {
					http.Error(w, "unexpected file-transfer session request", http.StatusBadRequest)
					return
				}
				created.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(api.WorkspaceSession{SessionID: domain.SessionID(claims.TicketID), Ticket: ticket,
					ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC(), Permissions: []workspacesession.Permission{workspacesession.PermissionFileTransfer}})
			}))
			defer apiServer.Close()

			body := []byte("workspace-copy-payload")
			seenPath := make(chan string, 1)
			uploadOK := make(chan []byte, 1)
			gateway, err := workspacegateway.New(workspacegateway.Config{ResourcePlaneID: "rp_cli",
				Verifier: &workspacesession.Verifier{Keys: map[string]ed25519.PublicKey{"copy-test": publicKey}, Audience: workspacesession.AudienceGateway, Lookup: activeWorkspaceSession{}},
				Dialer: copyTestDialer{seenPath: seenPath, uploadOK: uploadOK, download: body,
					uploadFailed: testCase.failed && !download, truncateDownload: testCase.truncateDownload},
				MaxConnections: 2, MaxConnectionsPerWorkspace: 2})
			if err != nil {
				t.Fatal(err)
			}
			gatewayServer := httptest.NewTLSServer(gateway.Handler())
			defer gatewayServer.Close()
			t.Setenv("HAKO_API_URL", apiServer.URL)
			t.Setenv("HAKO_GATEWAY_URL", gatewayServer.URL)
			gatewayTLS := gatewayServer.Client().Transport.(*http.Transport).TLSClientConfig
			dir := t.TempDir()
			localPath := filepath.Join(dir, "payload.bin")
			remotePath := "/workspace/project/payload.bin"
			if download {
				localPath = filepath.Join(dir, "download.bin")
			}
			if !download {
				if err := os.WriteFile(localPath, body, 0600); err != nil {
					t.Fatal(err)
				}
			} else if testCase.failed {
				if err := os.WriteFile(localPath, []byte("keep-existing"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			source, destination := localPath, remotePath
			if download {
				source, destination = remotePath, localPath
			}
			err = copyWorkspacePathWith(context.Background(), "tenant_cli", "ws_cli", source, destination, download,
				apiServer.Client(), &tls.Config{RootCAs: gatewayTLS.RootCAs})
			if testCase.failed && err == nil {
				t.Fatal("failed transfer unexpectedly succeeded")
			}
			if !testCase.failed && err != nil {
				t.Fatalf("copyWorkspacePathWith: %v", err)
			}
			if created.Load() != 1 || revoked.Load() != 1 {
				t.Fatalf("session create=%d revoke=%d", created.Load(), revoked.Load())
			}
			wantRemote := "/workspace/project/payload.bin"
			select {
			case got := <-seenPath:
				if got != wantRemote {
					t.Fatalf("remote path=%q, want %q", got, wantRemote)
				}
			case <-time.After(time.Second):
				t.Fatal("file transfer did not reach the Gateway")
			}
			if download && testCase.failed {
				got, readErr := os.ReadFile(localPath)
				if readErr != nil || string(got) != "keep-existing" {
					t.Fatalf("failed download changed destination: contents=%q err=%v", got, readErr)
				}
				entries, readErr := os.ReadDir(dir)
				if readErr != nil || len(entries) != 1 || entries[0].Name() != "download.bin" {
					t.Fatalf("failed download left a temporary file: entries=%v err=%v", entries, readErr)
				}
			} else if download {
				got, err := os.ReadFile(localPath)
				if err != nil || string(got) != string(body) {
					t.Fatalf("download contents=%q, err=%v", got, err)
				}
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 1 || entries[0].Name() != "download.bin" {
					t.Fatalf("temporary download was not committed cleanly: entries=%v err=%v", entries, err)
				}
			} else {
				select {
				case got := <-uploadOK:
					if string(got) != string(body) {
						t.Fatalf("uploaded contents=%q, want %q", got, body)
					}
				case <-time.After(time.Second):
					t.Fatal("upload data did not reach the Guest")
				}
			}
		})
	}
}

func TestValidWorkspaceFileCLIPath(t *testing.T) {
	for _, path := range []string{
		"/workspace/a.txt",
		"/workspace/nested dir/source.go",
		"/workspace/.hidden",
	} {
		if !validWorkspaceFileCLIPath(path) {
			t.Errorf("valid Workspace path rejected: %q", path)
		}
	}
	for _, path := range []string{
		"", "/workspace", "/workspace/", "/etc/passwd", "/workspace-old/file",
		"/workspace/../etc/passwd", "/workspace/a/../../etc/passwd", "/workspace/a\nfile",
	} {
		if validWorkspaceFileCLIPath(path) {
			t.Errorf("unsafe Workspace path accepted: %q", path)
		}
	}
}

func TestPrepareWorkspaceDownloadCreatesPrivateSiblingAndRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "download.txt")
	temp, file, err := prepareWorkspaceDownload(destination, true)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(temp) != dir || temp == destination {
		t.Fatalf("temporary download is not a sibling: %q", temp)
	}
	info, err := os.Stat(temp)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("temporary download permissions = %o, want 600", info.Mode().Perm())
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(temp); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, destination); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareWorkspaceDownload(destination, true); err == nil {
		t.Fatal("symlink destination was accepted")
	}
	contents, err := os.ReadFile(target)
	if err != nil || string(contents) != "preserve" {
		t.Fatalf("symlink target changed: %q, %v", contents, err)
	}
}

func TestPrepareWorkspaceDownloadNoopForUpload(t *testing.T) {
	temp, file, err := prepareWorkspaceDownload("ignored", false)
	if err != nil || temp != "" || file != nil {
		t.Fatalf("upload preparation = %q, %v, %v", temp, file, err)
	}
}

func TestValidCopyID(t *testing.T) {
	for _, id := range []string{"tenant_1", "ws-1", "a"} {
		if !validCopyID(id) {
			t.Errorf("valid ID rejected: %q", id)
		}
	}
	for _, id := range []string{"", "../ws", "tenant;id", "a/b", "space id"} {
		if validCopyID(id) {
			t.Errorf("unsafe ID accepted: %q", id)
		}
	}
}

func TestCopyWorkspacePathRejectsSymlinkUploadBeforeIssuingSession(t *testing.T) {
	previousGetter := cliCredentialGetter
	cliCredentialGetter = workspaceShellCredentials(fmt.Sprintf(`{"access_token":"copy-access","token_type":"Bearer","expires_in":3600,"stored_at":"%s"}`,
		time.Now().UTC().Format(time.RFC3339)))
	t.Cleanup(func() { cliCredentialGetter = previousGetter })
	t.Setenv("HAKO_API_URL", "https://api.example.test")
	t.Setenv("HAKO_GATEWAY_URL", "https://gateway.example.test")
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, source); err != nil {
		t.Fatal(err)
	}
	if err := copyWorkspacePath(context.Background(), "tenant_1", "ws_1", source, "/workspace/file.txt", false); err == nil {
		t.Fatal("symlink upload source was accepted")
	}
}
