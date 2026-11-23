//go:build linux && cloud_hypervisor_workspace_smoke

// local_cloud_hypervisor_workspace_smoke boots the actual Hako Workspace initrd and
// checks the identity-bound Workspace Agent over the host-private vsock.
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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspaceagent"
	"github.com/mo3789530/hako/internal/workspacegateway"
	"github.com/mo3789530/hako/internal/workspacejournal"
	"github.com/mo3789530/hako/internal/workspaceprotocol"
	"github.com/mo3789530/hako/internal/workspaceruntime"
	"github.com/mo3789530/hako/internal/workspacesession"
	"golang.org/x/net/websocket"
)

type smokeWorkspaceJournal struct{ snapshot workspacejournal.Snapshot }

func (j smokeWorkspaceJournal) Get(_ context.Context, tenant domain.TenantID, workspace domain.WorkspaceID, plane domain.ResourcePlaneID) (workspacejournal.Snapshot, error) {
	if j.snapshot.Journal.TenantID != tenant || j.snapshot.Journal.WorkspaceID != workspace || j.snapshot.Journal.ResourcePlaneID != plane {
		return workspacejournal.Snapshot{}, workspacejournal.ErrNotFound
	}
	return j.snapshot, nil
}

type smokeWorkspaceSessionLookup struct{}

