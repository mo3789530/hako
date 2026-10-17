//go:build linux

package workspaceagent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type supervisorTestLauncher struct {
	launch  WorkspaceAgentLaunch
	process *supervisorTestProcess
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
		if r.URL.Path == "/internal/ready" {
			w.WriteHeader(http.StatusNoContent)
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

func (p *supervisorTestProcess) Done() <-chan error { return p.done }

func (p *supervisorTestProcess) Stop(ctx context.Context) error {
	return p.server.Shutdown(ctx)
}

func TestWorkspaceAgentSupervisorStartsBoundChildAndProxiesOnlyWorkspaceRoutes(t *testing.T) {
	launcher := &supervisorTestLauncher{}
	socket := filepath.Join(t.TempDir(), "agent.sock")
	supervisor, err := NewLinuxWorkspaceAgentSupervisor(WorkspaceAgentSupervisorConfig{
		SocketPath: socket, BinaryPath: "/image/hako-workspace-agent", TrustedKeysFile: "/etc/hako/public.json",
		GatewayOrigin: "https://gateway.hako.test", Launcher: launcher, isRoot: func() bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := testMountSpec()
	if err := supervisor.BindWorkspaceRuntime("microvm-1", spec); err != nil {
		t.Fatalf("bind and wait for child readiness: %v", err)
	}
	if launcher.launch.MicroVMID != "microvm-1" || launcher.launch.Mount.WorkspaceID != spec.WorkspaceID ||
		launcher.launch.GatewayOrigin != "https://gateway.hako.test" || launcher.launch.TrustedKeysFile != "/etc/hako/public.json" {
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
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remains after shutdown: %v", err)
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
