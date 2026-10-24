//go:build linux

package workloadagent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	jobRuntimeUID       = uint32(100000)
	jobRuntimeSubIDBase = uint32(20000000)
	jobRuntimeSubIDSize = uint32(65536)
	jobRuntimeSocket    = "/run/user/100000/containerd/containerd.sock"
	jobRuntimeDirectory = "/run/user/100000"
	jobRuntimeData      = "/tmp/hako-job-containerd-100000"
	jobRuntimeHome      = "/tmp/hako-job-runtime-home-100000"
	jobRuntimeEnvFiles  = "/run/hako-job-services"

	JobContainerdSocketPath = jobRuntimeSocket
	JobContainerdRuntimeDir = jobRuntimeDirectory
	JobContainerdEnvFileDir = jobRuntimeEnvFiles
)

type JobRootlessContainerdConfig struct {
	BinaryPath     string
	RuntimeDir     string
	SocketPath     string
	DataDir        string
	HomeDir        string
	EnvFileDir     string
	UID            uint32
	GID            uint32
	StartupTimeout time.Duration
}

// JobRootlessContainerd owns one MicroVM-local rootless containerd daemon.
// Its Unix socket is owned by a dedicated identity that repository processes
// do not use. The trusted root hako-agent is the only client of nerdctl.
type JobRootlessContainerd struct {
	config  JobRootlessContainerdConfig
	command *exec.Cmd
	done    chan error
}

func StartLinuxJobRootlessContainerd(ctx context.Context, config JobRootlessContainerdConfig) (*JobRootlessContainerd, error) {
	if ctx == nil || os.Geteuid() != 0 {
		return nil, errors.New("Job containerd startup requires the trusted root Agent")
	}
	if config.BinaryPath == "" {
		config.BinaryPath = "/usr/local/bin/containerd-rootless.sh"
	}
	if config.RuntimeDir == "" {
		config.RuntimeDir = jobRuntimeDirectory
	}
	if config.SocketPath == "" {
		config.SocketPath = jobRuntimeSocket
	}
	if config.DataDir == "" {
		config.DataDir = jobRuntimeData
	}
	if config.HomeDir == "" {
		config.HomeDir = jobRuntimeHome
	}
	if config.EnvFileDir == "" {
		config.EnvFileDir = jobRuntimeEnvFiles
	}
	if config.UID == 0 {
		config.UID = jobRuntimeUID
	}
	if config.GID == 0 {
		config.GID = config.UID
	}
	if config.StartupTimeout == 0 {
		config.StartupTimeout = 20 * time.Second
	}
	if !validJobRootlessContainerdConfig(config) || validateJobRootlessImage(config) != nil ||
		prepareJobRootlessDirectories(config) != nil || removeStaleJobContainerdSocket(config.SocketPath, config.UID, config.GID) != nil {
		return nil, errors.New("Job rootless containerd image or paths are not ready")
	}

	command := exec.Command(config.BinaryPath)
	command.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + config.HomeDir,
		"XDG_RUNTIME_DIR=" + config.RuntimeDir,
		"XDG_DATA_HOME=" + config.DataDir,
		"XDG_CONFIG_HOME=" + filepath.Join(config.DataDir, "config"),
		"CONTAINERD_ROOTLESS_ROOTLESSKIT_STATE_DIR=" + filepath.Join(config.RuntimeDir, "containerd-rootless"),
		"CONTAINERD_ROOTLESS_ROOTLESSKIT_NET=slirp4netns",
		"CONTAINERD_ROOTLESS_ROOTLESSKIT_IPV6=false",
		"CONTAINERD_ROOTLESS_ROOTLESSKIT_DETACH_NETNS=false",
		"CONTAINERD_SNAPSHOTTER=native",
		"TMPDIR=/tmp",
	}
	command.Stdout, command.Stderr = nil, nil
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: &syscall.Credential{Uid: config.UID, Gid: config.GID, Groups: []uint32{}}}
	if err := command.Start(); err != nil {
		return nil, errors.New("start Job rootless containerd")
	}
	daemon := &JobRootlessContainerd{config: config, command: command, done: make(chan error, 1)}
	go func() { daemon.done <- command.Wait(); close(daemon.done) }()
	startupCtx, cancel := context.WithTimeout(ctx, config.StartupTimeout)
	defer cancel()
	if err := daemon.waitReady(startupCtx); err != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = daemon.Close(stopCtx)
		stopCancel()
		return nil, errors.New("Job rootless containerd did not become ready")
	}
	return daemon, nil
}

func validJobRootlessContainerdConfig(config JobRootlessContainerdConfig) bool {
	return config.BinaryPath == "/usr/local/bin/containerd-rootless.sh" &&
		config.RuntimeDir == jobRuntimeDirectory && config.SocketPath == jobRuntimeSocket &&
		config.DataDir == jobRuntimeData && config.HomeDir == jobRuntimeHome &&
		config.EnvFileDir == jobRuntimeEnvFiles && config.UID == jobRuntimeUID && config.GID == jobRuntimeUID &&
		config.StartupTimeout > 0 && config.StartupTimeout <= time.Minute
}

