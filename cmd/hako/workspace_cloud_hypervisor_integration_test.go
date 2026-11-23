//go:build linux && cloud_hypervisor_workspace_smoke

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/api"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadsource"
	"github.com/mo3789530/hako/internal/workspaceagent"
	"github.com/mo3789530/hako/internal/workspacegateway"
	"github.com/mo3789530/hako/internal/workspacejournal"
	"github.com/mo3789530/hako/internal/workspaceruntime"
	"github.com/mo3789530/hako/internal/workspacesession"
)

type cloudHypervisorCLIJournaling struct{ snapshot workspacejournal.Snapshot }

func (j cloudHypervisorCLIJournaling) Get(_ context.Context, tenant domain.TenantID, workspace domain.WorkspaceID, plane domain.ResourcePlaneID) (workspacejournal.Snapshot, error) {
	if j.snapshot.Journal.TenantID != tenant || j.snapshot.Journal.WorkspaceID != workspace || j.snapshot.Journal.ResourcePlaneID != plane {
		return workspacejournal.Snapshot{}, workspacejournal.ErrNotFound
	}
	return j.snapshot, nil
}

type cloudHypervisorCLIActiveSession struct{}

func (cloudHypervisorCLIActiveSession) IsActive(context.Context, workspacesession.Claims) (bool, error) {
	return true, nil
}

