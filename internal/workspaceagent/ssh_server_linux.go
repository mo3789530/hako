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
	WorkspaceSSHEnabledEnvironment = "HAKO_WORKSPACE_SSH_ENABLED"
	workspaceSSHKeygenBinary       = "/usr/bin/ssh-keygen"
	workspaceSSHDConfigPath        = "/run/hako/sshd_config"
	workspaceSSHDRunPath           = "/run/sshd"
	workspaceSSHKeyDir             = "/workspace/.hako/ssh"
	workspaceSSHPrivateKey         = "/workspace/.hako/ssh/ssh_host_ed25519_key"
	workspaceSSHPublicKey          = "/workspace/.hako/ssh/ssh_host_ed25519_key.pub"
	workspaceSSHEphemeralKey       = "/run/hako/ssh_host_ed25519_key"
	workspaceSSHAuthorized         = "/workspace/.ssh/authorized_keys"
	workspaceSSHDPath              = "/usr/sbin/sshd"
)

type WorkspaceSSHServerProcess interface {
	Stop(context.Context) error
}

type WorkspaceSSHServerStarter interface {
	Start(context.Context, WorkspaceMountSpec) (WorkspaceSSHServerProcess, error)
}

type sshServerCommandRunner interface {
	Run(context.Context, string, ...string) error
}

type execSSHServerCommandRunner struct{}

func (execSSHServerCommandRunner) Run(ctx context.Context, binary string, args ...string) error {
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "TMPDIR=/tmp"}
	command.Stdout, command.Stderr = io.Discard, io.Discard
	return command.Run()
}

type linuxWorkspaceSSHServerStarter struct {
	root           string
	runner         sshServerCommandRunner
	processStarter func(string, ...string) (WorkspaceSSHServerProcess, error)
	writeFile      func(string, []byte, os.FileMode) error
	mkdirAll       func(string, os.FileMode) error
	stat           func(string) (os.FileInfo, error)
	ensureIdentity func(uint32, uint32) error
	waitReady      func(context.Context, WorkspaceSSHServerProcess) error
}

func newLinuxWorkspaceSSHServerStarter() WorkspaceSSHServerStarter {
	return &linuxWorkspaceSSHServerStarter{
		runner: execSSHServerCommandRunner{}, processStarter: startWorkspaceSSHD,
		writeFile: os.WriteFile, mkdirAll: os.MkdirAll, stat: os.Lstat,
		ensureIdentity: ensureWorkspaceRootlessIdentity, waitReady: waitWorkspaceSSHReady,
	}
}

func (s *linuxWorkspaceSSHServerStarter) Start(ctx context.Context, spec WorkspaceMountSpec) (WorkspaceSSHServerProcess, error) {
	if s == nil || ctx == nil || ctx.Err() != nil || !validWorkspaceMountSpec(spec) || spec.MountPath != "/workspace" ||
		s.runner == nil || s.processStarter == nil || s.writeFile == nil || s.mkdirAll == nil || s.stat == nil || s.ensureIdentity == nil {
		return nil, errors.New("Workspace SSH server configuration is invalid")
	}
	if !workspaceSSHDPathsAreFixed() || (s.root != "" && (!filepath.IsAbs(s.root) || filepath.Clean(s.root) != s.root)) {
		return nil, errors.New("Workspace SSH server paths are invalid")
	}
	if err := s.ensureIdentity(spec.UID, spec.GID); err != nil {
		return nil, errors.New("prepare Workspace SSH account")
	}
	if err := s.prepareDirectories(); err != nil {
		return nil, errors.New("prepare Workspace SSH directories")
	}
	if err := s.ensureHostKey(ctx); err != nil {
		return nil, errors.New("prepare Workspace SSH host key")
	}
	privateKey, err := readWorkspaceSSHPrivateKey(s.path(workspaceSSHPrivateKey))
	if err != nil {
		return nil, errors.New("read Workspace SSH host key")
	}
	ephemeralKeyPath := s.path(workspaceSSHEphemeralKey)
	if err := writeRootPrivateFile(ephemeralKeyPath, privateKey); err != nil {
		return nil, errors.New("prepare runtime Workspace SSH host key")
	}
	started := false
	defer func() {
		if !started {
			_ = os.Remove(ephemeralKeyPath)
		}
	}()
	configPath := s.path(workspaceSSHDConfigPath)
	config := workspaceSSHDConfig(ephemeralKeyPath, s.path(workspaceSSHAuthorized), spec.UID, spec.GID)
	if err := s.writeFile(configPath, []byte(config), 0600); err != nil {
		return nil, errors.New("write Workspace SSH server configuration")
	}
	if err := s.runner.Run(ctx, s.path(workspaceSSHDPath), "-t", "-f", configPath); err != nil {
		return nil, errors.New("validate Workspace SSH server configuration")
	}
	process, err := s.processStarter(s.path(workspaceSSHDPath), "-D", "-e", "-f", configPath)
	if err != nil || process == nil {
		if process != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = process.Stop(stopCtx)
			cancel()
		}
		return nil, errors.New("start Workspace SSH server")
	}
	waitReady := s.waitReady
	if waitReady == nil {
		waitReady = waitWorkspaceSSHReady
	}
	if err := waitReady(ctx, process); err != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = process.Stop(stopCtx)
		cancel()
		return nil, errors.New("Workspace SSH server did not become ready")
	}
	started = true
	return &workspaceSSHServerRuntime{process: process, privateKeyPath: ephemeralKeyPath}, nil
}

