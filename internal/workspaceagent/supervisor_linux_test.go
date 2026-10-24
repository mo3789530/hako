//go:build linux

package workspaceagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

type supervisorTestLauncher struct {
	launch              WorkspaceAgentLaunch
	process             *supervisorTestProcess
	heartbeatOverride   atomic.Pointer[workspaceAgentHeartbeat]
	heartbeatHTTPStatus atomic.Int32
}

func (l *supervisorTestLauncher) Start(config WorkspaceAgentLaunch, listener net.Listener) (WorkspaceAgentProcess, error) {
	l.launch = config
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		return nil, errors.New("expected Unix listener")
	}
	file, err := unixListener.File()
	if err != nil {
		return nil, err
	}
	childListener, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/heartbeat" {
			if status := l.heartbeatHTTPStatus.Load(); status != 0 {
				w.WriteHeader(int(status))
				return
			}
			heartbeat := workspaceAgentHeartbeat{ProtocolVersion: workspaceAgentProtocolVersion,
				TenantID: domain.TenantID(config.Mount.TenantID), WorkspaceID: domain.WorkspaceID(config.Mount.WorkspaceID),
				ResourcePlaneID: domain.ResourcePlaneID(config.Mount.ResourcePlaneID), MicroVMID: config.MicroVMID}
			if override := l.heartbeatOverride.Load(); override != nil {
				heartbeat = *override
			}
			_ = json.NewEncoder(w).Encode(heartbeat)
			return
		}
		if r.URL.Path != "/v1/workspaces/workspace-1/shell" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Test-Workspace", config.Mount.WorkspaceID)
		w.WriteHeader(http.StatusAccepted)
	})}
	process := &supervisorTestProcess{server: server, done: make(chan error, 1)}
	l.process = process
	go func() {
		err := server.Serve(childListener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		process.done <- err
		close(process.done)
	}()
	return process, nil
}

type supervisorTestProcess struct {
	server *http.Server
	done   chan error
}

type supervisorTestContainerd struct {
	socket  string
	stopped bool
}

func (p *supervisorTestContainerd) SocketPath() string { return p.socket }
func (p *supervisorTestContainerd) Stop(context.Context) error {
	p.stopped = true
	return nil
}

func (p *supervisorTestProcess) Done() <-chan error { return p.done }

func (p *supervisorTestProcess) Stop(ctx context.Context) error {
	return p.server.Shutdown(ctx)
}

