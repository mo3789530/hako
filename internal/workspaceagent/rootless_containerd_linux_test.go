//go:build linux

package workspaceagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnsureWorkspaceRootlessIdentityWritesStableAccountAndDisjointSubIDs(t *testing.T) {
	root := t.TempDir()
	for name, contents := range map[string]string{
		"passwd": "root:x:0:0:root:/root:/bin/bash\n",
		"group":  "root:x:0:\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := ensureWorkspaceRootlessIdentityAt(root, 145678, 145678); err != nil {
		t.Fatalf("prepare rootless account: %v", err)
	}
	if err := ensureWorkspaceRootlessIdentityAt(root, 145678, 145678); err != nil {
		t.Fatalf("repeat rootless account setup: %v", err)
	}
	for name, want := range map[string]string{
		"passwd": "hako-workspace:x:145678:145678:Hako Workspace:/workspace:/bin/bash",
		"group":  "hako-workspace:x:145678:",
		"subuid": "hako-workspace:20000000:65536",
		"subgid": "hako-workspace:20000000:65536",
	} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || strings.Count(string(data), want) != 1 {
			t.Fatalf("%s configuration = %q; want one %q, err=%v", name, data, want, err)
		}
	}
}

func TestEnsureWorkspaceRootlessIdentityRejectsConflictingAccountsAndSubIDRanges(t *testing.T) {
	for name, contents := range map[string]string{
		"subuid": "another-user:20000000:65536\n",
	} {
		root := t.TempDir()
		for filename, value := range map[string]string{
			"passwd": "root:x:0:0:root:/root:/bin/bash\n",
			"group":  "root:x:0:\n",
			name:     contents,
		} {
			if err := os.WriteFile(filepath.Join(root, filename), []byte(value), 0644); err != nil {
				t.Fatal(err)
			}
		}
		if err := ensureWorkspaceRootlessIdentityAt(root, 145678, 145678); err == nil {
			t.Fatal("overlapping subordinate UID range was accepted")
		}
	}
	root := t.TempDir()
	for filename, contents := range map[string]string{
		"passwd": "root:x:0:0:root:/root:/bin/bash\nhako-workspace:x:145678:145678:Hako Workspace:/workspace:/bin/bash\n",
		"group":  "root:x:0:\nforeign:x:145678:\n",
	} {
		if err := os.WriteFile(filepath.Join(root, filename), []byte(contents), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := ensureWorkspaceRootlessIdentityAt(root, 145678, 145678); err == nil {
		t.Fatal("conflicting Workspace group mapping was accepted")
	}
}

func TestEnsureWorkspaceRootlessIdentityRejectsSymlinkedAccountFiles(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(outside, []byte("root:x:0:0:root:/root:/bin/bash\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "passwd")); err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkspaceRootlessIdentityAt(root, 145678, 145678); err == nil {
		t.Fatal("symlinked passwd file was accepted")
	}
	data, err := os.ReadFile(outside)
	if err != nil || strings.Contains(string(data), "hako-workspace") {
		t.Fatalf("symlink target was changed: %q, %v", data, err)
	}
}

func TestRootlessContainerdConfigurationPinsWorkspacePathsAndBounds(t *testing.T) {
	config := RootlessContainerdConfig{BinaryPath: RootlessContainerdBinaryPath, RuntimeDirectory: "/run/user/145678",
		DataDirectory: "/tmp/hako-containerd-145678", WorkspaceRoot: "/workspace",
		SocketPath: "/run/user/145678/containerd/containerd.sock", UID: 145678, GID: 145678, StartupTimeout: 15 * time.Second}
	if !validRootlessContainerdConfig(config) {
		t.Fatal("valid rootless containerd configuration rejected")
	}
	for _, mutate := range []func(*RootlessContainerdConfig){
		func(c *RootlessContainerdConfig) { c.SocketPath = "/run/containerd/containerd.sock" },
		func(c *RootlessContainerdConfig) { c.DataDirectory = "/workspace/.local/share/containerd" },
		func(c *RootlessContainerdConfig) { c.WorkspaceRoot = "/" },
		func(c *RootlessContainerdConfig) { c.StartupTimeout = 2 * time.Minute },
		func(c *RootlessContainerdConfig) { c.GID++ },
	} {
		invalid := config
		mutate(&invalid)
		if validRootlessContainerdConfig(invalid) {
			t.Errorf("accepted unsafe rootless containerd configuration: %+v", invalid)
		}
	}
}