func (smokeWorkspaceSessionLookup) IsActive(context.Context, workspacesession.Claims) (bool, error) {
	return true, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "local Hako Workspace Guest smoke failed:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 6 {
		return errors.New("usage: local_cloud_hypervisor_workspace_smoke <cloudHypervisor> <kernel> <initrd> <state-dir> <boot-args>")
	}
	inputArgs := os.Args[1:]
	binary, kernel, initrd, stateDir, bootArgs := inputArgs[0], inputArgs[1], inputArgs[2], inputArgs[3], inputArgs[4]
	kernelSHA, err := hashFile(kernel)
	if err != nil {
		return err
	}
	initrdSHA, err := hashFile(initrd)
	if err != nil {
		return err
	}
	const tenant, workspace, plane = "tenant_smoke", "workspace_smoke", "plane_smoke"
	const microVM = "ch_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	args := strings.Join([]string{bootArgs,
		"hako.tenant_id=" + tenant, "hako.workspace_id=" + workspace,
		"hako.resource_plane_id=" + plane, "hako.microvm_id=" + microVM,
		"hako.gateway_origin=https://workspace-gateway.invalid",
	}, " ")
	payload, err := json.Marshal(workspaceruntime.CloudHypervisorLocalLaunchSpec{
		KernelImagePath: kernel, KernelImageSHA256: kernelSHA,
		InitrdPath: initrd, InitrdSHA256: initrdSHA,
		// The gzip initrd expands to >1.6 GiB; keep ample headroom for the
		// decompressed rootfs, kernel and Guest services while unpacking.
		KernelBootArgs: args, VCPUCount: 1, MemoryMiB: 8192, WorkspaceDiskSizeMiB: 128,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	const allocation = "ch_hako_workspace_smoke"
	driver, err := workspaceruntime.NewCloudHypervisorLocalDriver(workspaceruntime.CloudHypervisorLocalDriverConfig{
		BinaryPath: binary, StateDirectory: stateDir, StartupTimeout: 2 * time.Minute,
		StopTimeout: 20 * time.Second,
	})
	if err != nil {
		return err
	}
	if err := driver.EnsureRunning(ctx, allocation, payload); err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cleanupCancel()
		_ = driver.Delete(cleanupCtx, allocation)
	}()
	if err := waitForWorkspaceAgent(ctx, filepath.Join(stateDir, allocation), tenant, workspace, plane, microVM); err != nil {
		logPath := filepath.Join(stateDir, allocation, "cloud-hypervisor.log")
		if log, readErr := os.ReadFile(logPath); readErr == nil {
			fmt.Fprintln(os.Stderr, string(log))
		}
		return err
	}
	if keyPath := os.Getenv("HAKO_LOCAL_WORKSPACE_AGENT_PRIVATE_KEY"); keyPath != "" {
		if err := exerciseGuestChannels(ctx, filepath.Join(stateDir, allocation), keyPath, tenant, workspace, plane, microVM, false); err != nil {
			return err
		}
		if err := driver.Suspend(ctx, allocation); err != nil {
			return fmt.Errorf("suspend Workspace Guest after channel acceptance: %w", err)
		}
		if os.Getenv("HAKO_LOCAL_WORKSPACE_SMOKE_KEEP_STATE") == "true" {
			logPath := filepath.Join(stateDir, allocation, "cloud-hypervisor.log")
			if err := preservePrivateSmokeLog(logPath, filepath.Join(stateDir, "cloud-hypervisor-first-boot-debug.log")); err != nil {
				return fmt.Errorf("suspend succeeded but the private Guest log could not be preserved: %w", err)
			}
		}
		// Reconstruct the controller driver to cover a cold Guest resume after
		// controller process loss, then verify the same Workspace files and
		// interactive channels against the resumed Guest.
		driver, err = workspaceruntime.NewCloudHypervisorLocalDriver(workspaceruntime.CloudHypervisorLocalDriverConfig{
			BinaryPath: binary, StateDirectory: stateDir, StartupTimeout: 2 * time.Minute,
			StopTimeout: 20 * time.Second,
		})
		if err != nil {
			return err
		}
		if err := driver.Resume(ctx, allocation, payload); err != nil {
			if os.Getenv("HAKO_LOCAL_WORKSPACE_SMOKE_KEEP_STATE") == "true" {
				source := filepath.Join(stateDir, allocation, "workspace.ext4")
				info, statErr := os.Lstat(source)
				if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
					return fmt.Errorf("cold resume failed and the private Workspace disk could not be preserved")
				}
				if err := os.Link(source, filepath.Join(stateDir, "workspace-debug.ext4")); err != nil {
					return fmt.Errorf("cold resume failed and the Workspace disk snapshot could not be linked: %w", err)
				}
				logSource := filepath.Join(stateDir, allocation, "cloud-hypervisor.log")
				if err := preservePrivateSmokeLog(logSource, filepath.Join(stateDir, "cloud-hypervisor-debug.log")); err != nil {
					return fmt.Errorf("cold resume failed and the private Guest log could not be preserved: %w", err)
				}
			}
			return fmt.Errorf("cold resume Workspace Guest after controller reconstruction: %w", err)
		}
		if err := waitForWorkspaceAgent(ctx, filepath.Join(stateDir, allocation), tenant, workspace, plane, microVM); err != nil {
			return fmt.Errorf("wait for resumed Workspace Guest: %w", err)
		}
		if err := exerciseGuestChannels(ctx, filepath.Join(stateDir, allocation), keyPath, tenant, workspace, plane, microVM, true); err != nil {
			return fmt.Errorf("verify Workspace Guest channels after cold resume: %w", err)
		}
	}
	if err := driver.Delete(ctx, allocation); err != nil {
		return fmt.Errorf("orderly Guest shutdown and cleanup: %w", err)
	}
	if _, err := os.Lstat(filepath.Join(stateDir, allocation)); !errors.Is(err, os.ErrNotExist) {
		return errors.New("Hako Workspace Guest allocation was not removed")
	}
	fmt.Println("HAKO_LOCAL_WORKSPACE_INITRD_CLOUD_HYPERVISOR_BOOT_OK")
	return nil
}

func preservePrivateSmokeLog(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || info.Size() < 0 || info.Size() > 32<<20 {
		return errors.New("private Guest log has an unsafe type, mode, or size")
	}
	data, err := os.ReadFile(source)
	if err != nil || int64(len(data)) != info.Size() {
		return errors.New("private Guest log could not be read consistently")
	}
	if err := os.WriteFile(destination, data, 0600); err != nil {
		return err
	}
	return nil
}