type workspaceSSHServerRuntime struct {
	process        WorkspaceSSHServerProcess
	privateKeyPath string
}

func (p *workspaceSSHServerRuntime) Stop(ctx context.Context) error {
	if p == nil || p.process == nil || p.privateKeyPath == "" || ctx == nil {
		return errors.New("Workspace SSH server shutdown is invalid")
	}
	processErr := p.process.Stop(ctx)
	removeErr := os.Remove(p.privateKeyPath)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	if processErr != nil {
		return processErr
	}
	return removeErr
}

func readWorkspaceSSHPrivateKey(path string) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("Workspace SSH key path is invalid")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() < 1 || info.Size() > 16*1024 {
		return nil, errors.New("Workspace SSH private key has unsafe type, permissions, or size")
	}
	data, err := io.ReadAll(io.LimitReader(file, 16*1024+1))
	if err != nil || len(data) == 0 || len(data) > 16*1024 {
		return nil, errors.New("Workspace SSH private key is invalid")
	}
	return data, nil
}

func writeRootPrivateFile(path string, data []byte) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(data) == 0 || len(data) > 16*1024 {
		return errors.New("runtime Workspace SSH key is invalid")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		_ = os.Remove(path)
		return writeErr
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return closeErr
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		_ = os.Remove(path)
		return errors.New("runtime Workspace SSH key has unsafe type or permissions")
	}
	return nil
}

func (s *linuxWorkspaceSSHServerStarter) prepareDirectories() error {
	for _, directory := range []struct {
		path string
		mode os.FileMode
	}{
		{path: s.path("/workspace/.hako"), mode: 0700},
		{path: s.path(workspaceSSHKeyDir), mode: 0700},
		{path: s.path("/run/hako"), mode: 0700},
		{path: s.path(workspaceSSHDRunPath), mode: 0755},
	} {
		if err := s.mkdirAll(directory.path, directory.mode); err != nil {
			return err
		}
		info, err := s.stat(directory.path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return errors.New("Workspace SSH directory has unsafe type or permissions")
		}
	}
	return nil
}

func (s *linuxWorkspaceSSHServerStarter) ensureHostKey(ctx context.Context) error {
	privatePath, publicPath := s.path(workspaceSSHPrivateKey), s.path(workspaceSSHPublicKey)
	privateInfo, privateErr := s.stat(privatePath)
	publicInfo, publicErr := s.stat(publicPath)
	privateMissing, publicMissing := errors.Is(privateErr, os.ErrNotExist), errors.Is(publicErr, os.ErrNotExist)
	if privateErr != nil && !privateMissing || publicErr != nil && !publicMissing {
		return errors.New("cannot inspect Workspace SSH host key")
	}
	if privateMissing != publicMissing {
		return errors.New("Workspace SSH host key pair is incomplete")
	}
	if privateMissing {
		keyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := s.runner.Run(keyCtx, s.path(workspaceSSHKeygenBinary), "-q", "-t", "ed25519", "-N", "", "-f", privatePath); err != nil {
			return errors.New("generate Workspace SSH host key")
		}
		privateInfo, privateErr = s.stat(privatePath)
		publicInfo, publicErr = s.stat(publicPath)
	}
	if privateErr != nil || publicErr != nil || !privateInfo.Mode().IsRegular() || !publicInfo.Mode().IsRegular() ||
		privateInfo.Mode().Perm() != 0600 || publicInfo.Mode().Perm()&0022 != 0 {
		return errors.New("Workspace SSH host key files have unsafe type or permissions")
	}
	return nil
}