// This opt-in integration test drives the real hako exec client through the
// authenticated Gateway and local CloudHypervisor StreamDirectory into a KVM Guest.
func TestWorkspaceExecCLIThroughCloudHypervisorGuest(t *testing.T) {
	if os.Getenv("HAKO_LOCAL_WORKSPACE_AGENT_PRIVATE_KEY") == "" {
		t.Skip("set HAKO_LOCAL_WORKSPACE_AGENT_PRIVATE_KEY for the matching Guest initrd")
	}
	cloudHypervisor := strings.TrimSpace(os.Getenv("CLOUD_HYPERVISOR_BIN"))
	kernel := strings.TrimSpace(os.Getenv("CLOUD_HYPERVISOR_KERNEL"))
	initrd := strings.TrimSpace(os.Getenv("CLOUD_HYPERVISOR_INITRD"))
	if cloudHypervisor == "" || kernel == "" || initrd == "" {
		t.Fatal("CLOUD_HYPERVISOR_BIN, CLOUD_HYPERVISOR_KERNEL and CLOUD_HYPERVISOR_INITRD are required")
	}
	if !readableWritableKVM() {
		t.Fatal("readable and writable /dev/kvm is required")
	}
	privateAgentKey := readCloudHypervisorCLIAgentKey(t, os.Getenv("HAKO_LOCAL_WORKSPACE_AGENT_PRIVATE_KEY"))
	defer clear(privateAgentKey)

	const tenant = "tenant_cli_smoke"
	const workspace = "workspace_cli_smoke"
	const plane = "plane_cli_smoke"
	const microVM = "ch_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root, err := os.MkdirTemp("/tmp", "hfc-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	stateDir := filepath.Join(root, "state")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	kernelDigest := sha256.Sum256(readCloudHypervisorCLIFile(t, kernel))
	initrdDigest := sha256.Sum256(readCloudHypervisorCLIFile(t, initrd))
	bootArgs := strings.Join([]string{"console=tty0", "console=ttyS0", "panic=1", "reboot=k", "rdinit=/init",
		"hako.tenant_id=" + tenant, "hako.workspace_id=" + workspace,
		"hako.resource_plane_id=" + plane, "hako.microvm_id=" + microVM,
		"hako.gateway_origin=https://workspace-gateway.invalid"}, " ")
	launch, err := json.Marshal(workspaceruntime.CloudHypervisorLocalLaunchSpec{
		KernelImagePath: kernel, KernelImageSHA256: hex.EncodeToString(kernelDigest[:]),
		InitrdPath: initrd, InitrdSHA256: hex.EncodeToString(initrdDigest[:]),
		KernelBootArgs: bootArgs, VCPUCount: 1, MemoryMiB: 8192, WorkspaceDiskSizeMiB: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	driver, err := workspaceruntime.NewCloudHypervisorLocalDriver(workspaceruntime.CloudHypervisorLocalDriverConfig{
		BinaryPath: cloudHypervisor, StateDirectory: stateDir, StartupTimeout: 2 * time.Minute, StopTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.EnsureRunning(ctx, microVM, launch); err != nil {
		t.Fatalf("ensure real CloudHypervisor Workspace: %v", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cleanupCancel()
		if err := driver.Delete(cleanupCtx, microVM); err != nil {
			t.Errorf("delete real CloudHypervisor Workspace: %v", err)
		}
	}()
	if err := waitForCloudHypervisorCLIAgent(ctx, filepath.Join(stateDir, microVM), tenant, workspace, plane, microVM); err != nil {
		t.Fatal(err)
	}

	journal := workspacejournal.Journal{SchemaVersion: 1, TenantID: tenant, WorkspaceID: workspace,
		ResourcePlaneID: plane, WorkspaceRevision: 1, Revision: 1, OperationID: "op_cli_smoke",
		OperationType: domain.OperationEnsureRunning, State: workspacejournal.Running, AllocationGeneration: 1,
		ProviderID: microVM, LaunchToken: "launch_cli_smoke", LaunchSpecSHA256: strings.Repeat("a", 64),
		CleanupState: workspacejournal.CleanupNotRequired, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	snapshot := workspacejournal.Snapshot{Journal: journal, ETag: "cli-smoke"}
	if err := snapshot.Journal.Validate(); err != nil {
		t.Fatalf("validate smoke journal: %v", err)
	}
	agentIssuer := &workspaceagent.AgentTicketIssuer{KeyID: "local_agent", PrivateKey: privateAgentKey, TTL: time.Minute}
	directory := &workspaceagent.CloudHypervisorWorkspaceVsockStreamDirectory{
		ResourcePlaneID: plane, Journal: cloudHypervisorCLIJournaling{snapshot: snapshot}, StateDirectory: stateDir,
		Tickets: agentIssuer, Origin: "https://workspace-gateway.invalid",
		Dial: func(ctx context.Context, allocationDir string) (net.Conn, error) {
			return workspaceruntime.DialCloudHypervisorLocalGuest(ctx, allocationDir)
		},
	}

	publicKey, sessionPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(sessionPrivateKey)
	sessionIssuer := &workspacesession.Ed25519Issuer{KeyID: "local_session", PrivateKey: sessionPrivateKey, TTL: time.Minute}
	var created, revoked atomic.Int32
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cli-access" {
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
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tenants/"+tenant+"/workspaces/"+workspace+"/sessions" ||
			json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Permissions) != 1 ||
			(request.Permissions[0] != workspacesession.PermissionExec && request.Permissions[0] != workspacesession.PermissionShell) {
			http.Error(w, "unexpected Workspace session request", http.StatusBadRequest)
			return
		}
		ticket, claims, issueErr := sessionIssuer.Issue(workspacesession.Claims{UserID: "user_cli_smoke", TenantID: tenant,
			WorkspaceID: workspace, ResourcePlane: plane, Permissions: request.Permissions})
		if issueErr != nil {
			http.Error(w, "session issue failed", http.StatusInternalServerError)
			return
		}
		created.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.WorkspaceSession{SessionID: domain.SessionID(claims.TicketID), Ticket: ticket,
			ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC(), Permissions: request.Permissions})
	}))
	defer apiServer.Close()
	gateway, err := workspacegateway.New(workspacegateway.Config{ResourcePlaneID: plane,
		Verifier: &workspacesession.Verifier{Keys: map[string]ed25519.PublicKey{"local_session": publicKey},
			Audience: workspacesession.AudienceGateway, Lookup: cloudHypervisorCLIActiveSession{}},
		Dialer: directory, MaxConnections: 4, MaxConnectionsPerWorkspace: 4})
	if err != nil {
		t.Fatal(err)
	}
	gatewayServer := httptest.NewTLSServer(gateway.Handler())
	defer gatewayServer.Close()
	gatewayTLS := gatewayServer.Client().Transport.(*http.Transport).TLSClientConfig
	previousGetter := cliCredentialGetter
	cliCredentialGetter = workspaceShellCredentials(`{"access_token":"cli-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	t.Cleanup(func() { cliCredentialGetter = previousGetter })
	t.Setenv("HAKO_API_URL", apiServer.URL)
	t.Setenv("HAKO_GATEWAY_URL", gatewayServer.URL)
	var stdout, stderr strings.Builder
	if err := workspaceExecWithTimeout(ctx, tenant, workspace, []string{"/usr/bin/id"}, 0, &stdout, &stderr,
		apiServer.Client(), &tls.Config{RootCAs: gatewayTLS.RootCAs}); err != nil {
		t.Fatalf("hako exec through real CloudHypervisor Guest: %v", err)
	}
	if !strings.Contains(stdout.String(), "uid=100000") || stderr.Len() != 0 || created.Load() != 1 || revoked.Load() != 1 {
		t.Fatalf("stdout=%q stderr=%q sessions created=%d revoked=%d", stdout.String(), stderr.String(), created.Load(), revoked.Load())
	}

	// Exercise the actual authenticated Guest file protocol used after the
	// Resource Plane Broker has resolved and pinned a GitHub repository.
	repository := filepath.Join(root, "repository")
	if err := os.Mkdir(repository, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s (%v)", args, output, err)
		}
		return strings.TrimSpace(string(output))
	}
	_ = git("-C", repository, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("KVM checkout\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", repository, "config", "user.email", "workspace@example.test"},
		{"-C", repository, "config", "user.name", "Workspace"}, {"-C", repository, "add", "README.md"},
		{"-C", repository, "commit", "--quiet", "-m", "initial"}, {"-C", repository, "checkout", "--quiet", "--detach", "HEAD"}} {
		_ = git(args...)
	}
	commit := git("-C", repository, "rev-parse", "HEAD")
	archive, err := workloadsource.CreateWorkspaceRepositoryArchive(ctx, repository, workloadsource.DefaultMaxArchiveBytes)
	if err != nil {
		t.Fatalf("create pinned repository archive: %v", err)
	}
	defer archive.Close()
	archiveBytes, err := io.ReadAll(archive.Reader())
	if err != nil {
		t.Fatalf("read repository archive: %v", err)
	}
	installRepository := func() error {
		fileTicket, _, err := sessionIssuer.Issue(workspacesession.Claims{UserID: "user_cli_smoke", TenantID: tenant,
			WorkspaceID: workspace, ResourcePlane: plane, Permissions: []workspacesession.Permission{workspacesession.PermissionFileTransfer}})
		if err != nil {
			return err
		}
		fileStream, err := workspacegateway.Dial(ctx, gatewayServer.URL, domain.WorkspaceID(workspace),
			workspacesession.PermissionFileTransfer, 0, fileTicket, &tls.Config{RootCAs: gatewayTLS.RootCAs})
		if err != nil {
			return err
		}
		defer fileStream.Close()
		return workspaceagent.TransferWorkspaceRepositoryArchive(ctx, fileStream,
			workloadsource.SourceArchive{Body: io.NopCloser(bytes.NewReader(archiveBytes)), Size: int64(len(archiveBytes)), SHA256: archive.SHA256()},
			"demo", commit, archive.SHA256())
	}
	if err := installRepository(); err != nil {
		t.Fatalf("install pinned repository through real Gateway and Guest: %v", err)
	}
	if err := installRepository(); err != nil {
		t.Fatalf("identical repository retry through real Guest: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if err := workspaceExecWithTimeout(ctx, tenant, workspace, []string{"/usr/bin/cat", "/workspace/demo/README.md"}, 0, &stdout, &stderr,
		apiServer.Client(), &tls.Config{RootCAs: gatewayTLS.RootCAs}); err != nil {
		t.Fatalf("read repository checkout through hako exec: %v", err)
	}
	if stdout.String() != "KVM checkout\n" || stderr.Len() != 0 {
		t.Fatalf("repository checkout content=%q stderr=%q", stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if err := workspaceExecWithTimeout(ctx, tenant, workspace, []string{"/bin/sh", "-c", "printf 'local edit\\n' > /workspace/demo/README.md"}, 0, &stdout, &stderr,
		apiServer.Client(), &tls.Config{RootCAs: gatewayTLS.RootCAs}); err != nil || stderr.Len() != 0 {
		t.Fatalf("edit repository in real Guest: stdout=%q stderr=%q err=%v", stdout.String(), stderr.String(), err)
	}
	if err := installRepository(); err == nil {
		t.Fatal("repository retry replaced an existing local edit")
	}
	stdout.Reset()
	stderr.Reset()
	if err := workspaceExecWithTimeout(ctx, tenant, workspace, []string{"/usr/bin/cat", "/workspace/demo/README.md"}, 0, &stdout, &stderr,
		apiServer.Client(), &tls.Config{RootCAs: gatewayTLS.RootCAs}); err != nil {
		t.Fatalf("read local edit after retry: %v", err)
	}
	if stdout.String() != "local edit\n" || stderr.Len() != 0 {
		t.Fatalf("repository retry did not preserve local edit: content=%q stderr=%q", stdout.String(), stderr.String())
	}
	var shellOutput strings.Builder
	if err := workspaceShellWith(ctx, tenant, workspace, strings.NewReader("cat /workspace/demo/README.md\nexit\n"), &shellOutput,
		apiServer.Client(), &tls.Config{RootCAs: gatewayTLS.RootCAs}); err != nil {
		t.Fatalf("hako shell through real CloudHypervisor Guest: %v", err)
	}
	if !strings.Contains(shellOutput.String(), "local edit") {
		t.Fatalf("hako shell did not read the preserved repository edit: %q", shellOutput.String())
	}
	if err := driver.Suspend(ctx, microVM); err != nil {
		t.Fatalf("cold-suspend repository Workspace: %v", err)
	}
	if err := driver.Resume(ctx, microVM, launch); err != nil {
		t.Fatalf("cold-resume repository Workspace: %v", err)
	}
	if err := waitForCloudHypervisorCLIAgent(ctx, filepath.Join(stateDir, microVM), tenant, workspace, plane, microVM); err != nil {
		t.Fatalf("wait for repository Guest after cold resume: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if err := workspaceExecWithTimeout(ctx, tenant, workspace, []string{"/usr/bin/cat", "/workspace/demo/README.md"}, 0, &stdout, &stderr,
		apiServer.Client(), &tls.Config{RootCAs: gatewayTLS.RootCAs}); err != nil {
		t.Fatalf("read checkout after cold resume: %v", err)
	}
	if stdout.String() != "local edit\n" || stderr.Len() != 0 {
		t.Fatalf("checkout/local edit did not persist after cold resume: content=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func readCloudHypervisorCLIFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		t.Fatalf("read CloudHypervisor smoke input %q: %v", path, err)
	}
	return data
}

func readCloudHypervisorCLIAgentKey(t *testing.T, path string) ed25519.PrivateKey {
	t.Helper()
	encoded := readCloudHypervisorCLIFile(t, path)
	block, rest := pem.Decode(encoded)
	if block == nil || block.Type != "PRIVATE KEY" || strings.TrimSpace(string(rest)) != "" {
		t.Fatal("decode local Workspace Agent private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(ed25519.PrivateKey)
	if err != nil || !ok {
		t.Fatal("parse local Workspace Agent Ed25519 private key")
	}
	return key
}

func waitForCloudHypervisorCLIAgent(ctx context.Context, allocationDir, tenant, workspace, plane, microVM string) error {
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		dialCtx, cancel := context.WithTimeout(ctx, time.Second)
		conn, err := workspaceruntime.DialCloudHypervisorLocalGuest(dialCtx, allocationDir)
		cancel()
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			_, err = io.WriteString(conn, "GET /internal/heartbeat HTTP/1.1\r\nHost: workspace-agent.vsock\r\nConnection: close\r\n\r\n")
			if err == nil {
				response, readErr := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
				if readErr == nil {
					var identity struct {
						ProtocolVersion int    `json:"protocol_version"`
						TenantID        string `json:"tenant_id"`
						WorkspaceID     string `json:"workspace_id"`
						ResourcePlaneID string `json:"resource_plane_id"`
						MicroVMID       string `json:"microvm_id"`
					}
					decodeErr := json.NewDecoder(io.LimitReader(response.Body, 4097)).Decode(&identity)
					_ = response.Body.Close()
					if decodeErr == nil && response.StatusCode == http.StatusOK && identity.ProtocolVersion == 1 &&
						identity.TenantID == tenant && identity.WorkspaceID == workspace && identity.ResourcePlaneID == plane && identity.MicroVMID == microVM {
						_ = conn.Close()
						return nil
					}
					lastErr = io.ErrUnexpectedEOF
				} else {
					lastErr = readErr
				}
			} else {
				lastErr = err
			}
			_ = conn.Close()
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return lastErr
		case <-ticker.C:
		}
	}
}

func readableWritableKVM() bool {
	file, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	return file.Close() == nil
}