func exerciseGuestChannels(ctx context.Context, allocationDir, keyPath, tenant, workspace, plane, microVM string, verifyExistingFile bool) error {
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil || len(keyPEM) > 16<<10 {
		return errors.New("read local Workspace smoke signing key")
	}
	block, rest := pem.Decode(keyPEM)
	if block == nil || block.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return errors.New("decode local Workspace smoke signing key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return errors.New("parse local Workspace smoke signing key")
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return errors.New("local Workspace smoke signing key is not Ed25519")
	}
	defer clear(privateKey)
	issuer := &workspaceagent.AgentTicketIssuer{KeyID: "local_agent", PrivateKey: privateKey, TTL: time.Minute}
	issue := func(permission workspacesession.Permission) (string, error) {
		ticket, _, issueErr := issuer.Issue(workspaceagent.AgentTicket{UserID: domain.UserID("local-smoke-user"),
			TenantID: domain.TenantID(tenant), WorkspaceID: domain.WorkspaceID(workspace),
			ResourcePlane: domain.ResourcePlaneID(plane), MicroVMID: microVM, Permission: permission})
		return ticket, issueErr
	}
	dial := func(permission workspacesession.Permission, endpoint string) (*websocket.Conn, error) {
		ticket, issueErr := issue(permission)
		if issueErr != nil {
			return nil, errors.New("issue local Workspace smoke ticket")
		}
		conn, dialErr := workspaceruntime.DialCloudHypervisorLocalGuest(ctx, allocationDir)
		if dialErr != nil {
			return nil, errors.New("connect to local Workspace Guest vsock")
		}
		_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
		location, parseErr := url.Parse("ws://workspace-agent.vsock" + endpoint)
		origin, originErr := url.Parse("https://workspace-gateway.invalid")
		if parseErr != nil || originErr != nil {
			_ = conn.Close()
			return nil, errors.New("configure local Workspace smoke WebSocket")
		}
		config := &websocket.Config{Location: location, Origin: origin, Protocol: []string{"hako.workspace.agent.v1"},
			Version: websocket.ProtocolVersionHybi13, Header: http.Header{"Authorization": []string{"Bearer " + ticket}}}
		client, clientErr := websocket.NewClient(config, conn)
		if clientErr != nil {
			_ = conn.Close()
			return nil, errors.New("authorize local Workspace Guest WebSocket")
		}
		return client, nil
	}
	execConn, err := dial(workspacesession.PermissionExec, "/v1/workspaces/"+workspace+"/exec")
	if err != nil {
		return err
	}
	request, _ := json.Marshal(workspaceprotocol.ExecRequest{Argv: []string{"/usr/bin/bash", "-c", "printf 'hako-kvm-exec-ok\\n'; /usr/bin/id"}})
	if err := websocket.Message.Send(execConn, request); err != nil {
		_ = execConn.Close()
		return errors.New("send local Workspace Guest exec smoke")
	}
	var execPayload []byte
	if err := websocket.Message.Receive(execConn, &execPayload); err != nil {
		_ = execConn.Close()
		return errors.New("receive local Workspace Guest exec smoke")
	}
	_ = execConn.Close()
	var execResult workspaceprotocol.ExecResult
	if json.Unmarshal(execPayload, &execResult) != nil || workspaceprotocol.ValidateExecResult(execResult) != nil ||
		execResult.ExitCode != 0 || !strings.Contains(string(execResult.Stdout), "hako-kvm-exec-ok") ||
		!strings.Contains(string(execResult.Stdout), "uid=100000") {
		return fmt.Errorf("local Workspace Guest exec smoke failed (exit=%d code=%q)", execResult.ExitCode, execResult.Code)
	}
	payload := []byte("hako-kvm-file-transfer-ok\\n")
	if verifyExistingFile {
		downloaded, err := downloadWorkspaceSmokeFile(dial, workspace)
		if err != nil || !bytes.Equal(downloaded, payload) {
			return errors.New("Workspace file did not persist through cold resume")
		}
	}
	filesConn, err := dial(workspacesession.PermissionFileTransfer, "/v1/workspaces/"+workspace+"/files")
	if err != nil {
		return err
	}
	if err := workspaceprotocol.WriteFileRequest(filesConn, workspaceprotocol.FileRequest{Operation: "upload", Path: "/workspace/smoke.txt", Size: int64(len(payload))}); err != nil {
		_ = filesConn.Close()
		return errors.New("send local Workspace Guest upload request")
	}
	if _, err := filesConn.Write(payload); err != nil {
		_ = filesConn.Close()
		return errors.New("send local Workspace Guest upload bytes")
	}
	response, err := workspaceprotocol.ReadFileResponse(filesConn)
	_ = filesConn.Close()
	if err != nil || !response.OK || response.Size != int64(len(payload)) {
		return fmt.Errorf("local Workspace Guest upload smoke failed (code=%q)", response.Code)
	}
	downloaded, err := downloadWorkspaceSmokeFile(dial, workspace)
	if err != nil || !bytes.Equal(downloaded, payload) {
		return errors.New("verify local Workspace Guest downloaded bytes")
	}
	shellConn, err := dial(workspacesession.PermissionShell, "/v1/workspaces/"+workspace+"/shell")
	if err != nil {
		return err
	}
	attach, err := workspaceprotocol.EncodeAttach("")
	if err != nil || websocket.Message.Send(shellConn, attach) != nil {
		_ = shellConn.Close()
		return errors.New("attach local Workspace Guest shell")
	}
	var sessionFrame []byte
	if err := websocket.Message.Receive(shellConn, &sessionFrame); err != nil {
		_ = shellConn.Close()
		return errors.New("receive local Workspace Guest shell session")
	}
	sessionFrames, err := new(workspaceprotocol.Decoder).Feed(sessionFrame)
	if err != nil || len(sessionFrames) != 1 || sessionFrames[0].Session == "" {
		_ = shellConn.Close()
		return errors.New("decode local Workspace Guest shell session")
	}
	shellInput, err := workspaceprotocol.EncodeData([]byte("/usr/bin/id\n"))
	if err != nil || websocket.Message.Send(shellConn, shellInput) != nil {
		_ = shellConn.Close()
		return errors.New("send local Workspace Guest shell command")
	}
	var shellOutput strings.Builder
	for !strings.Contains(shellOutput.String(), "uid=100000") {
		var outputFrame []byte
		if err := websocket.Message.Receive(shellConn, &outputFrame); err != nil {
			_ = shellConn.Close()
			return errors.New("receive local Workspace Guest shell output")
		}
		frames, err := new(workspaceprotocol.Decoder).Feed(outputFrame)
		if err != nil {
			_ = shellConn.Close()
			return errors.New("decode local Workspace Guest shell output")
		}
		for _, frame := range frames {
			shellOutput.Write(frame.Data)
		}
	}
	_ = shellConn.Close()
	if err := exerciseVSCodeBridge(dial, workspace); err != nil {
		return err
	}
	if err := exerciseBrowserIDE(ctx, dial, workspace); err != nil {
		return err
	}
	if err := exerciseSSHBridge(dial, workspace); err != nil {
		return err
	}
	if err := exerciseSSHLogin(ctx, dial, workspace); err != nil {
		return err
	}
	if err := exerciseGatewayExec(ctx, allocationDir, tenant, workspace, plane, microVM, privateKey); err != nil {
		return err
	}
	fmt.Println("HAKO_LOCAL_WORKSPACE_GUEST_VSCODE_SSH_SMOKE_OK")
	fmt.Println("HAKO_LOCAL_WORKSPACE_GUEST_EXEC_FILE_SMOKE_OK")
	return nil
}

