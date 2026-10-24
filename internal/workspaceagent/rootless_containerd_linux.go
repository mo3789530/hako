//go:build linux

package workspaceagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	WorkspaceServicesEnabledEnvironment = "HAKO_WORKSPACE_SERVICES_ENABLED"
	RootlessContainerdBinaryPath        = "/usr/local/bin/containerd-rootless.sh"
	rootlessSubIDStart                  = uint32(20_000_000)
	rootlessSubIDCount                  = uint32(65_536)
)

type RootlessContainerdConfig struct {
	BinaryPath       string
	RuntimeDirectory string
	DataDirectory    string
	WorkspaceRoot    string
	SocketPath       string
	UID              uint32
	GID              uint32
	StartupTimeout   time.Duration
}

// RootlessContainerd owns the RootlessKit/containerd process group for one
// Workspace MicroVM. The root init supervisor starts it as the Workspace UID;
// it never receives credentials in its environment.
type RootlessContainerd struct {
	process *rootlessContainerdProcess
	config  RootlessContainerdConfig
}

func (d *RootlessContainerd) SocketPath() string {
	if d == nil {
		return ""
	}
	return d.config.SocketPath
}

type rootlessContainerdProcess struct {
	command *exec.Cmd
	done    chan error
}

func StartLinuxRootlessContainerd(ctx context.Context, config RootlessContainerdConfig) (*RootlessContainerd, error) {
	if ctx == nil {
		return nil, errors.New("rootless containerd context is required")
	}
	if config.StartupTimeout == 0 {
		config.StartupTimeout = 15 * time.Second
	}
	if config.BinaryPath == "" {
		config.BinaryPath = RootlessContainerdBinaryPath
	}
	if config.RuntimeDirectory == "" {
		config.RuntimeDirectory = "/run/user/" + strconv.FormatUint(uint64(config.UID), 10)
	}
	if config.SocketPath == "" {
		config.SocketPath = filepath.Join(config.RuntimeDirectory, "containerd", "containerd.sock")
	}
	if config.DataDirectory == "" {
		config.DataDirectory = filepath.Join("/tmp", "hako-containerd-"+strconv.FormatUint(uint64(config.UID), 10))
	}
	if config.WorkspaceRoot == "" {
		config.WorkspaceRoot = "/workspace"
	}
	rootOwnedStart := os.Geteuid() == 0
	if !validRootlessContainerdConfig(config) || (!rootOwnedStart && (os.Geteuid() != int(config.UID) || os.Getegid() != int(config.GID))) {
		return nil, errors.New("rootless containerd identity or image configuration is invalid")
	}
	if err := validateWorkspaceRuntimeTmpfs(config.RuntimeDirectory, config.UID, config.GID); err != nil {
		return nil, errors.New("rootless containerd runtime directory is not the private Workspace tmpfs")
	}
	if err := validateRootlessOwnedDirectory(config.WorkspaceRoot, config.UID, config.GID); err != nil {
		return nil, errors.New("rootless containerd Workspace directory is not owned by the Guest Agent")
	}
	if err := prepareRootlessDataDirectory(config.DataDirectory, config.UID, config.GID); err != nil {
		return nil, errors.New("prepare ephemeral rootless containerd data directory")
	}
	if err := removeStaleRootlessSocket(config.SocketPath, config.RuntimeDirectory, config.UID, config.GID); err != nil {
		return nil, errors.New("remove stale rootless containerd socket")
	}

	command := exec.Command(config.BinaryPath)
	command.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + config.WorkspaceRoot,
		"XDG_RUNTIME_DIR=" + config.RuntimeDirectory,
		"XDG_DATA_HOME=" + config.DataDirectory,
		"XDG_CONFIG_HOME=" + filepath.Join(config.DataDirectory, "config"),
		"CONTAINERD_ROOTLESS_ROOTLESSKIT_STATE_DIR=" + filepath.Join(config.RuntimeDirectory, "containerd-rootless"),
		"CONTAINERD_ROOTLESS_ROOTLESSKIT_NET=slirp4netns",
		"CONTAINERD_ROOTLESS_ROOTLESSKIT_IPV6=false",
		"CONTAINERD_SNAPSHOTTER=native",
	}
	command.Stdout, command.Stderr = io.Discard, io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if rootOwnedStart {
		command.SysProcAttr.Credential = &syscall.Credential{Uid: config.UID, Gid: config.GID, Groups: []uint32{}}
	}
	if err := command.Start(); err != nil {
		return nil, errors.New("start rootless containerd process")
	}
	process := &rootlessContainerdProcess{command: command, done: make(chan error, 1)}
	go func() { process.done <- command.Wait(); close(process.done) }()
	daemon := &RootlessContainerd{process: process, config: config}
	startupCtx, cancel := context.WithTimeout(ctx, config.StartupTimeout)
	defer cancel()
	if err := waitRootlessContainerdSocket(startupCtx, process, config.SocketPath, config.UID, config.GID); err != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = daemon.Stop(stopCtx)
		stopCancel()
		return nil, errors.New("rootless containerd did not become ready")
	}
	return daemon, nil
}

