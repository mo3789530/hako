//go:build linux

package workspaceagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	workspaceEFSMarkerPath = "/run/hako/workspace-efs-mount.json"
	workspaceEFSMountPath  = "/workspace"
	workspaceMountBinary   = "/bin/mount"
	workspaceUmountBinary  = "/bin/umount"
	workspaceMountInfoPath = "/proc/self/mountinfo"
	// Leave margin below Lambda MicroVM's maximum 60-second run-hook deadline.
	workspaceEFSMountTimeout      = 50 * time.Second
	workspaceEFSMountPollInterval = 100 * time.Millisecond
)

type mountCommandRunner interface {
	Run(context.Context, string, ...string) error
}

type execMountCommandRunner struct{}

func (execMountCommandRunner) Run(ctx context.Context, binary string, args ...string) error {
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = []string{"PATH=/sbin:/usr/sbin:/bin:/usr/bin", "HOME=/root", "TMPDIR=/tmp"}
	command.Stdout, command.Stderr = io.Discard, io.Discard
	return command.Run()
}

// LinuxEFSMountExecutor mounts only the pre-authorized EFS Access Point at
// /workspace. The helper is invoked without a shell and with a minimal
// environment. A root-owned /run marker makes retries idempotent while a
// pre-existing unowned mount fails closed.
type LinuxEFSMountExecutor struct {
	mu            sync.Mutex
	runner        mountCommandRunner
	mountBinary   string
	umountBinary  string
	mountInfoPath string
	markerPath    string
	workspacePath string
	mountTimeout  time.Duration
	isRoot        func() bool
	makeDirectory func(string, os.FileMode) error
}

func NewLinuxEFSMountExecutor() *LinuxEFSMountExecutor {
	return &LinuxEFSMountExecutor{runner: execMountCommandRunner{}, mountBinary: workspaceMountBinary,
		umountBinary:  workspaceUmountBinary,
		mountInfoPath: workspaceMountInfoPath, markerPath: workspaceEFSMarkerPath, workspacePath: workspaceEFSMountPath,
		mountTimeout: workspaceEFSMountTimeout,
		isRoot:       func() bool { return os.Geteuid() == 0 }, makeDirectory: os.MkdirAll}
}

// NewLinuxWorkspaceMountLifecycle returns both the lifecycle handler and the
// executor so the root init process can perform ownership-checked unmount on
// graceful shutdown using the same runtime configuration.
func NewLinuxWorkspaceMountLifecycle(next http.Handler) (http.Handler, *LinuxEFSMountExecutor, error) {
	if os.Geteuid() != 0 {
		return nil, nil, errors.New("Workspace mount lifecycle must run in the root guest init layer")
	}
	if info, err := os.Stat("/sbin/mount.efs"); err != nil || info.IsDir() || info.Mode().Perm()&0111 == 0 {
		return nil, nil, errors.New("Amazon EFS mount helper is not installed in the Workspace image")
	}
	executor := NewLinuxEFSMountExecutor()
	handler, err := NewWorkspaceMountLifecycleHandler(next, executor)
	if err != nil {
		return nil, nil, err
	}
	return handler, executor, nil
}

// NewLinuxWorkspaceMountLifecycleHandler wires the strict Workspace /run hook
// to the Linux EFS mount helper. The process must run as root in the guest
// init layer; the interactive Agent should run unprivileged after the mount.
func NewLinuxWorkspaceMountLifecycleHandler(next http.Handler) (http.Handler, error) {
	handler, _, err := NewLinuxWorkspaceMountLifecycle(next)
	return handler, err
}

