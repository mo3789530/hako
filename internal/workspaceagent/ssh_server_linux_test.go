//go:build linux

package workspaceagent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type sshServerCommandCall struct {
	binary string
	args   []string
}

type sshServerCommandStub struct {
	calls []sshServerCommandCall
	err   error
}

func (s *sshServerCommandStub) Run(_ context.Context, binary string, args ...string) error {
	s.calls = append(s.calls, sshServerCommandCall{binary: binary, args: append([]string(nil), args...)})
	if strings.HasSuffix(binary, "ssh-keygen") {
		keyPath := args[len(args)-1]
		if err := os.WriteFile(keyPath, []byte("synthetic-private-key"), 0600); err != nil {
			return err
		}
		if err := os.WriteFile(keyPath+".pub", []byte("ssh-ed25519 synthetic-public-key"), 0644); err != nil {
			return err
		}
	}
	return s.err
}

type sshServerProcessStub struct{ stopped int }

func (p *sshServerProcessStub) Stop(context.Context) error { p.stopped++; return nil }

func workspaceSSHMountSpec() WorkspaceMountSpec {
	return WorkspaceMountSpec{TenantID: "tenant_ssh", WorkspaceID: "workspace_ssh", ResourcePlaneID: "rp_ssh",
		FileSystemID: "fs-0123456789abcdef", Region: "ap-northeast-1", AccessPointID: "fsap-0123456789abcdef",
		RootPath: "/hako/0123456789abcdef/1123456789abcdef/2123456789abcdef", MountPath: "/workspace",
		UID: 145678, GID: 145678, TLS: true, IAM: true}
}

