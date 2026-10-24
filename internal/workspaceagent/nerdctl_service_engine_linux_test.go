//go:build linux

package workspaceagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/servicecatalog"
)

type nerdctlCall struct {
	args  []string
	env   []string
	input []byte
}

type nerdctlRunnerFake struct {
	calls        []nerdctlCall
	network      bool
	exists       bool
	running      bool
	failCommand  string
	ignoreRemove bool
	envFileData  []byte
	envFileMode  os.FileMode
	envFilePath  string
}

func (r *nerdctlRunnerFake) Run(_ context.Context, _ string, args, env []string) ([]byte, error) {
	r.calls = append(r.calls, nerdctlCall{args: append([]string(nil), args...), env: append([]string(nil), env...)})
	if len(args) < 5 {
		return nil, errors.New("invalid fake invocation")
	}
	operation := strings.Join(args[4:], " ")
	if r.failCommand != "" && strings.HasPrefix(operation, r.failCommand) {
		return nil, errors.New("fake containerd failure")
	}
	switch args[4] {
	case "network":
		if len(args) > 5 && args[5] == "ls" {
			if r.network {
				return []byte("hako-ws_1\n"), nil
			}
			return nil, nil
		}
		r.network = true
		return nil, nil
	case "ps":
		if r.exists {
			state := "exited"
			if r.running {
				state = "running"
			}
			return []byte("hako-svc_1 " + state + "\n"), nil
		}
		return nil, nil
	case "run":
		for index, argument := range args {
			if argument == "--env-file" && index+1 < len(args) {
				r.envFilePath = args[index+1]
				r.envFileData, _ = os.ReadFile(r.envFilePath)
				if info, err := os.Stat(r.envFilePath); err == nil {
					r.envFileMode = info.Mode().Perm()
				}
			}
		}
		r.exists, r.running = true, true
	case "start":
		r.exists, r.running = true, true
	case "stop":
		r.running = false
	case "rm":
		if !r.ignoreRemove {
			r.exists, r.running = false, false
		}
	case "exec":
	}
	return nil, nil
}

func (r *nerdctlRunnerFake) RunInput(_ context.Context, _ string, args, env []string, input []byte) ([]byte, error) {
	r.calls = append(r.calls, nerdctlCall{args: append([]string(nil), args...), env: append([]string(nil), env...), input: append([]byte(nil), input...)})
	return nil, nil
}

