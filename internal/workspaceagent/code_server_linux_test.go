//go:build linux

package workspaceagent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceCodeServerUsesFixedLoopbackAndWorkspaceIdentity(t *testing.T) {
	root := t.TempDir()
	argumentsPath := filepath.Join(root, "args")
	binary := filepath.Join(root, "code-server-test")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argumentsPath + "\ntrap 'exit 0' TERM INT\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	if uid == 0 {
		uid, gid = 65532, 65532
		if err := os.Chown(root, int(uid), int(gid)); err != nil {
			t.Fatal(err)
		}
	}
	process, err := startWorkspaceCodeServer(binary, root, uid, gid, func(_ *exec.Cmd, _ <-chan struct{}, _ time.Duration) error { return nil })
	if err != nil {
		t.Fatalf("startWorkspaceCodeServer: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = process.Stop(ctx)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(argumentsPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("code-server did not record its fixed launch arguments")
		}
		time.Sleep(5 * time.Millisecond)
	}
	contents, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(contents)), "\n")
	want := []string{"--bind-addr", "127.0.0.1:8081", "--auth", "none", "--disable-telemetry",
		"--user-data-dir", filepath.Join(root, ".hako", "code-server", "data"),
		"--extensions-dir", filepath.Join(root, ".hako", "code-server", "extensions"), root}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("code-server arguments=%q want=%q", got, want)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := process.Stop(ctx); err != nil {
		t.Fatalf("stop code-server process group: %v", err)
	}
	select {
	case <-process.done:
	default:
		t.Fatal("code-server process remained alive after Stop")
	}
}

func TestWorkspaceCodeServerRejectsInvalidImageConfiguration(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "code-server")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := startWorkspaceCodeServer("relative", root, 65532, 65532, func(*exec.Cmd, <-chan struct{}, time.Duration) error { return nil }); err == nil {
		t.Fatal("accepted non-absolute code-server image path")
	}
	if _, err := startWorkspaceCodeServer(binary, root+"/../escape", 65532, 65532, func(*exec.Cmd, <-chan struct{}, time.Duration) error { return nil }); err == nil {
		t.Fatal("accepted non-canonical workspace root")
	}
	if _, err := startWorkspaceCodeServer(binary, root, 0, 0, func(*exec.Cmd, <-chan struct{}, time.Duration) error { return nil }); err == nil {
		t.Fatal("accepted privileged Workspace identity")
	}
}