func exerciseVSCodeBridge(dial func(workspacesession.Permission, string) (*websocket.Conn, error), workspace string) error {
	conn, err := dial(workspacesession.PermissionVSCode, "/v1/workspaces/"+workspace+"/vscode")
	if err != nil {
		return errors.New("connect to local Workspace VS Code bridge")
	}
	defer conn.Close()
	conn.PayloadType = websocket.BinaryFrame
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	request := []byte("GET /healthz HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n")
	if err := websocket.Message.Send(conn, request); err != nil {
		return errors.New("send local Workspace VS Code health request")
	}
	var response []byte
	for !bytes.Contains(response, []byte("\r\n\r\n")) {
		var frame []byte
		if err := websocket.Message.Receive(conn, &frame); err != nil {
			return errors.New("receive local Workspace VS Code health response")
		}
		response = append(response, frame...)
		if len(response) > 16<<10 {
			return errors.New("local Workspace VS Code response headers exceeded limit")
		}
	}
	if !bytes.HasPrefix(response, []byte("HTTP/1.1 200 ")) && !bytes.HasPrefix(response, []byte("HTTP/1.0 200 ")) {
		return errors.New("local Workspace VS Code health endpoint did not return HTTP 200")
	}
	return nil
}

func exerciseGatewayExec(ctx context.Context, allocationDir, tenant, workspace, plane, microVM string, agentKey ed25519.PrivateKey) error {
	if len(agentKey) != ed25519.PrivateKeySize {
		return errors.New("configure local Gateway smoke Agent key")
	}
	journal := workspacejournal.Journal{SchemaVersion: 1, TenantID: domain.TenantID(tenant), WorkspaceID: domain.WorkspaceID(workspace),
		ResourcePlaneID: domain.ResourcePlaneID(plane), WorkspaceRevision: 1, Revision: 1, OperationID: "op_smoke_gateway",
		OperationType: domain.OperationEnsureRunning, State: workspacejournal.Running, AllocationGeneration: 1,
		ProviderID: microVM, LaunchToken: "launch_smoke_gateway", LaunchSpecSHA256: strings.Repeat("a", 64),
		CleanupState: workspacejournal.CleanupNotRequired, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	snapshot := workspacejournal.Snapshot{Journal: journal, ETag: "smoke"}
	if err := snapshot.Journal.Validate(); err != nil {
		return errors.New("validate local Gateway smoke allocation journal")
	}
	agentIssuer := &workspaceagent.AgentTicketIssuer{KeyID: "local_agent", PrivateKey: agentKey, TTL: time.Minute}
	directory := &workspaceagent.CloudHypervisorWorkspaceVsockStreamDirectory{
		ResourcePlaneID: domain.ResourcePlaneID(plane), Journal: smokeWorkspaceJournal{snapshot: snapshot},
		StateDirectory: filepath.Dir(allocationDir), Tickets: agentIssuer, Origin: "https://workspace-gateway.invalid",
		Dial: func(ctx context.Context, _ string) (net.Conn, error) {
			return workspaceruntime.DialCloudHypervisorLocalGuest(ctx, allocationDir)
		},
	}
	_, sessionPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return errors.New("create local Gateway smoke Session key")
	}
	defer clear(sessionPrivate)
	sessionPublic := sessionPrivate.Public().(ed25519.PublicKey)
	sessionIssuer := &workspacesession.Ed25519Issuer{KeyID: "local_session", PrivateKey: sessionPrivate, TTL: time.Minute}
	gateway, err := workspacegateway.New(workspacegateway.Config{ResourcePlaneID: domain.ResourcePlaneID(plane),
		Verifier: &workspacesession.Verifier{Keys: map[string]ed25519.PublicKey{"local_session": sessionPublic},
			Audience: workspacesession.AudienceGateway, Lookup: smokeWorkspaceSessionLookup{}},
		Dialer: directory, AllowedOrigins: []string{"https://workspace-gateway.invalid"}, MaxConnections: 64, MaxConnectionsPerWorkspace: 32})
	if err != nil {
		return errors.New("configure local Workspace Gateway smoke")
	}
	server := httptest.NewTLSServer(gateway.Handler())
	defer server.Close()
	certs := x509.NewCertPool()
	certs.AddCert(server.Certificate())
	dialGateway := func(permission workspacesession.Permission, endpoint string) (*websocket.Conn, error) {
		channelToken, _, issueErr := sessionIssuer.Issue(workspacesession.Claims{UserID: "local-smoke-user", TenantID: domain.TenantID(tenant),
			WorkspaceID: domain.WorkspaceID(workspace), ResourcePlane: domain.ResourcePlaneID(plane), Permissions: []workspacesession.Permission{permission}})
		if issueErr != nil {
			return nil, errors.New("issue local Gateway channel Session")
		}
		channelConfig, configErr := websocket.NewConfig("wss"+strings.TrimPrefix(server.URL, "https")+endpoint, "https://workspace-gateway.invalid")
		if configErr != nil {
			return nil, errors.New("configure local Gateway channel WebSocket")
		}
		channelConfig.TlsConfig = &tls.Config{RootCAs: certs, MinVersion: tls.VersionTLS12}
		channelConfig.Protocol = []string{"hako.workspace.v1"}
		channelConfig.Header.Set("Authorization", "Bearer "+channelToken)
		channel, connectErr := websocket.DialConfig(channelConfig)
		if connectErr != nil {
			return nil, errors.New("connect signed Session to local Workspace Gateway")
		}
		channel.PayloadType = websocket.BinaryFrame
		_ = channel.SetDeadline(time.Now().Add(30 * time.Second))
		return channel, nil
	}
	conn, err := dialGateway(workspacesession.PermissionExec, "/v1/workspaces/"+workspace+"/exec")
	if err != nil {
		return err
	}
	request, _ := json.Marshal(workspaceprotocol.ExecRequest{Argv: []string{"/usr/bin/id"}})
	if err := websocket.Message.Send(conn, request); err != nil {
		_ = conn.Close()
		return errors.New("send local Gateway-to-Guest exec request")
	}
	var response []byte
	if err := websocket.Message.Receive(conn, &response); err != nil {
		_ = conn.Close()
		return errors.New("receive local Gateway-to-Guest exec result")
	}
	_ = conn.Close()
	var result workspaceprotocol.ExecResult
	if json.Unmarshal(response, &result) != nil || workspaceprotocol.ValidateExecResult(result) != nil ||
		result.ExitCode != 0 || !bytes.Contains(result.Stdout, []byte("uid=100000")) {
		return errors.New("local Gateway-to-Guest exec did not run as Workspace user")
	}
	if err := exerciseVSCodeBridge(dialGateway, workspace); err != nil {
		return fmt.Errorf("local Gateway VS Code channel failed: %w", err)
	}
	if err := exerciseBrowserIDE(ctx, dialGateway, workspace); err != nil {
		return fmt.Errorf("local Gateway browser IDE failed: %w", err)
	}
	if err := exerciseSSHBridge(dialGateway, workspace); err != nil {
		return fmt.Errorf("local Gateway SSH channel failed: %w", err)
	}
	if err := exerciseSSHLogin(ctx, dialGateway, workspace); err != nil {
		return fmt.Errorf("local Gateway SSH login failed: %w", err)
	}
	return nil
}