func TestWorkspaceAgentSupervisorStartsBoundChildAndProxiesOnlyWorkspaceRoutes(t *testing.T) {
	launcher := &supervisorTestLauncher{}
	containerd := &supervisorTestContainerd{socket: "/run/user/100123/containerd/containerd.sock"}
	containerdStarts := 0
	root := t.TempDir()
	socket := filepath.Join(root, "agent.sock")
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trustedKeys, err := json.Marshal(map[string]string{"rp_key": base64.RawURLEncoding.EncodeToString(publicKey)})
	if err != nil {
		t.Fatal(err)
	}
	trustedKeysPath := filepath.Join(root, "public-keys.json")
	if err := os.WriteFile(trustedKeysPath, trustedKeys, 0600); err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewLinuxWorkspaceAgentSupervisor(WorkspaceAgentSupervisorConfig{
		SocketPath: socket, BinaryPath: "/image/hako-workspace-agent", TrustedKeysFile: trustedKeysPath,
		ServiceFenceDirectory: filepath.Join(root, "service-fences"),
		GatewayOrigin:         "https://gateway.hako.test", Launcher: launcher, isRoot: func() bool { return true }, EnableServices: true,
		HeartbeatInterval: 10 * time.Millisecond, HeartbeatLease: 50 * time.Millisecond,
		PrepareRuntimeDirectory: func(uid, gid uint32) error {
			if uid != testMountSpec().UID || gid != testMountSpec().GID {
				t.Fatalf("runtime directory identity = %d:%d", uid, gid)
			}
			return nil
		},
		PrepareRootlessIdentity: func(uid, gid uint32) error {
			if uid != testMountSpec().UID || gid != testMountSpec().GID {
				t.Fatalf("rootless identity = %d:%d", uid, gid)
			}
			return nil
		},
		StartRootlessContainerd: func(_ context.Context, config RootlessContainerdConfig) (RootlessContainerdRuntime, error) {
			containerdStarts++
			if config.UID != testMountSpec().UID || config.GID != testMountSpec().GID || config.WorkspaceRoot != "/workspace" {
				t.Fatalf("containerd config = %+v", config)
			}
			return containerd, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := testMountSpec()
	if err := supervisor.BindWorkspaceRuntime("microvm-1", spec); err != nil {
		t.Fatalf("bind and wait for child readiness: %v", err)
	}
	if launcher.launch.MicroVMID != "microvm-1" || launcher.launch.Mount.WorkspaceID != spec.WorkspaceID ||
		launcher.launch.GatewayOrigin != "https://gateway.hako.test" || launcher.launch.TrustedKeysFile != trustedKeysPath || !launcher.launch.EnableServices ||
		launcher.launch.ContainerdSocket != containerd.socket {
		t.Fatalf("child launch config = %+v", launcher.launch)
	}
	if info, err := os.Stat(socket); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("socket mode = %v, err = %v; want 0600", info, err)
	}
	response := httptest.NewRecorder()
	supervisor.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://guest/v1/workspaces/workspace-1/shell", nil))
	if response.Code != http.StatusAccepted || response.Header().Get("X-Test-Workspace") != "workspace-1" {
		t.Fatalf("proxied response = %d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
	foreignHeartbeat := workspaceAgentHeartbeat{ProtocolVersion: workspaceAgentProtocolVersion,
		TenantID: domain.TenantID(spec.TenantID), WorkspaceID: domain.WorkspaceID(spec.WorkspaceID),
		ResourcePlaneID: domain.ResourcePlaneID(spec.ResourcePlaneID), MicroVMID: "foreign-microvm"}
	launcher.heartbeatOverride.Store(&foreignHeartbeat)
	response = httptest.NewRecorder()
	supervisor.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://guest/v1/workspaces/workspace-1/shell", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("foreign Agent heartbeat status = %d, want fail-closed 503", response.Code)
	}
	unsupportedHeartbeat := foreignHeartbeat
	unsupportedHeartbeat.ProtocolVersion = workspaceAgentProtocolVersion + 1
	unsupportedHeartbeat.MicroVMID = "microvm-1"
	launcher.heartbeatOverride.Store(&unsupportedHeartbeat)
	response = httptest.NewRecorder()
	supervisor.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://guest/v1/workspaces/workspace-1/shell", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unsupported Agent heartbeat version status = %d, want fail-closed 503", response.Code)
	}
	launcher.heartbeatOverride.Store(nil)
	launcher.heartbeatHTTPStatus.Store(http.StatusServiceUnavailable)
	response = httptest.NewRecorder()
	supervisor.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://guest/v1/workspaces/workspace-1/shell", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable Agent heartbeat status = %d, want fail-closed 503", response.Code)
	}
	deadline := time.Now().Add(time.Second)
	for supervisor.heartbeatLeaseActive(time.Now()) {
		if time.Now().After(deadline) {
			t.Fatal("heartbeat lease did not expire after repeated failures")
		}
		time.Sleep(time.Millisecond)
	}
	response = httptest.NewRecorder()
	supervisor.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://guest/v1/workspaces/workspace-1/shell", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("expired Agent heartbeat lease status = %d, want fail-closed 503", response.Code)
	}
	launcher.heartbeatHTTPStatus.Store(0)
	deadline = time.Now().Add(time.Second)
	for !supervisor.heartbeatLeaseActive(time.Now()) {
		if time.Now().After(deadline) {
			t.Fatal("heartbeat lease did not recover after valid Agent response")
		}
		time.Sleep(time.Millisecond)
	}
	response = httptest.NewRecorder()
	supervisor.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://guest/v1/workspaces/workspace-1/shell", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("route after heartbeat lease recovery = %d", response.Code)
	}
	firstChild := launcher.process
	if err := firstChild.server.Close(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for {
		select {
		case <-firstChild.Done():
			goto childStopped
		default:
			if time.Now().After(deadline) {
				t.Fatal("Workspace Agent child did not stop")
			}
			time.Sleep(time.Millisecond)
		}
	}
childStopped:
	response = httptest.NewRecorder()
	supervisor.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://guest/v1/workspaces/workspace-1/shell", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("route after child restart = %d body=%q", response.Code, response.Body.String())
	}
	if launcher.process == firstChild || supervisor.serviceGuard == nil || containerdStarts != 1 {
		t.Fatal("Workspace Agent child was not restarted with a durable Service guard")
	}
	for _, path := range []string{"/internal/ready", "/v1/workspaces/workspace-2/shell", "/other"} {
		response = httptest.NewRecorder()
		supervisor.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://guest"+path, nil))
		if response.Code != http.StatusNotFound {
			t.Errorf("route %q status = %d, want 404", path, response.Code)
		}
	}
	foreign := spec
	foreign.WorkspaceID = "workspace-2"
	if err := supervisor.BindWorkspaceRuntime("microvm-2", foreign); err == nil {
		t.Fatal("accepted a second runtime identity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.Stop(ctx); err != nil {
		t.Fatalf("stop child: %v", err)
	}
	if !containerd.stopped {
		t.Fatal("rootless containerd was not stopped with the Workspace Agent supervisor")
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remains after shutdown: %v", err)
	}
}