func validateJobRootlessImage(config JobRootlessContainerdConfig) error {
	for _, path := range []string{config.BinaryPath, "/usr/local/bin/nerdctl", "/usr/local/bin/containerd",
		"/usr/local/bin/containerd-shim-runc-v2", "/usr/local/bin/runc", "/usr/local/bin/rootlesskit", "/usr/local/bin/slirp4netns"} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return errors.New("required rootless runtime executable is missing")
		}
	}
	for _, path := range []string{"/usr/bin/newuidmap", "/usr/bin/newgidmap"} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSetuid == 0 || info.Mode().Perm()&0111 == 0 {
			return errors.New("rootless subordinate ID helper is unavailable")
		}
	}
	if !jobSubIDConfigured("/etc/subuid", "hako-job-runtime", jobRuntimeSubIDBase, jobRuntimeSubIDSize) ||
		!jobSubIDConfigured("/etc/subgid", "hako-job-runtime", jobRuntimeSubIDBase, jobRuntimeSubIDSize) {
		return errors.New("rootless subordinate ID mapping is missing or overlaps")
	}
	if !jobRuntimeAccountConfigured("/etc/passwd", "hako-job-runtime", config.UID, config.GID) ||
		!jobRuntimeGroupConfigured("/etc/group", "hako-job-runtime", config.GID) {
		return errors.New("dedicated rootless runtime account is missing or conflicts")
	}
	return nil
}

func jobRuntimeGroupConfigured(path, name string, gid uint32) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ":")
		if len(fields) < 4 || fields[0] != name {
			continue
		}
		actual, parseErr := strconv.ParseUint(fields[2], 10, 32)
		if parseErr != nil || uint32(actual) != gid || found {
			return false
		}
		found = true
	}
	return found
}

func jobRuntimeAccountConfigured(path, name string, id, secondaryID uint32) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ":")
		if len(fields) < 4 || fields[0] != name {
			continue
		}
		actualID, errID := strconv.ParseUint(fields[2], 10, 32)
		actualSecondary, errSecondary := strconv.ParseUint(fields[3], 10, 32)
		if errID != nil || errSecondary != nil || uint32(actualID) != id || uint32(actualSecondary) != secondaryID || found {
			return false
		}
		found = true
	}
	return found
}

func jobSubIDConfigured(path, name string, start, count uint32) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	want := fmt.Sprintf("%s:%d:%d", name, start, count)
	found := false
	end := uint64(start) + uint64(count)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ":")
		if len(fields) != 3 {
			if strings.TrimSpace(line) != "" {
				return false
			}
			continue
		}
		if strings.Join(fields, ":") == want {
			if found {
				return false
			}
			found = true
			continue
		}
		otherStart, errStart := strconv.ParseUint(fields[1], 10, 32)
		otherCount, errCount := strconv.ParseUint(fields[2], 10, 32)
		if errStart != nil || errCount != nil || otherCount == 0 {
			return false
		}
		otherEnd := otherStart + otherCount
		if otherEnd < otherStart || (otherStart < end && otherEnd > uint64(start)) {
			return false
		}
	}
	return found
}

func prepareJobRootlessDirectories(config JobRootlessContainerdConfig) error {
	var stat unix.Statfs_t
	if err := unix.Statfs("/run", &stat); err != nil || uint64(stat.Type) != uint64(unix.TMPFS_MAGIC) {
		return errors.New("Job runtime directory is not on tmpfs")
	}
	if err := ensureJobRuntimeParent(); err != nil {
		return err
	}
	for _, directory := range []struct {
		path string
		uid  uint32
		gid  uint32
	}{{config.RuntimeDir, config.UID, config.GID}, {config.EnvFileDir, 0, 0}, {config.DataDir, config.UID, config.GID},
		{config.HomeDir, config.UID, config.GID}} {
		if err := ensureJobPrivateDirectory(directory.path, directory.uid, directory.gid); err != nil {
			return err
		}
	}
	return nil
}

func ensureJobRuntimeParent() error {
	info, err := os.Lstat("/run/user")
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir("/run/user", 0755); err != nil {
			return err
		}
		info, err = os.Lstat("/run/user")
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return errors.New("unsafe Job runtime parent directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || stat.Gid != 0 {
		return errors.New("Job runtime parent is not root-owned")
	}
	return nil
}

func ensureJobPrivateDirectory(path string, uid, gid uint32) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return errors.New("invalid private directory path")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, 0700); err != nil {
			return err
		}
		if err := os.Chown(path, int(uid), int(gid)); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return errors.New("private directory is missing or unsafe")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uid || stat.Gid != gid {
		return errors.New("private directory owner mismatch")
	}
	return nil
}

func (d *JobRootlessContainerd) waitReady(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := validateJobContainerdSocket(d.config.SocketPath, d.config.UID, d.config.GID); err == nil {
			connection, err := (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(ctx, "unix", d.config.SocketPath)
			if err == nil {
				_ = connection.Close()
				return nil
			}
		}
		select {
		case <-d.done:
			return errors.New("Job rootless containerd exited before ready")
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func validateJobContainerdSocket(path string, uid, gid uint32) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("Job containerd socket is absent or exposed")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uid || stat.Gid != gid {
		return errors.New("Job containerd socket owner mismatch")
	}
	return nil
}

func removeStaleJobContainerdSocket(path string, uid, gid uint32) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || validateJobContainerdSocket(path, uid, gid) != nil {
		return errors.New("refusing to remove foreign Job runtime path")
	}
	return os.Remove(path)
}

func (d *JobRootlessContainerd) Close(ctx context.Context) error {
	if d == nil || d.command == nil || d.command.Process == nil || ctx == nil {
		return errors.New("Job containerd shutdown context is required")
	}
	_ = syscall.Kill(-d.command.Process.Pid, syscall.SIGTERM)
	select {
	case <-d.done:
	case <-ctx.Done():
		_ = syscall.Kill(-d.command.Process.Pid, syscall.SIGKILL)
		select {
		case <-d.done:
		case <-time.After(time.Second):
		}
	}
	if err := removeStaleJobContainerdSocket(d.config.SocketPath, d.config.UID, d.config.GID); err != nil {
		return err
	}
	return ctx.Err()
}