func (m *LinuxEFSMountExecutor) MountAndVerify(ctx context.Context, spec WorkspaceMountSpec) error {
	if m == nil || ctx == nil || ctx.Err() != nil || !validWorkspaceMountSpec(spec) || m.runner == nil ||
		m.mountBinary == "" || m.mountInfoPath == "" || m.markerPath == "" || m.workspacePath != workspaceEFSMountPath ||
		m.mountTimeout <= 0 || m.isRoot == nil || !m.isRoot() || m.makeDirectory == nil {
		return errors.New("Workspace EFS mount configuration or privilege is invalid")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	marker, markerExists, err := readWorkspaceMountMarker(m.markerPath)
	if err != nil {
		return err
	}
	if markerExists && marker != spec {
		return errors.New("Workspace EFS mount marker belongs to a different Access Point")
	}
	mounted, err := workspacePathMounted(m.mountInfoPath, m.workspacePath)
	if err != nil {
		return err
	}
	if mounted {
		if markerExists {
			return nil
		}
		return errors.New("refusing to adopt a pre-existing unowned Workspace mount")
	}
	if !markerExists {
		if err := writeWorkspaceMountMarker(m.markerPath, spec); err != nil {
			return err
		}
	}
	if err := m.makeDirectory(m.workspacePath, 0755); err != nil {
		return errors.New("create Workspace mount directory")
	}
	mountCtx, cancel := context.WithTimeout(ctx, m.mountTimeout)
	defer cancel()
	if err := m.runner.Run(mountCtx, m.mountBinary, "-t", "efs", "-o", mountOptions(spec), spec.FileSystemID+":/", m.workspacePath); err != nil {
		return errors.New("mount Workspace EFS Access Point")
	}
	ticker := time.NewTicker(workspaceEFSMountPollInterval)
	defer ticker.Stop()
	for {
		mounted, err = workspacePathMounted(m.mountInfoPath, m.workspacePath)
		if err != nil {
			return err
		}
		if mounted {
			return nil
		}
		select {
		case <-mountCtx.Done():
			return errors.New("Workspace EFS mount was not healthy before the mount timeout")
		case <-ticker.C:
		}
	}
}

// Unmount removes only a mount whose exact Workspace identity was recorded by
// this executor. It is safe to retry after the kernel unmounted the filesystem
// but before marker removal completed.
func (m *LinuxEFSMountExecutor) Unmount(ctx context.Context) error {
	if m == nil || ctx == nil || ctx.Err() != nil || m.runner == nil || m.umountBinary == "" ||
		m.mountInfoPath == "" || m.markerPath == "" || m.workspacePath != workspaceEFSMountPath || m.isRoot == nil || !m.isRoot() {
		return errors.New("Workspace EFS unmount configuration or privilege is invalid")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, found, err := readWorkspaceMountMarker(m.markerPath); err != nil {
		return err
	} else if !found {
		return nil
	}
	mounted, err := workspacePathMounted(m.mountInfoPath, m.workspacePath)
	if err != nil {
		return err
	}
	if mounted {
		if err := m.runner.Run(ctx, m.umountBinary, m.workspacePath); err != nil {
			return errors.New("unmount Workspace EFS")
		}
		mounted, err = workspacePathMounted(m.mountInfoPath, m.workspacePath)
		if err != nil || mounted {
			return errors.New("Workspace EFS remains mounted after unmount")
		}
	}
	if err := os.Remove(m.markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("remove Workspace EFS mount marker")
	}
	return nil
}

func readWorkspaceMountMarker(path string) (WorkspaceMountSpec, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return WorkspaceMountSpec{}, false, nil
	}
	if err != nil || len(data) == 0 || len(data) > 4<<10 {
		return WorkspaceMountSpec{}, false, errors.New("read Workspace EFS mount marker")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var spec WorkspaceMountSpec
	if decoder.Decode(&spec) != nil || !validWorkspaceMountSpec(spec) {
		return WorkspaceMountSpec{}, false, errors.New("Workspace EFS mount marker is invalid")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return WorkspaceMountSpec{}, false, errors.New("Workspace EFS mount marker contains trailing data")
	}
	return spec, true, nil
}

func writeWorkspaceMountMarker(path string, spec WorkspaceMountSpec) error {
	if !validWorkspaceMountSpec(spec) {
		return errors.New("Workspace EFS mount marker specification is invalid")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("create Workspace EFS marker directory")
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return errors.New("encode Workspace EFS mount marker")
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".workspace-efs-mount-*")
	if err != nil {
		return errors.New("create temporary Workspace EFS mount marker")
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return errors.New("protect Workspace EFS mount marker")
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return errors.New("write Workspace EFS mount marker")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return errors.New("sync Workspace EFS mount marker")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("close Workspace EFS mount marker")
	}
	if err := os.Rename(name, path); err != nil {
		return errors.New("publish Workspace EFS mount marker")
	}
	return nil
}

func workspacePathMounted(mountInfoPath, mountPath string) (bool, error) {
	file, err := os.Open(mountInfoPath)
	if err != nil {
		return false, errors.New("read Workspace mount table")
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		separator := -1
		for index, field := range fields {
			if field == "-" {
				separator = index
				break
			}
		}
		if separator < 6 || len(fields) <= separator+2 || unescapeMountInfo(fields[4]) != mountPath {
			continue
		}
		filesystemType := fields[separator+1]
		if filesystemType != "nfs" && filesystemType != "nfs4" {
			return false, errors.New("Workspace mount path is occupied by a non-EFS filesystem")
		}
		return true, nil
	}
	if err := scanner.Err(); err != nil {
		return false, errors.New("scan Workspace mount table")
	}
	return false, nil
}

func unescapeMountInfo(value string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(value)
}

func mountOptions(spec WorkspaceMountSpec) string {
	return fmt.Sprintf("tls,iam,accesspoint=%s,region=%s", spec.AccessPointID, spec.Region)
}