func TestWorkspaceSSHServerUsesLoopbackAndStableWorkspaceHostKey(t *testing.T) {
	root := t.TempDir()
	runner := &sshServerCommandStub{}
	process := &sshServerProcessStub{}
	identityCalls := 0
	started := 0
	var startArgs []string
	starter := &linuxWorkspaceSSHServerStarter{root: root, runner: runner,
		processStarter: func(binary string, args ...string) (WorkspaceSSHServerProcess, error) {
			started++
			startArgs = append([]string{binary}, args...)
			return process, nil
		},
		writeFile: os.WriteFile, mkdirAll: os.MkdirAll, stat: os.Lstat,
		ensureIdentity: func(uid, gid uint32) error {
			identityCalls++
			if uid != 145678 || gid != 145678 {
				t.Fatalf("SSH account identity = %d/%d", uid, gid)
			}
			return nil
		}}

	got, err := starter.Start(context.Background(), workspaceSSHMountSpec())
	if err != nil {
		t.Fatalf("Start() process=%v error=%v", got, err)
	}
	runtimeProcess, ok := got.(*workspaceSSHServerRuntime)
	if !ok || runtimeProcess.process != process {
		t.Fatalf("Start() process=%T, want SSH runtime wrapper", got)
	}
	if identityCalls != 1 || started != 1 || len(runner.calls) != 2 {
		t.Fatalf("identity calls=%d process starts=%d commands=%+v", identityCalls, started, runner.calls)
	}
	if !strings.HasSuffix(runner.calls[0].binary, "ssh-keygen") || !reflect.DeepEqual(runner.calls[0].args,
		[]string{"-q", "-t", "ed25519", "-N", "", "-f", starter.path(workspaceSSHPrivateKey)}) {
		t.Fatalf("host-key generation call = %+v", runner.calls[0])
	}
	config, err := os.ReadFile(starter.path(workspaceSSHDConfigPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"ListenAddress 127.0.0.1", "Port 22", "HostKey " + starter.path(workspaceSSHEphemeralKey),
		"AuthorizedKeysFile " + starter.path(workspaceSSHAuthorized), "AuthenticationMethods publickey",
		"PasswordAuthentication no", "PermitRootLogin no", "AllowUsers hako-workspace",
		"AllowTcpForwarding local", "GatewayPorts no", "Subsystem sftp internal-sftp",
	} {
		if !strings.Contains(string(config), required) {
			t.Errorf("sshd config is missing %q:\n%s", required, config)
		}
	}
	privateInfo, err := os.Stat(starter.path(workspaceSSHPrivateKey))
	if err != nil || privateInfo.Mode().Perm() != 0600 {
		t.Fatalf("private host key mode=%v err=%v", privateInfo.Mode().Perm(), err)
	}
	if got := startArgs; !reflect.DeepEqual(got, []string{starter.path(workspaceSSHDPath), "-D", "-e", "-f", starter.path(workspaceSSHDConfigPath)}) {
		t.Fatalf("sshd process args = %v", got)
	}
	if err := runtimeProcess.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(starter.path(workspaceSSHEphemeralKey)); !os.IsNotExist(err) {
		t.Fatalf("ephemeral host key remains after Stop: %v", err)
	}

	retryProcess, err := starter.Start(context.Background(), workspaceSSHMountSpec())
	if err != nil {
		t.Fatalf("retry with existing stable host key: %v", err)
	}
	if err := retryProcess.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 || strings.HasSuffix(runner.calls[2].binary, "ssh-keygen") {
		t.Fatalf("host key was regenerated on retry: %+v", runner.calls)
	}
}

func TestWorkspaceSSHServerRejectsPartialKeysAndInvalidIdentity(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, *linuxWorkspaceSSHServerStarter)
		spec  WorkspaceMountSpec
	}{
		{name: "partial host key", spec: workspaceSSHMountSpec(), setup: func(t *testing.T, starter *linuxWorkspaceSSHServerStarter) {
			if err := os.MkdirAll(starter.path(workspaceSSHKeyDir), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(starter.path(workspaceSSHPrivateKey), []byte("private"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "invalid mount identity", spec: WorkspaceMountSpec{MountPath: "/tmp"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			starter := &linuxWorkspaceSSHServerStarter{root: t.TempDir(), runner: &sshServerCommandStub{},
				processStarter: func(string, ...string) (WorkspaceSSHServerProcess, error) { return &sshServerProcessStub{}, nil },
				writeFile:      os.WriteFile, mkdirAll: os.MkdirAll, stat: os.Lstat, ensureIdentity: func(uint32, uint32) error { return nil }}
			if test.setup != nil {
				test.setup(t, starter)
			}
			if _, err := starter.Start(context.Background(), test.spec); err == nil {
				t.Fatal("invalid SSH server state was accepted")
			}
			if len(starter.runner.(*sshServerCommandStub).calls) != 0 {
				t.Fatalf("invalid state invoked a command: %+v", starter.runner.(*sshServerCommandStub).calls)
			}
		})
	}
}

func TestWorkspaceSSHServerRejectsCanceledContextAndUnsafeKeyMode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	starter := &linuxWorkspaceSSHServerStarter{root: t.TempDir(), runner: &sshServerCommandStub{},
		processStarter: func(string, ...string) (WorkspaceSSHServerProcess, error) { return &sshServerProcessStub{}, nil },
		writeFile:      os.WriteFile, mkdirAll: os.MkdirAll, stat: os.Lstat, ensureIdentity: func(uint32, uint32) error { return nil }}
	if _, err := starter.Start(ctx, workspaceSSHMountSpec()); err == nil {
		t.Fatal("canceled startup context was accepted")
	}

	root := t.TempDir()
	unsafe := &linuxWorkspaceSSHServerStarter{root: root, runner: &sshServerCommandStub{},
		processStarter: func(string, ...string) (WorkspaceSSHServerProcess, error) { return &sshServerProcessStub{}, nil },
		writeFile:      os.WriteFile, mkdirAll: os.MkdirAll, stat: os.Lstat, ensureIdentity: func(uint32, uint32) error { return nil }}
	if err := os.MkdirAll(unsafe.path(workspaceSSHKeyDir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unsafe.path(workspaceSSHPrivateKey), []byte("private"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unsafe.path(workspaceSSHPublicKey), []byte("public"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := unsafe.Start(context.Background(), workspaceSSHMountSpec()); err == nil {
		t.Fatal("group-readable host private key was accepted")
	}
}

func TestWorkspaceSSHDConfigDoesNotPermitPasswordOrExternalBind(t *testing.T) {
	config := workspaceSSHDConfig("/workspace/.hako/ssh/host", "/workspace/.ssh/authorized_keys", 145678, 145678)
	if strings.Contains(config, "ListenAddress 0.0.0.0") || strings.Contains(config, "PasswordAuthentication yes") ||
		strings.Contains(config, "PermitRootLogin yes") {
		t.Fatalf("unsafe sshd configuration: %s", config)
	}
	if workspaceSSHDPathsAreFixed() != true {
		t.Fatal("compiled Workspace SSH paths changed unexpectedly")
	}
	if _, err := filepath.Abs("."); err != nil {
		t.Fatal(err)
	}
}