func validRootlessContainerdConfig(config RootlessContainerdConfig) bool {
	return filepath.IsAbs(config.BinaryPath) && config.BinaryPath == filepath.Clean(config.BinaryPath) &&
		filepath.IsAbs(config.RuntimeDirectory) && config.RuntimeDirectory == filepath.Clean(config.RuntimeDirectory) &&
		filepath.IsAbs(config.SocketPath) && config.SocketPath == filepath.Clean(config.SocketPath) &&
		filepath.IsAbs(config.DataDirectory) && config.DataDirectory == filepath.Clean(config.DataDirectory) &&
		filepath.IsAbs(config.WorkspaceRoot) && config.WorkspaceRoot == "/workspace" &&
		config.RuntimeDirectory == "/run/user/"+strconv.FormatUint(uint64(config.UID), 10) &&
		config.SocketPath == filepath.Join(config.RuntimeDirectory, "containerd", "containerd.sock") &&
		strings.HasPrefix(config.DataDirectory, "/tmp/hako-containerd-") &&
		config.DataDirectory == filepath.Join("/tmp", "hako-containerd-"+strconv.FormatUint(uint64(config.UID), 10)) &&
		config.UID >= 100000 && config.GID == config.UID && config.StartupTimeout > 0 && config.StartupTimeout <= time.Minute
}

func validateRootlessOwnedDirectory(path string, uid, gid uint32) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return errors.New("directory is missing, unsafe, or writable by another user")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uid || stat.Gid != gid || info.Mode().Perm() != 0700 {
		return errors.New("directory ownership does not match Guest Agent")
	}
	return nil
}

func prepareRootlessDataDirectory(path string, uid, gid uint32) error {
	if err := os.Mkdir(path, 0700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := validateRootlessOwnedDirectory(path, uid, gid); err != nil {
			return err
		}
	} else if os.Geteuid() == 0 {
		if err := os.Chown(path, int(uid), int(gid)); err != nil {
			return err
		}
	}
	return validateRootlessOwnedDirectory(path, uid, gid)
}

func removeStaleRootlessSocket(socketPath, runtimeDirectory string, uid, gid uint32) error {
	if filepath.Dir(filepath.Dir(socketPath)) != runtimeDirectory {
		return errors.New("containerd socket escaped runtime directory")
	}
	info, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("inspect rootless containerd socket")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSocket == 0 || !ok || stat.Uid != uid || stat.Gid != gid || info.Mode().Perm()&0077 != 0 {
		return errors.New("refusing to remove a non-socket containerd path")
	}
	return os.Remove(socketPath)
}