func exerciseBrowserIDE(ctx context.Context, dial func(workspacesession.Permission, string) (*websocket.Conn, error), workspace string) error {
	chromePath := strings.TrimSpace(os.Getenv("CHROMIUM_BIN"))
	if chromePath == "" {
		for _, candidate := range []string{"chromium", "chromium-browser", "google-chrome"} {
			if resolved, err := exec.LookPath(candidate); err == nil {
				chromePath = resolved
				break
			}
		}
	}
	if chromePath == "" {
		fmt.Println("HAKO_LOCAL_WORKSPACE_BROWSER_IDE_SMOKE_SKIPPED (Chromium not installed)")
		return nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return errors.New("listen for local Workspace browser IDE smoke")
	}
	defer listener.Close()
	stopAccept := make(chan struct{})
	defer close(stopAccept)
	go func() {
		for {
			client, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func(client net.Conn) {
				defer client.Close()
				guest, dialErr := dial(workspacesession.PermissionVSCode, "/v1/workspaces/"+workspace+"/vscode")
				if dialErr != nil {
					return
				}
				defer guest.Close()
				guest.PayloadType = websocket.BinaryFrame
				_ = guest.SetDeadline(time.Now().Add(30 * time.Second))
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(guest, client); done <- struct{}{} }()
				go func() { _, _ = io.Copy(client, guest); done <- struct{}{} }()
				<-done
				_ = client.Close()
				_ = guest.Close()
				<-done
			}(client)
			select {
			case <-stopAccept:
				return
			default:
			}
		}
	}()
	profileDir, err := os.MkdirTemp("", "hako-workspace-chromium-smoke-")
	if err != nil {
		return errors.New("create temporary browser IDE profile")
	}
	defer os.RemoveAll(profileDir)
	port := listener.Addr().(*net.TCPAddr).Port
	browserCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	command := exec.CommandContext(browserCtx, chromePath, "--headless", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
		"--disable-background-networking", "--disable-extensions", "--no-first-run", "--virtual-time-budget=12000",
		"--user-data-dir="+profileDir, "--dump-dom", fmt.Sprintf("http://127.0.0.1:%d/", port))
	output, err := command.CombinedOutput()
	if err != nil || !(bytes.Contains(output, []byte("code-server")) || bytes.Contains(output, []byte("Visual Studio Code")) || bytes.Contains(output, []byte("vscode"))) {
		return errors.New("headless browser did not render the Workspace IDE page")
	}
	return nil
}

