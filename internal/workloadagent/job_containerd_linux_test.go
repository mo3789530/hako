//go:build linux

package workloadagent

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestJobRootlessContainerdConfigIsImagePinned(t *testing.T) {
	config := JobRootlessContainerdConfig{BinaryPath: "/usr/local/bin/containerd-rootless.sh",
		RuntimeDir: jobRuntimeDirectory, SocketPath: jobRuntimeSocket, DataDir: jobRuntimeData,
		HomeDir: jobRuntimeHome, EnvFileDir: jobRuntimeEnvFiles, UID: jobRuntimeUID, GID: jobRuntimeUID,
		StartupTimeout: 20 * time.Second}
	if !validJobRootlessContainerdConfig(config) {
		t.Fatal("fixed image runtime configuration was rejected")
	}
	for _, mutate := range []func(*JobRootlessContainerdConfig){
		func(c *JobRootlessContainerdConfig) { c.SocketPath = "/tmp/containerd.sock" },
		func(c *JobRootlessContainerdConfig) { c.DataDir = "/workspace/source" },
		func(c *JobRootlessContainerdConfig) { c.EnvFileDir = "/tmp/secrets" },
		func(c *JobRootlessContainerdConfig) { c.UID = 65532 },
		func(c *JobRootlessContainerdConfig) { c.GID++ },
		func(c *JobRootlessContainerdConfig) { c.StartupTimeout = 2 * time.Minute },
	} {
		invalid := config
		mutate(&invalid)
		if validJobRootlessContainerdConfig(invalid) {
			t.Errorf("accepted unsafe rootless runtime config: %+v", invalid)
		}
	}
}

func TestJobRootlessSubIDMappingRejectsOverlapDuplicateAndMalformedRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subuid")
	want := "hako-job-runtime:20000000:65536\nother:30000000:65536\n"
	if err := os.WriteFile(path, []byte(want), 0600); err != nil {
		t.Fatal(err)
	}
	if !jobSubIDConfigured(path, "hako-job-runtime", jobRuntimeSubIDBase, jobRuntimeSubIDSize) {
		t.Fatal("valid non-overlapping subordinate ID ranges rejected")
	}
	for _, contents := range []string{
		"hako-job-runtime:20000000:65536\nother:20060000:65536\n",
		"hako-job-runtime:20000000:65536\nhako-job-runtime:20000000:65536\n",
		"hako-job-runtime:20000000:bad\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if jobSubIDConfigured(path, "hako-job-runtime", jobRuntimeSubIDBase, jobRuntimeSubIDSize) {
			t.Errorf("accepted invalid subordinate ID configuration %q", contents)
		}
	}
}

func TestJobRootlessAccountRequiresExactNumericIdentity(t *testing.T) {
	root := t.TempDir()
	passwd := filepath.Join(root, "passwd")
	group := filepath.Join(root, "group")
	if err := os.WriteFile(passwd, []byte("root:x:0:0:root:/root:/bin/bash\nhako-job-runtime:x:100000:100000:Hako Job Runtime:/tmp/hako-job-runtime:/sbin/nologin\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(group, []byte("root:x:0:\nhako-job-runtime:x:100000:\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if !jobRuntimeAccountConfigured(passwd, "hako-job-runtime", 100000, 100000) || !jobRuntimeGroupConfigured(group, "hako-job-runtime", 100000) {
		t.Fatal("expected rootless account mapping was rejected")
	}
	if jobRuntimeAccountConfigured(passwd, "hako-job-runtime", 100001, 100000) || jobRuntimeGroupConfigured(group, "hako-job-runtime", 100001) {
		t.Fatal("unexpected numeric runtime identity was accepted")
	}
}

func TestJobContainerdSocketRejectsPublicPermissionsAndForeignOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "containerd.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	owner := stat.Sys().(*syscall.Stat_t)
	if err := validateJobContainerdSocket(path, owner.Uid, owner.Gid); err != nil {
		t.Fatalf("private owned socket rejected: %v", err)
	}
	if validateJobContainerdSocket(path, owner.Uid+1, owner.Gid) == nil {
		t.Fatal("foreign socket owner was accepted")
	}
	if err := os.Chmod(path, 0660); err != nil {
		t.Fatal(err)
	}
	if validateJobContainerdSocket(path, owner.Uid, owner.Gid) == nil {
		t.Fatal("group-readable runtime socket was accepted")
	}
}

func TestEnsureJobPrivateDirectoryRejectsUnsafePaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := ensureJobPrivateDirectory(path, uint32(os.Getuid()), uint32(os.Getgid())); err != nil {
		t.Fatalf("private directory rejected: %v", err)
	}
	if ensureJobPrivateDirectory(filepath.Join(path, "missing"), uint32(os.Getuid()+1), uint32(os.Getgid()+1)) == nil {
		t.Fatal("directory with foreign ownership was accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if ensureJobPrivateDirectory(link, uint32(os.Getuid()), uint32(os.Getgid())) == nil {
		t.Fatal("symlinked private directory was accepted")
	}
}