func newTestNerdctlEngine(t *testing.T, runner ServiceCommandRunner) *NerdctlServiceEngine {
	t.Helper()
	root := t.TempDir()
	engine, err := NewNerdctlServiceEngine(NerdctlServiceEngineConfig{ContainerdSocket: "/run/user/100000/containerd/containerd.sock",
		WorkspaceRoot: root, WorkspaceID: "ws_1", RunAsUID: 100000, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	// Unit tests use a temporary directory instead of mounting tmpfs. Production
	// construction always uses the Linux filesystem-type check.
	engine.isTmpfs = func(string) bool { return true }
	return engine
}

func TestNerdctlServiceEngineUsesOnlyCatalogInputsAndLoopbackPorts(t *testing.T) {
	runner := &nerdctlRunnerFake{network: true}
	engine := newTestNerdctlEngine(t, runner)
	template, err := servicecatalog.Resolve("redis")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Ensure(context.Background(), "svc_1", "redis", template, nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("ensure calls = %+v", runner.calls)
	}
	args := strings.Join(runner.calls[2].args, " ")
	for _, required := range []string{"run --detach --restart=always --name hako-svc_1", "--network hako-ws_1", "hako.workspace-id=ws_1", "hako.service-id=svc_1", "127.0.0.1:6379:6379/tcp", "--memory 268435456", "--cpus 0.250", template.Image} {
		if !strings.Contains(args, required) {
			t.Errorf("nerdctl args %q missing %q", args, required)
		}
	}
	if strings.Contains(args, "--privileged") || strings.Contains(args, "--network host") || strings.Contains(args, "0.0.0.0") {
		t.Fatalf("unsafe container settings passed to nerdctl: %s", args)
	}
	if strings.Join(runner.calls[2].env, " ") != "PATH=/usr/local/bin:/usr/bin:/bin HOME="+engine.config.WorkspaceRoot+" XDG_RUNTIME_DIR=/run/user/100000 CONTAINERD_SNAPSHOTTER=native" {
		t.Fatalf("container runtime inherited unexpected environment: %q", runner.calls[2].env)
	}
}

func TestNerdctlServiceEngineRestartsStopsRemovesAndChecksHealth(t *testing.T) {
	runner := &nerdctlRunnerFake{network: true, exists: true}
	engine := newTestNerdctlEngine(t, runner)
	template, _ := servicecatalog.Resolve("redis")
	if err := engine.Ensure(context.Background(), "svc_1", "redis", template, nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 || !strings.HasPrefix(strings.Join(runner.calls[2].args[4:], " "), "start hako-svc_1") {
		t.Fatalf("stopped container was not restarted: %+v", runner.calls)
	}
	if healthy, err := engine.Healthy(context.Background(), "svc_1", template.HealthCheck); err != nil || !healthy {
		t.Fatalf("health check = healthy %v, err %v", healthy, err)
	}
	if err := engine.Stop(context.Background(), "svc_1"); err != nil || runner.running {
		t.Fatalf("stop = running %v, err %v", runner.running, err)
	}
	if err := engine.Remove(context.Background(), "svc_1"); err != nil || runner.exists {
		t.Fatalf("remove = exists %v, err %v", runner.exists, err)
	}
	if _, err := os.Lstat(filepath.Join(engine.config.WorkspaceRoot, ".hako", "services", "svc_1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("service data survived confirmed removal: %v", err)
	}
}

func TestNerdctlServiceEngineRemovalDoesNotFollowWorkspaceSymlinks(t *testing.T) {
	runner := &nerdctlRunnerFake{network: true}
	engine := newTestNerdctlEngine(t, runner)
	serviceRoot := filepath.Join(engine.config.WorkspaceRoot, ".hako", "services", "svc_1")
	outside := t.TempDir()
	marker := filepath.Join(outside, "keep")
	if err := os.WriteFile(marker, []byte("user data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(serviceRoot), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, serviceRoot); err != nil {
		t.Fatal(err)
	}
	if err := engine.Remove(context.Background(), "svc_1"); err == nil {
		t.Fatal("cleanup reported success for a symlinked Service data directory")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "user data" {
		t.Fatalf("cleanup followed a Workspace symlink: data=%q err=%v", data, err)
	}
}

func TestNerdctlServiceEngineDoesNotDeleteDataBeforeContainerRemovalIsConfirmed(t *testing.T) {
	runner := &nerdctlRunnerFake{network: true, exists: true, running: true, ignoreRemove: true}
	engine := newTestNerdctlEngine(t, runner)
	serviceData := filepath.Join(engine.config.WorkspaceRoot, ".hako", "services", "svc_1", "data")
	if err := os.MkdirAll(serviceData, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(serviceData, "keep")
	if err := os.WriteFile(marker, []byte("pending cleanup"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := engine.Remove(context.Background(), "svc_1"); err == nil {
		t.Fatal("removal succeeded even though the runtime still reported the container")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "pending cleanup" {
		t.Fatalf("Service data was removed before container cleanup was confirmed: data=%q err=%v", data, err)
	}
}

func TestNerdctlServiceEngineFailsClosedOnUnapprovedOrSecretTemplates(t *testing.T) {
	runner := &nerdctlRunnerFake{network: true}
	engine := newTestNerdctlEngine(t, runner)
	template, _ := servicecatalog.Resolve("redis")
	template.Image = "attacker.invalid/root:latest"
	if err := engine.Ensure(context.Background(), "svc_1", "redis", template, nil); err == nil || len(runner.calls) != 0 {
		t.Fatalf("unapproved image reached containerd: err=%v calls=%+v", err, runner.calls)
	}
	postgres, _ := servicecatalog.Resolve("postgres")
	if err := engine.Ensure(context.Background(), "svc_1", "postgres", postgres, nil); !errors.Is(err, ErrServiceSecretPolicy) || len(runner.calls) != 0 {
		t.Fatalf("generated-secret template was not blocked before containerd: err=%v calls=%+v", err, runner.calls)
	}
	if _, err := NewNerdctlServiceEngine(NerdctlServiceEngineConfig{ContainerdSocket: "/run/containerd.sock", WorkspaceRoot: "/", WorkspaceID: "ws_1", RunAsUID: 100000, Runner: runner}); err == nil {
		t.Fatal("root filesystem was accepted as a Workspace data root")
	}
}

func TestNerdctlServiceEngineInjectsGeneratedSecretThroughEphemeralEnvFile(t *testing.T) {
	runner := &nerdctlRunnerFake{network: true}
	engine := newTestNerdctlEngine(t, runner)
	envDir := t.TempDir()
	engine.config.EnvFileDirectory = envDir
	postgres, _ := servicecatalog.Resolve("postgres")
	secret := "0123456789012345678901234567890123456789012"
	if err := engine.Ensure(context.Background(), "svc_1", "postgres", postgres, map[string]string{"POSTGRES_PASSWORD": secret}); err != nil {
		t.Fatal(err)
	}
	if string(runner.envFileData) != "POSTGRES_PASSWORD="+secret+"\n" || runner.envFileMode != 0600 {
		t.Fatalf("ephemeral env file contents/mode = %q / %o", runner.envFileData, runner.envFileMode)
	}
	if _, err := os.Lstat(runner.envFilePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ephemeral secret file remains after container creation: %v", err)
	}
	args := strings.Join(runner.calls[len(runner.calls)-1].args, " ")
	if strings.Contains(args, secret) || !strings.Contains(args, "--env-file "+runner.envFilePath) {
		t.Fatalf("secret was passed in process arguments or env-file path missing: %q", args)
	}
	for _, entry := range runner.calls[len(runner.calls)-1].env {
		if strings.Contains(entry, secret) {
			t.Fatalf("secret leaked to nerdctl process environment: %q", entry)
		}
	}
}

func TestNerdctlServiceEngineAppliesPostgresCredentialRotationThroughStdin(t *testing.T) {
	secret := "0123456789012345678901234567890123456789012"
	runner := &nerdctlRunnerFake{network: true, exists: true, running: true}
	engine := newTestNerdctlEngine(t, runner)
	postgres, _ := servicecatalog.Resolve("postgres")
	if err := engine.Ensure(context.Background(), "svc_1", "postgres", postgres, map[string]string{"POSTGRES_PASSWORD": secret}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("existing PostgreSQL service calls = %d, want network lookup, state lookup, credential update: %+v", len(runner.calls), runner.calls)
	}
	call := runner.calls[2]
	if strings.Join(call.args, " ") != "--address /run/user/100000/containerd/containerd.sock exec --interactive --user postgres hako-svc_1 psql -v ON_ERROR_STOP=1 -U postgres -d postgres" {
		t.Fatalf("credential update used unexpected command: %q", call.args)
	}
	if string(call.input) != "ALTER ROLE postgres WITH PASSWORD '"+secret+"';\n" {
		t.Fatalf("credential update stdin = %q", call.input)
	}
	if strings.Contains(strings.Join(call.args, " "), secret) || strings.Contains(strings.Join(call.env, " "), secret) {
		t.Fatal("rotated credential leaked into command arguments or environment")
	}
}

func TestNerdctlServiceEngineRejectsSecretFileDirectoryOutsideTmpfs(t *testing.T) {
	runner := &nerdctlRunnerFake{network: true}
	engine := newTestNerdctlEngine(t, runner)
	engine.isTmpfs = func(string) bool { return false }
	postgres, _ := servicecatalog.Resolve("postgres")
	err := engine.Ensure(context.Background(), "svc_1", "postgres", postgres,
		map[string]string{"POSTGRES_PASSWORD": "0123456789012345678901234567890123456789012"})
	if err == nil || len(runner.calls) != 2 {
		t.Fatalf("secret was allowed outside an ephemeral filesystem: err=%v calls=%+v", err, runner.calls)
	}
}

func TestNerdctlServiceEngineRejectsSymlinkedWorkspaceVolumePath(t *testing.T) {
	runner := &nerdctlRunnerFake{}
	engine := newTestNerdctlEngine(t, runner)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(engine.config.WorkspaceRoot, ".hako")); err != nil {
		t.Fatal(err)
	}
	postgres, _ := servicecatalog.Resolve("postgres")
	if err := engine.prepareVolumes(postgres, domain.ServiceID("svc_1")); err == nil {
		t.Fatal("symlinked service volume path was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "services")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink check created files outside Workspace root: %v", err)
	}
}