func TestWorkspaceAgentSupervisorStopsContainerdWhenSocketIsNotTheFixedWorkspacePath(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trustedKeys, err := json.Marshal(map[string]string{"rp_key": base64.RawURLEncoding.EncodeToString(publicKey)})
	if err != nil {
		t.Fatal(err)
	}
	trustedKeysPath := filepath.Join(t.TempDir(), "public-keys.json")
	if err := os.WriteFile(trustedKeysPath, trustedKeys, 0600); err != nil {
		t.Fatal(err)
	}
	daemon := &supervisorTestContainerd{socket: "/tmp/attacker-controlled.sock"}
	containerdStarted := false
	root := t.TempDir()
	supervisor, err := NewLinuxWorkspaceAgentSupervisor(WorkspaceAgentSupervisorConfig{
		SocketPath: filepath.Join(root, "agent.sock"), BinaryPath: "/image/hako-workspace-agent",
		TrustedKeysFile: trustedKeysPath, GatewayOrigin: "https://gateway.hako.test", EnableServices: true,
		ServiceFenceDirectory: filepath.Join(root, "service-fences"),
		Launcher:              &supervisorTestLauncher{}, isRoot: func() bool { return true },
		PrepareRuntimeDirectory: func(uint32, uint32) error { return nil },
		PrepareRootlessIdentity: func(uint32, uint32) error { return nil },
		StartRootlessContainerd: func(context.Context, RootlessContainerdConfig) (RootlessContainerdRuntime, error) {
			containerdStarted = true
			return daemon, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.BindWorkspaceRuntime("microvm-1", testMountSpec()); err == nil {
		t.Fatal("accepted a containerd socket outside the fixed Workspace runtime path")
	}
	if !containerdStarted || !daemon.stopped {
		t.Fatalf("rootless containerd cleanup: started=%t stopped=%t", containerdStarted, daemon.stopped)
	}
}

func TestWorkspaceAgentSupervisorRejectsUnprivilegedInitAndBadImageConfig(t *testing.T) {
	if _, err := NewLinuxWorkspaceAgentSupervisor(WorkspaceAgentSupervisorConfig{isRoot: func() bool { return false }}); err == nil {
		t.Fatal("accepted unprivileged lifecycle supervisor")
	}
	for _, config := range []WorkspaceAgentSupervisorConfig{
		{SocketPath: "relative.sock", TrustedKeysFile: "/keys", GatewayOrigin: "https://gateway.test", isRoot: func() bool { return true }},
		{SocketPath: filepath.Join(t.TempDir(), "agent.sock"), TrustedKeysFile: "/keys", GatewayOrigin: "http://gateway.test", isRoot: func() bool { return true }},
	} {
		if _, err := NewLinuxWorkspaceAgentSupervisor(config); err == nil {
			t.Errorf("accepted invalid config %+v", config)
		}
	}
}

func TestWorkspaceAgentSupervisorFailsClosedWhenRuntimeTmpfsCannotBePrepared(t *testing.T) {
	root := t.TempDir()
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trustedKeys, err := json.Marshal(map[string]string{"rp_key": base64.RawURLEncoding.EncodeToString(publicKey)})
	if err != nil {
		t.Fatal(err)
	}
	trustedKeysPath := filepath.Join(root, "public-keys.json")
	if err := os.WriteFile(trustedKeysPath, trustedKeys, 0600); err != nil {
		t.Fatal(err)
	}
	launcher := &supervisorTestLauncher{}
	supervisor, err := NewLinuxWorkspaceAgentSupervisor(WorkspaceAgentSupervisorConfig{
		SocketPath: filepath.Join(root, "agent.sock"), TrustedKeysFile: trustedKeysPath,
		GatewayOrigin: "https://gateway.hako.test", Launcher: launcher, isRoot: func() bool { return true },
		PrepareRuntimeDirectory: func(uint32, uint32) error { return errors.New("tmpfs unavailable") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.BindWorkspaceRuntime("microvm-1", testMountSpec()); err == nil || launcher.process != nil {
		t.Fatalf("Agent started without its private runtime tmpfs: err=%v launcher=%+v", err, launcher)
	}
}

func TestWorkspaceAgentChildPayloadStrictlyValidatesIdentityAndBounds(t *testing.T) {
	payload := `{"microvm_id":"microvm-1","mount":{"tenant_id":"tenant-1","workspace_id":"workspace-1","resource_plane_id":"rp-1","file_system_id":"fs-0123456789abcdef0","region":"us-east-1","access_point_id":"fsap-0123456789abcdef0","root_path":"/hako/0123456789abcdef/1123456789abcdef/2123456789abcdef","mount_path":"/workspace","uid":100123,"gid":100123,"tls":true,"iam":true},"gateway_origin":"https://gateway.hako.test"}`
	parsed, err := ReadWorkspaceAgentChildPayload(strings.NewReader(payload))
	if err != nil || parsed.MicroVMID != "microvm-1" || parsed.Mount.WorkspaceID != "workspace-1" {
		t.Fatalf("read child payload = %+v, err = %v", parsed, err)
	}
	for _, malformed := range []string{
		strings.TrimSuffix(payload, "}") + `,"command":"/bin/sh"}`,
		strings.Replace(payload, `"uid":100123`, `"uid":0`, 1),
		payload + strings.Repeat(" ", maxWorkspaceHookBytes),
	} {
		if _, err := ReadWorkspaceAgentChildPayload(strings.NewReader(malformed)); err == nil {
			t.Error("accepted malformed, privileged, or oversized child payload")
		}
	}
}