func exerciseSSHBridge(dial func(workspacesession.Permission, string) (*websocket.Conn, error), workspace string) error {
	conn, err := dial(workspacesession.PermissionSSHProxy, "/v1/workspaces/"+workspace+"/ssh")
	if err != nil {
		return errors.New("connect to local Workspace SSH bridge")
	}
	defer conn.Close()
	conn.PayloadType = websocket.BinaryFrame
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	var banner []byte
	for !bytes.Contains(banner, []byte("\r\n")) {
		var frame []byte
		if err := websocket.Message.Receive(conn, &frame); err != nil {
			return errors.New("receive local Workspace SSH protocol banner")
		}
		banner = append(banner, frame...)
		if len(banner) > 4096 {
			return errors.New("local Workspace SSH protocol banner exceeded limit")
		}
	}
	if !bytes.HasPrefix(banner, []byte("SSH-2.0-")) {
		return errors.New("local Workspace SSH bridge returned an invalid protocol banner")
	}
	return nil
}

func exerciseSSHLogin(ctx context.Context, dial func(workspacesession.Permission, string) (*websocket.Conn, error), workspace string) error {
	keyDir, err := os.MkdirTemp("", "hako-workspace-ssh-smoke-")
	if err != nil {
		return errors.New("create temporary local SSH key directory")
	}
	defer os.RemoveAll(keyDir)
	privateKey := filepath.Join(keyDir, "id_ed25519")
	keygen := exec.CommandContext(ctx, "/usr/bin/ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", privateKey)
	if output, err := keygen.CombinedOutput(); err != nil || len(output) != 0 {
		return errors.New("generate temporary local SSH client key")
	}
	publicKey, err := os.ReadFile(privateKey + ".pub")
	if err != nil || len(publicKey) > 4096 {
		return errors.New("read temporary local SSH public key")
	}
	prepareConn, err := dial(workspacesession.PermissionExec, "/v1/workspaces/"+workspace+"/exec")
	if err != nil {
		return errors.New("connect to prepare Workspace SSH authorized-key directory")
	}
	prepareRequest, _ := json.Marshal(workspaceprotocol.ExecRequest{Argv: []string{"/usr/bin/mkdir", "-m", "0700", "-p", "/workspace/.ssh"}})
	if err := websocket.Message.Send(prepareConn, prepareRequest); err != nil {
		_ = prepareConn.Close()
		return errors.New("prepare Workspace SSH authorized-key directory")
	}
	var preparePayload []byte
	prepareErr := websocket.Message.Receive(prepareConn, &preparePayload)
	_ = prepareConn.Close()
	var prepareResult workspaceprotocol.ExecResult
	if prepareErr != nil || json.Unmarshal(preparePayload, &prepareResult) != nil || workspaceprotocol.ValidateExecResult(prepareResult) != nil || prepareResult.ExitCode != 0 {
		return errors.New("prepare Workspace SSH authorized-key directory")
	}
	if err := uploadWorkspaceSmokeFile(dial, workspace, "/workspace/.ssh/authorized_keys", publicKey); err != nil {
		return fmt.Errorf("install temporary Workspace SSH smoke key: %w", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return errors.New("listen for local SSH smoke client")
	}
	defer listener.Close()
	relayResult := make(chan error, 1)
	go func() {
		client, acceptErr := listener.Accept()
		if acceptErr != nil {
			relayResult <- acceptErr
			return
		}
		defer client.Close()
		guest, dialErr := dial(workspacesession.PermissionSSHProxy, "/v1/workspaces/"+workspace+"/ssh")
		if dialErr != nil {
			relayResult <- dialErr
			return
		}
		defer guest.Close()
		guest.PayloadType = websocket.BinaryFrame
		_ = guest.SetDeadline(time.Now().Add(20 * time.Second))
		copyDone := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(guest, client); copyDone <- struct{}{} }()
		go func() { _, _ = io.Copy(client, guest); copyDone <- struct{}{} }()
		<-copyDone
		_ = client.Close()
		_ = guest.Close()
		<-copyDone
		relayResult <- nil
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	sshCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	command := exec.CommandContext(sshCtx, "/usr/bin/ssh", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "LogLevel=ERROR", "-i", privateKey,
		"-p", strconv.Itoa(port), "hako-workspace@127.0.0.1", "/usr/bin/id -u")
	output, err := command.CombinedOutput()
	var relayErr error
	select {
	case relayErr = <-relayResult:
	case <-time.After(20 * time.Second):
		_ = listener.Close()
		relayErr = errors.New("local SSH smoke relay timed out")
	}
	if err != nil || relayErr != nil || strings.TrimSpace(string(output)) != "100000" {
		return errors.New("local SSH client did not authenticate and execute in the Workspace")
	}
	return nil
}

func downloadWorkspaceSmokeFile(dial func(workspacesession.Permission, string) (*websocket.Conn, error), workspace string) ([]byte, error) {
	conn, err := dial(workspacesession.PermissionFileTransfer, "/v1/workspaces/"+workspace+"/files")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := workspaceprotocol.WriteFileRequest(conn, workspaceprotocol.FileRequest{Operation: "download", Path: "/workspace/smoke.txt"}); err != nil {
		return nil, errors.New("send local Workspace Guest download request")
	}
	response, err := workspaceprotocol.ReadFileResponse(conn)
	if err != nil || !response.OK || response.Size < 0 || response.Size > 4096 {
		return nil, errors.New("local Workspace Guest download was not accepted")
	}
	downloaded := make([]byte, int(response.Size))
	if _, err := io.ReadFull(conn, downloaded); err != nil {
		return nil, err
	}
	return downloaded, nil
}

func uploadWorkspaceSmokeFile(dial func(workspacesession.Permission, string) (*websocket.Conn, error), workspace, path string, payload []byte) error {
	conn, err := dial(workspacesession.PermissionFileTransfer, "/v1/workspaces/"+workspace+"/files")
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := workspaceprotocol.WriteFileRequest(conn, workspaceprotocol.FileRequest{Operation: "upload", Path: path, Size: int64(len(payload))}); err != nil {
		return err
	}
	if _, err := conn.Write(payload); err != nil {
		return err
	}
	response, err := workspaceprotocol.ReadFileResponse(conn)
	if err != nil || !response.OK || response.Size != int64(len(payload)) {
		if err != nil {
			return errors.New("Workspace smoke file upload response was invalid")
		}
		return fmt.Errorf("Workspace smoke file upload was rejected (code=%q)", response.Code)
	}
	return nil
}

func waitForWorkspaceAgent(ctx context.Context, allocationDir, tenant, workspace, plane, microVM string) error {
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		if bootErr := guestBootError(filepath.Join(allocationDir, "cloud-hypervisor.log")); bootErr != nil {
			return bootErr
		}
		dialCtx, dialCancel := context.WithTimeout(ctx, time.Second)
		conn, err := workspaceruntime.DialCloudHypervisorLocalGuest(dialCtx, allocationDir)
		dialCancel()
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			_, err = io.WriteString(conn, "GET /internal/heartbeat HTTP/1.1\r\nHost: workspace-agent.vsock\r\nConnection: close\r\n\r\n")
			if err == nil {
				response, readErr := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
				if readErr == nil {
					var body struct {
						ProtocolVersion int    `json:"protocol_version"`
						TenantID        string `json:"tenant_id"`
						WorkspaceID     string `json:"workspace_id"`
						ResourcePlaneID string `json:"resource_plane_id"`
						MicroVMID       string `json:"microvm_id"`
					}
					decodeErr := json.NewDecoder(io.LimitReader(response.Body, 4097)).Decode(&body)
					_ = response.Body.Close()
					if decodeErr == nil && response.StatusCode == http.StatusOK && body.ProtocolVersion == 1 &&
						body.TenantID == tenant && body.WorkspaceID == workspace &&
						body.ResourcePlaneID == plane && body.MicroVMID == microVM {
						_ = conn.Close()
						return nil
					}
					err = errors.New("Guest Agent heartbeat identity or protocol mismatch")
				} else {
					err = readErr
				}
			}
			_ = conn.Close()
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("Hako Workspace Agent did not become ready: %w", lastErr)
		case <-ticker.C:
		}
	}
}

func guestBootError(logPath string) error {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	log := string(data)
	for _, marker := range []string{"Kernel panic - not syncing:", "Initramfs unpacking failed:", "Hako local Workspace Guest stopped"} {
		if index := strings.Index(log, marker); index >= 0 {
			line := log[index:]
			if end := strings.IndexByte(line, '\n'); end >= 0 {
				line = line[:end]
			}
			return fmt.Errorf("Guest boot stopped: %s", strings.TrimSpace(line))
		}
	}
	return nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