func waitRootlessContainerdSocket(ctx context.Context, process *rootlessContainerdProcess, socketPath string, uid, gid uint32) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := validateRootlessContainerdSocket(socketPath, uid, gid); err == nil {
			connection, err := (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(ctx, "unix", socketPath)
			if err == nil {
				_ = connection.Close()
				return nil
			}
		}
		select {
		case <-process.done:
			return errors.New("rootless containerd exited before ready")
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func validateRootlessContainerdSocket(path string, uid, gid uint32) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("containerd socket is absent or exposed to other users")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uid || stat.Gid != gid {
		return errors.New("containerd socket owner does not match Guest Agent")
	}
	return nil
}

func (d *RootlessContainerd) Stop(ctx context.Context) error {
	if d == nil || d.process == nil || ctx == nil {
		return errors.New("rootless containerd shutdown context is required")
	}
	process := d.process
	if process.command.Process == nil {
		return nil
	}
	_ = syscall.Kill(-process.command.Process.Pid, syscall.SIGTERM)
	select {
	case <-process.done:
	case <-ctx.Done():
		_ = syscall.Kill(-process.command.Process.Pid, syscall.SIGKILL)
		select {
		case <-process.done:
		case <-time.After(time.Second):
		}
	}
	err := removeStaleRootlessSocket(d.config.SocketPath, d.config.RuntimeDirectory, d.config.UID, d.config.GID)
	if err != nil {
		return fmt.Errorf("clean rootless containerd socket: %w", err)
	}
	return ctx.Err()
}

// ensureWorkspaceRootlessIdentity configures the per-MicroVM account and a
// non-overlapping subordinate-ID range before dropping privileges. The range
// begins above the stable EFS UID space and is shared only within this one VM.
func ensureWorkspaceRootlessIdentity(uid, gid uint32) error {
	if os.Geteuid() != 0 || uid < 100000 || gid != uid {
		return errors.New("rootless account setup requires root and a valid Workspace identity")
	}
	return ensureWorkspaceRootlessIdentityAt("/etc", uid, gid)
}

func ensureWorkspaceRootlessIdentityAt(root string, uid, gid uint32) error {
	if uid < 100000 || gid != uid || !filepath.IsAbs(root) {
		return errors.New("rootless account identity is invalid")
	}
	userLine := fmt.Sprintf("hako-workspace:%s:%d:%d:Hako Workspace:/workspace:/sbin/nologin", "x", uid, gid)
	groupLine := fmt.Sprintf("hako-workspace:x:%d:", gid)
	subIDLine := fmt.Sprintf("hako-workspace:%d:%d", rootlessSubIDStart, rootlessSubIDCount)
	if err := ensureIdentityLine(filepath.Join(root, "passwd"), userLine, 0, 2); err != nil {
		return err
	}
	if err := ensureIdentityLine(filepath.Join(root, "group"), groupLine, 0, 2); err != nil {
		return err
	}
	if err := ensureSubIDLine(filepath.Join(root, "subuid"), subIDLine); err != nil {
		return err
	}
	return ensureSubIDLine(filepath.Join(root, "subgid"), subIDLine)
}

func ensureIdentityLine(path, expected string, nameField, idField int) error {
	fields := strings.Split(expected, ":")
	if len(fields) <= idField {
		return errors.New("invalid rootless account record")
	}
	data, mode, ownerUID, ownerGID, err := readRootlessConfigFile(path, true)
	if err != nil {
		return err
	}
	found := false
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) <= idField {
			return errors.New("malformed rootless account file")
		}
		if parts[nameField] == fields[nameField] || parts[idField] == fields[idField] {
			if line == expected {
				found = true
				continue
			}
			return errors.New("Workspace UID/GID conflicts with an existing system account")
		}
	}
	if found {
		return nil
	}
	return appendRootlessConfigLine(path, data, expected, mode, ownerUID, ownerGID)
}

func ensureSubIDLine(path, expected string) error {
	data, mode, ownerUID, ownerGID, err := readRootlessConfigFile(path, true)
	if err != nil {
		return err
	}
	want := strings.Split(expected, ":")
	found := false
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) != 3 {
			return errors.New("malformed subordinate ID file")
		}
		if line == expected {
			found = true
			continue
		}
		start, startErr := strconv.ParseUint(parts[1], 10, 32)
		count, countErr := strconv.ParseUint(parts[2], 10, 32)
		wantStart, _ := strconv.ParseUint(want[1], 10, 32)
		wantCount, _ := strconv.ParseUint(want[2], 10, 32)
		if startErr != nil || countErr != nil || count == 0 || start+count > 1<<32 ||
			(start < wantStart+wantCount && wantStart < start+count) {
			return errors.New("subordinate ID range conflicts with an existing assignment")
		}
		if parts[0] == want[0] {
			return errors.New("Workspace subordinate ID assignment changed")
		}
	}
	if found {
		return nil
	}
	return appendRootlessConfigLine(path, data, expected, mode, ownerUID, ownerGID)
}

func readRootlessConfigFile(path string, missingAllowed bool) ([]byte, os.FileMode, int, int, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && missingAllowed {
		return nil, 0644, 0, 0, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return nil, 0, 0, 0, errors.New("rootless account configuration file is invalid")
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 1<<20 {
		return nil, 0, 0, 0, errors.New("read rootless account configuration")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, 0, 0, 0, errors.New("rootless account configuration owner is invalid")
	}
	return data, info.Mode().Perm(), int(stat.Uid), int(stat.Gid), nil
}

func appendRootlessConfigLine(path string, existing []byte, line string, mode os.FileMode, ownerUID, ownerGID int) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".hako-rootless-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if ownerUID != 0 || ownerGID != 0 {
		if err := file.Chown(ownerUID, ownerGID); err != nil {
			_ = file.Close()
			return err
		}
	}
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		existing = append(existing, '\n')
	}
	if _, err := file.Write(append(existing, []byte(line+"\n")...)); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

var _ interface{ Stop(context.Context) error } = (*RootlessContainerd)(nil)