func workspaceSSHDConfig(hostKey, authorizedKeys string, uid, gid uint32) string {
	return fmt.Sprintf(`Port 22
AddressFamily inet
ListenAddress 127.0.0.1
HostKey %s
PidFile /run/hako/sshd.pid
AuthorizedKeysFile %s
StrictModes yes
PubkeyAuthentication yes
AuthenticationMethods publickey
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
AllowUsers hako-workspace
PermitUserEnvironment no
PermitEmptyPasswords no
UsePAM no
X11Forwarding no
AllowAgentForwarding no
AllowTcpForwarding local
PermitOpen 127.0.0.1:*
GatewayPorts no
PermitTunnel no
MaxAuthTries 3
MaxSessions 4
ClientAliveInterval 30
ClientAliveCountMax 3
Subsystem sftp internal-sftp
`, hostKey, authorizedKeys) + fmt.Sprintf("# Workspace identity uid=%s gid=%s\n", strconv.FormatUint(uint64(uid), 10), strconv.FormatUint(uint64(gid), 10))
}

func (s *linuxWorkspaceSSHServerStarter) path(path string) string {
	if s.root == "" {
		return path
	}
	return filepath.Join(s.root, strings.TrimPrefix(path, string(filepath.Separator)))
}

func workspaceSSHDPathsAreFixed() bool {
	for _, path := range []string{workspaceSSHDConfigPath, workspaceSSHDRunPath, workspaceSSHKeyDir,
		workspaceSSHPrivateKey, workspaceSSHPublicKey, workspaceSSHEphemeralKey, workspaceSSHAuthorized, workspaceSSHDPath} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.Contains(path, "..") {
			return false
		}
	}
	return workspaceSSHDConfigPath == "/run/hako/sshd_config" && workspaceSSHDRunPath == "/run/sshd" &&
		workspaceSSHKeyDir == "/workspace/.hako/ssh" && workspaceSSHPrivateKey == filepath.Join(workspaceSSHKeyDir, "ssh_host_ed25519_key") &&
		workspaceSSHPublicKey == workspaceSSHPrivateKey+".pub" && workspaceSSHEphemeralKey == "/run/hako/ssh_host_ed25519_key" && workspaceSSHAuthorized == "/workspace/.ssh/authorized_keys" &&
		workspaceSSHDPath == "/usr/sbin/sshd"
}

type execWorkspaceSSHDProcess struct {
	command *exec.Cmd
	done    chan struct{}
}

type workspaceSSHServerLiveness interface {
	Alive() bool
}

func waitWorkspaceSSHReady(ctx context.Context, process WorkspaceSSHServerProcess) error {
	if ctx == nil || process == nil {
		return errors.New("Workspace SSH readiness check is invalid")
	}
	readyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if state, ok := process.(workspaceSSHServerLiveness); ok && !state.Alive() {
			return errors.New("Workspace SSH server exited before readiness")
		}
		connection, err := net.DialTimeout("tcp4", "127.0.0.1:22", 100*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return nil
		}
		select {
		case <-readyCtx.Done():
			return readyCtx.Err()
		case <-ticker.C:
		}
	}
}

func (p *execWorkspaceSSHDProcess) Alive() bool {
	if p == nil || p.command == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func startWorkspaceSSHD(binary string, args ...string) (WorkspaceSSHServerProcess, error) {
	command := exec.Command(binary, args...)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "TMPDIR=/tmp"}
	command.Stdout, command.Stderr = io.Discard, io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	process := &execWorkspaceSSHDProcess{command: command, done: make(chan struct{})}
	go func() {
		_ = command.Wait()
		close(process.done)
	}()
	return process, nil
}

func (p *execWorkspaceSSHDProcess) Stop(ctx context.Context) error {
	if p == nil || p.command == nil || p.command.Process == nil || ctx == nil {
		return errors.New("Workspace SSH server shutdown is invalid")
	}
	select {
	case <-p.done:
		return nil
	default:
	}
	if err := syscall.Kill(-p.command.Process.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGKILL)
		select {
		case <-p.done:
		case <-time.After(time.Second):
		}
		return ctx.Err()
	}
}
