//go:build linux

package workspaceagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadagent"
)

const (
	workspaceAgentSocketPath                 = "/run/hako/workspace-agent.sock"
	workspaceAgentBinaryPath                 = "/usr/local/bin/hako-workspace-agent"
	workspaceAgentProtocolVersion            = 1
	workspaceAgentHeartbeatInterval          = 10 * time.Second
	workspaceAgentHeartbeatLease             = 30 * time.Second
	WorkspaceAgentTrustedKeysFileEnvironment = "HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE"
	WorkspaceGatewayOriginEnvironment        = "HAKO_GATEWAY_ORIGIN"
	ChildListenerFileDescriptor              = 3
)

// workspaceAgentHeartbeat is exchanged only over the root-owned private
// Unix socket. The root init verifies the bound identity before forwarding a
// new external request to the unprivileged Agent.
type workspaceAgentHeartbeat struct {
	ProtocolVersion int                    `json:"protocol_version"`
	TenantID        domain.TenantID        `json:"tenant_id"`
	WorkspaceID     domain.WorkspaceID     `json:"workspace_id"`
	ResourcePlaneID domain.ResourcePlaneID `json:"resource_plane_id"`
	MicroVMID       string                 `json:"microvm_id"`
}

// WorkspaceAgentLaunch contains only signed /run identity and image-owned
// configuration. It deliberately has no client-controlled command or env.
type WorkspaceAgentLaunch struct {
	MicroVMID        string             `json:"microvm_id"`
	Mount            WorkspaceMountSpec `json:"mount"`
	GatewayOrigin    string             `json:"gateway_origin"`
	TrustedKeysFile  string             `json:"trusted_keys_file"`
	EnableServices   bool               `json:"enable_services"`
	ContainerdSocket string             `json:"containerd_socket,omitempty"`
}

type RootlessContainerdRuntime interface {
	SocketPath() string
	Stop(context.Context) error
}

type RootlessContainerdStarter func(context.Context, RootlessContainerdConfig) (RootlessContainerdRuntime, error)

type WorkspaceAgentProcess interface {
	Done() <-chan error
	Stop(context.Context) error
}

type WorkspaceAgentLauncher interface {
	Start(WorkspaceAgentLaunch, net.Listener) (WorkspaceAgentProcess, error)
}

type WorkspaceAgentSupervisorConfig struct {
	SocketPath              string
	BinaryPath              string
	TrustedKeysFile         string
	GatewayOrigin           string
	ServiceFenceDirectory   string
	EnableServices          bool
	EnableSSH               bool
	HeartbeatInterval       time.Duration
	HeartbeatLease          time.Duration
	PrepareRuntimeDirectory func(uint32, uint32) error
	PrepareRootlessIdentity func(uint32, uint32) error
	StartRootlessContainerd RootlessContainerdStarter
	StartSSHServer          WorkspaceSSHServerStarter
	Launcher                WorkspaceAgentLauncher
	isRoot                  func() bool
}

// WorkspaceAgentSupervisor keeps the privileged lifecycle listener in the
// root init process while serving interactive routes from a dedicated
// unprivileged child over a root-owned Unix socket.
type WorkspaceAgentSupervisor struct {
	mu            sync.Mutex
	heartbeatMu   sync.RWMutex
	config        WorkspaceAgentSupervisorConfig
	process       WorkspaceAgentProcess
	containerd    RootlessContainerdRuntime
	sshServer     WorkspaceSSHServerProcess
	bound         AgentTicket
	launch        WorkspaceAgentLaunch
	proxy         http.Handler
	serviceGuard  *WorkspaceServiceSupervisorGuard
	isRoot        func() bool
	lastHeartbeat time.Time
}

func NewLinuxWorkspaceAgentSupervisor(config WorkspaceAgentSupervisorConfig) (*WorkspaceAgentSupervisor, error) {
	if config.isRoot == nil {
		config.isRoot = func() bool { return os.Geteuid() == 0 }
	}
	if !config.isRoot() {
		return nil, errors.New("Workspace Agent supervisor must run as root")
	}
	config.SocketPath = strings.TrimSpace(config.SocketPath)
	config.BinaryPath = strings.TrimSpace(config.BinaryPath)
	config.TrustedKeysFile = strings.TrimSpace(config.TrustedKeysFile)
	config.GatewayOrigin = strings.TrimSpace(config.GatewayOrigin)
	config.ServiceFenceDirectory = strings.TrimSpace(config.ServiceFenceDirectory)
	if config.SocketPath == "" {
		config.SocketPath = workspaceAgentSocketPath
	}
	if config.BinaryPath == "" {
		config.BinaryPath = workspaceAgentBinaryPath
	}
	if config.ServiceFenceDirectory == "" {
		config.ServiceFenceDirectory = defaultWorkspaceServiceFenceDirectory
	}
	if config.HeartbeatInterval == 0 {
		config.HeartbeatInterval = workspaceAgentHeartbeatInterval
	}
	if config.HeartbeatLease == 0 {
		config.HeartbeatLease = workspaceAgentHeartbeatLease
	}
	if config.Launcher == nil {
		config.Launcher = execWorkspaceAgentLauncher{binaryPath: config.BinaryPath}
	}
	if config.PrepareRuntimeDirectory == nil {
		config.PrepareRuntimeDirectory = prepareWorkspaceAgentRuntimeDirectory
	}
	if config.PrepareRootlessIdentity == nil {
		config.PrepareRootlessIdentity = ensureWorkspaceRootlessIdentity
	}
	if config.StartRootlessContainerd == nil {
		config.StartRootlessContainerd = func(ctx context.Context, config RootlessContainerdConfig) (RootlessContainerdRuntime, error) {
			return StartLinuxRootlessContainerd(ctx, config)
		}
	}
	if config.EnableSSH && config.StartSSHServer == nil {
		config.StartSSHServer = newLinuxWorkspaceSSHServerStarter()
	}
	if !filepath.IsAbs(config.SocketPath) || !filepath.IsAbs(config.BinaryPath) || !filepath.IsAbs(config.TrustedKeysFile) ||
		config.HeartbeatInterval < 10*time.Millisecond || config.HeartbeatInterval > time.Minute ||
		config.HeartbeatLease < 2*config.HeartbeatInterval || config.HeartbeatLease > 10*time.Minute ||
		!validGatewayOrigin(config.GatewayOrigin) || config.SocketPath == "/" || strings.Contains(config.SocketPath, "..") {
		return nil, errors.New("Workspace Agent image configuration is invalid")
	}
	target, _ := url.Parse("http://workspace-agent")
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", config.SocketPath)
		},
		DisableKeepAlives: true,
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "Workspace Agent is not ready", http.StatusServiceUnavailable)
	}
	return &WorkspaceAgentSupervisor{config: config, proxy: proxy, isRoot: config.isRoot}, nil
}

func validGatewayOrigin(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func (s *WorkspaceAgentSupervisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s == nil || r.URL.Path == "/" || !strings.HasPrefix(r.URL.Path, "/v1/workspaces/") {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	process := s.process
	launch := s.launch
	childExited := false
	if process != nil {
		select {
		case <-process.Done():
			childExited = true
		default:
		}
	}
	s.mu.Unlock()
	if childExited {
		if err := s.BindWorkspaceRuntime(launch.MicroVMID, launch.Mount); err != nil {
			http.Error(w, "Workspace Agent is not ready", http.StatusServiceUnavailable)
			return
		}
	}
	s.mu.Lock()
	ready := s.process != nil
	serviceGuard := s.serviceGuard
	workspaceID := s.bound.WorkspaceID
	s.mu.Unlock()
	if !ready {
		http.Error(w, "Workspace Agent is not ready", http.StatusServiceUnavailable)
		return
	}
	if !s.heartbeatLeaseActive(time.Now()) {
		http.Error(w, "Workspace Agent heartbeat lease expired", http.StatusServiceUnavailable)
		return
	}
	if err := s.checkHeartbeat(launch); err != nil {
		http.Error(w, "Workspace Agent heartbeat is unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.URL.Path == "/v1/workspaces/"+string(workspaceID)+"/services/reconcile" {
		if serviceGuard == nil {
			http.Error(w, "Workspace Service runtime is not ready", http.StatusServiceUnavailable)
			return
		}
		if status, err := serviceGuard.Authorize(r); err != nil {
			http.Error(w, "invalid Workspace Service request", status)
			return
		}
	}
	s.proxy.ServeHTTP(w, r)
}

func (s *WorkspaceAgentSupervisor) BindWorkspaceRuntime(microVMID string, spec WorkspaceMountSpec) error {
	if s == nil || s.isRoot == nil || !s.isRoot() || !validWorkspaceHookVMID(microVMID) || !validWorkspaceMountSpec(spec) {
		return errors.New("Workspace Agent runtime binding is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	identity := AgentTicket{TenantID: domain.TenantID(spec.TenantID), WorkspaceID: domain.WorkspaceID(spec.WorkspaceID),
		ResourcePlane: domain.ResourcePlaneID(spec.ResourcePlaneID), MicroVMID: microVMID}
	if s.process != nil {
		if s.bound != identity {
			return errors.New("Workspace Agent is already bound to another runtime")
		}
		select {
		case <-s.process.Done():
			// Re-run the child after a crash or clean exit. The root-owned
			// Service fence is reloaded below before the child can serve requests.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = s.process.Stop(ctx)
			cancel()
			if err := os.Remove(s.config.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return errors.New("remove stopped Workspace Agent socket")
			}
			s.process, s.serviceGuard = nil, nil
		default:
			// The same binding is already active.
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.config.SocketPath), 0700); err != nil {
		return errors.New("prepare Workspace Agent socket directory")
	}
	if err := s.config.PrepareRuntimeDirectory(spec.UID, spec.GID); err != nil {
		return errors.New("prepare private Workspace runtime tmpfs")
	}
	if s.config.EnableServices || s.config.EnableSSH {
		if err := s.config.PrepareRootlessIdentity(spec.UID, spec.GID); err != nil {
			return errors.New("prepare Workspace runtime account")
		}
	}
	keyFile, err := os.Open(s.config.TrustedKeysFile)
	if err != nil {
		return errors.New("open image-provisioned Workspace Agent public keys")
	}
	trustedKeys, keyErr := workloadagent.LoadTrustedKeys(keyFile)
	closeErr := keyFile.Close()
	if keyErr != nil || closeErr != nil {
		return errors.New("load image-provisioned Workspace Agent public keys")
	}
	serviceGuard, err := NewWorkspaceServiceSupervisorGuard(WorkspaceServiceIdentity{
		TenantID: domain.TenantID(spec.TenantID), WorkspaceID: domain.WorkspaceID(spec.WorkspaceID),
		ResourcePlaneID: domain.ResourcePlaneID(spec.ResourcePlaneID), MicroVMID: microVMID,
	}, trustedKeys, s.config.ServiceFenceDirectory, nil)
	if err != nil {
		return errors.New("configure privileged Workspace Service revision fence")
	}
	startedContainerd := false
	if s.config.EnableServices && s.containerd == nil {
		containerd, err := s.config.StartRootlessContainerd(context.Background(), RootlessContainerdConfig{
			RuntimeDirectory: "/run/user/" + strconv.FormatUint(uint64(spec.UID), 10),
			WorkspaceRoot:    spec.MountPath,
			UID:              spec.UID,
			GID:              spec.GID,
		})
		wantSocket := filepath.Join("/run/user", strconv.FormatUint(uint64(spec.UID), 10), "containerd", "containerd.sock")
		if err != nil || containerd == nil || containerd.SocketPath() != wantSocket {
			if containerd != nil {
				stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = containerd.Stop(stopCtx)
				cancel()
			}
			return errors.New("start rootless Workspace containerd")
		}
		s.containerd = containerd
		startedContainerd = true
	}
	startedSSH := false
	if s.config.EnableSSH && s.sshServer == nil {
		sshServer, err := s.config.StartSSHServer.Start(context.Background(), spec)
		if err != nil || sshServer == nil {
			if sshServer != nil {
				stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = sshServer.Stop(stopCtx)
				cancel()
			}
			if startedContainerd {
				s.stopRootlessContainerd()
			}
			return errors.New("start Workspace SSH server")
		}
		s.sshServer = sshServer
		startedSSH = true
	}
	if err := os.Remove(s.config.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		if startedSSH {
			s.stopSSHServer()
		}
		if startedContainerd {
			s.stopRootlessContainerd()
		}
		return errors.New("remove stale Workspace Agent socket")
	}
	listener, err := net.Listen("unix", s.config.SocketPath)
	if err != nil {
		if startedSSH {
			s.stopSSHServer()
		}
		if startedContainerd {
			s.stopRootlessContainerd()
		}
		return errors.New("listen on Workspace Agent Unix socket")
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	if err := os.Chmod(s.config.SocketPath, 0600); err != nil {
		_ = listener.Close()
		_ = os.Remove(s.config.SocketPath)
		if startedSSH {
			s.stopSSHServer()
		}
		if startedContainerd {
			s.stopRootlessContainerd()
		}
		return errors.New("secure Workspace Agent Unix socket")
	}
	launch := WorkspaceAgentLaunch{MicroVMID: microVMID, Mount: spec,
		GatewayOrigin: s.config.GatewayOrigin, TrustedKeysFile: s.config.TrustedKeysFile, EnableServices: s.config.EnableServices}
	if s.containerd != nil {
		launch.ContainerdSocket = s.containerd.SocketPath()
	}
	process, err := s.config.Launcher.Start(launch, listener)
	_ = listener.Close()
	if err != nil || process == nil {
		_ = os.Remove(s.config.SocketPath)
		if startedContainerd {
			s.stopRootlessContainerd()
		}
		if startedSSH {
			s.stopSSHServer()
		}
		return errors.New("start unprivileged Workspace Agent")
	}
	if err := s.waitReady(process, launch); err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = process.Stop(ctx)
		cancel()
		_ = os.Remove(s.config.SocketPath)
		if startedContainerd {
			s.stopRootlessContainerd()
		}
		if startedSSH {
			s.stopSSHServer()
		}
		return err
	}
	s.process, s.bound, s.launch, s.serviceGuard = process, identity, launch, serviceGuard
	go s.monitorHeartbeat(process, launch)
	return nil
}

func prepareWorkspaceAgentRuntimeDirectory(uid, gid uint32) error {
	if uid < 100000 || gid < 100000 {
		return errors.New("invalid Workspace runtime uid mapping")
	}
	runInfo, err := os.Lstat("/run")
	if err != nil || !runInfo.IsDir() || runInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("invalid /run directory")
	}
	userRoot := "/run/user"
	if info, err := os.Lstat(userRoot); errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(userRoot, 0755); err != nil {
			return err
		}
	} else if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return errors.New("invalid /run/user directory")
	}
	runtimeDir := filepath.Join(userRoot, strconv.FormatUint(uint64(uid), 10))
	if info, err := os.Lstat(runtimeDir); errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(runtimeDir, 0700); err != nil {
			return err
		}
	} else if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("invalid Workspace runtime directory")
	}
	if isTmpfsDirectory(runtimeDir) {
		return validateWorkspaceRuntimeTmpfs(runtimeDir, uid, gid)
	}
	entries, err := os.ReadDir(runtimeDir)
	if err != nil || len(entries) != 0 {
		return errors.New("refusing to cover a nonempty Workspace runtime directory")
	}
	options := fmt.Sprintf("size=8m,mode=0700,uid=%d,gid=%d", uid, gid)
	if err := unix.Mount("tmpfs", runtimeDir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, options); err != nil {
		return err
	}
	if err := validateWorkspaceRuntimeTmpfs(runtimeDir, uid, gid); err != nil {
		_ = unix.Unmount(runtimeDir, unix.MNT_DETACH)
		return err
	}
	return nil
}

func validateWorkspaceRuntimeTmpfs(path string, uid, gid uint32) error {
	if !isTmpfsDirectory(path) {
		return errors.New("Workspace runtime directory is not tmpfs")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("Workspace runtime tmpfs permissions are invalid")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uid || stat.Gid != gid {
		return errors.New("Workspace runtime tmpfs ownership is invalid")
	}
	return nil
}

func (s *WorkspaceAgentSupervisor) waitReady(process WorkspaceAgentProcess, launch WorkspaceAgentLaunch) error {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-process.Done():
			return errors.New("Workspace Agent exited before becoming ready")
		default:
		}
		if err := s.checkHeartbeat(launch); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("Workspace Agent did not become ready")
}

func (s *WorkspaceAgentSupervisor) checkHeartbeat(launch WorkspaceAgentLaunch) error {
	if launch.MicroVMID == "" || launch.Mount.WorkspaceID == "" {
		return errors.New("Workspace Agent heartbeat binding is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(ctx, "unix", s.config.SocketPath)
	}, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://workspace-agent/internal/heartbeat", nil)
	if err != nil {
		return errors.New("create Workspace Agent heartbeat request")
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("Workspace Agent heartbeat failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("Workspace Agent heartbeat returned an invalid status")
	}
	var heartbeat workspaceAgentHeartbeat
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4097))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&heartbeat) != nil || decoder.Decode(new(any)) != io.EOF || heartbeat.ProtocolVersion != workspaceAgentProtocolVersion ||
		heartbeat.TenantID != domain.TenantID(launch.Mount.TenantID) || heartbeat.WorkspaceID != domain.WorkspaceID(launch.Mount.WorkspaceID) ||
		heartbeat.ResourcePlaneID != domain.ResourcePlaneID(launch.Mount.ResourcePlaneID) || heartbeat.MicroVMID != launch.MicroVMID {
		return errors.New("Workspace Agent heartbeat identity or protocol version does not match")
	}
	s.heartbeatMu.Lock()
	s.lastHeartbeat = time.Now()
	s.heartbeatMu.Unlock()
	return nil
}

func (s *WorkspaceAgentSupervisor) monitorHeartbeat(process WorkspaceAgentProcess, launch WorkspaceAgentLaunch) {
	ticker := time.NewTicker(s.config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-process.Done():
			return
		case <-ticker.C:
			_ = s.checkHeartbeat(launch)
		}
	}
}

func (s *WorkspaceAgentSupervisor) heartbeatLeaseActive(now time.Time) bool {
	s.heartbeatMu.RLock()
	last := s.lastHeartbeat
	s.heartbeatMu.RUnlock()
	return !last.IsZero() && now.Before(last.Add(s.config.HeartbeatLease))
}

func (s *WorkspaceAgentSupervisor) Stop(ctx context.Context) error {
	if s == nil || ctx == nil {
		return errors.New("Workspace Agent shutdown context is required")
	}
	s.mu.Lock()
	process := s.process
	containerd := s.containerd
	sshServer := s.sshServer
	s.process = nil
	s.containerd = nil
	s.sshServer = nil
	s.serviceGuard = nil
	s.mu.Unlock()
	s.heartbeatMu.Lock()
	s.lastHeartbeat = time.Time{}
	s.heartbeatMu.Unlock()
	var sshErr error
	if sshServer != nil {
		sshErr = sshServer.Stop(ctx)
	}
	if process == nil {
		if containerd == nil {
			return sshErr
		}
		if err := containerd.Stop(ctx); err != nil && sshErr == nil {
			return err
		}
		return sshErr
	}
	err := process.Stop(ctx)
	if removeErr := os.Remove(s.config.SocketPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && err == nil {
		err = removeErr
	}
	if containerd != nil {
		if containerdErr := containerd.Stop(ctx); containerdErr != nil && err == nil {
			err = containerdErr
		}
	}
	if sshErr != nil && err == nil {
		err = sshErr
	}
	return err
}

func (s *WorkspaceAgentSupervisor) stopSSHServer() {
	if s.sshServer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = s.sshServer.Stop(ctx)
	cancel()
	s.sshServer = nil
}

func (s *WorkspaceAgentSupervisor) stopRootlessContainerd() {
	if s.containerd == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = s.containerd.Stop(ctx)
	cancel()
	s.containerd = nil
}

type WorkspaceAgentChildPayload struct {
	MicroVMID     string             `json:"microvm_id"`
	Mount         WorkspaceMountSpec `json:"mount"`
	GatewayOrigin string             `json:"gateway_origin"`
}

func ReadWorkspaceAgentChildPayload(reader io.Reader) (WorkspaceAgentChildPayload, error) {
	var payload WorkspaceAgentChildPayload
	if reader == nil {
		return WorkspaceAgentChildPayload{}, errors.New("invalid Workspace Agent child configuration")
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxWorkspaceHookBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxWorkspaceHookBytes {
		return WorkspaceAgentChildPayload{}, errors.New("invalid Workspace Agent child configuration")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil || decoder.Decode(new(any)) != io.EOF ||
		!validWorkspaceHookVMID(payload.MicroVMID) || !validWorkspaceMountSpec(payload.Mount) || !validGatewayOrigin(payload.GatewayOrigin) {
		return WorkspaceAgentChildPayload{}, errors.New("invalid Workspace Agent child configuration")
	}
	return payload, nil
}

type execWorkspaceAgentLauncher struct{ binaryPath string }

func (l execWorkspaceAgentLauncher) Start(config WorkspaceAgentLaunch, listener net.Listener) (WorkspaceAgentProcess, error) {
	unixListener, ok := listener.(*net.UnixListener)
	if !ok || !validWorkspaceMountSpec(config.Mount) || config.Mount.UID == 0 || config.Mount.GID == 0 {
		return nil, errors.New("Workspace Agent child launch identity is invalid")
	}
	file, err := unixListener.File()
	if err != nil {
		return nil, errors.New("inherit Workspace Agent Unix listener")
	}
	payload, err := json.Marshal(WorkspaceAgentChildPayload{MicroVMID: config.MicroVMID, Mount: config.Mount, GatewayOrigin: config.GatewayOrigin})
	if err != nil {
		_ = file.Close()
		return nil, errors.New("encode Workspace Agent child configuration")
	}
	command := exec.Command(l.binaryPath)
	command.Stdin = strings.NewReader(string(payload))
	command.Stdout, command.Stderr = io.Discard, io.Discard
	command.ExtraFiles = []*os.File{file}
	command.Env = []string{"PATH=/usr/bin:/bin", "HOME=/workspace", WorkspaceAgentTrustedKeysFileEnvironment + "=" + config.TrustedKeysFile}
	if config.EnableServices {
		command.Env = append(command.Env, WorkspaceServicesEnabledEnvironment+"=true")
		if !filepath.IsAbs(config.ContainerdSocket) || config.ContainerdSocket != filepath.Clean(config.ContainerdSocket) {
			_ = file.Close()
			return nil, errors.New("Workspace containerd socket configuration is invalid")
		}
		command.Env = append(command.Env, ContainerdSocketEnvironment+"="+config.ContainerdSocket)
	}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: config.Mount.UID, Gid: config.Mount.GID, Groups: []uint32{}}}
	if err := command.Start(); err != nil {
		_ = file.Close()
		return nil, errors.New("launch unprivileged Workspace Agent child")
	}
	_ = file.Close()
	process := &execWorkspaceAgentProcess{command: command, done: make(chan error, 1)}
	go func() { process.done <- command.Wait(); close(process.done) }()
	return process, nil
}

type execWorkspaceAgentProcess struct {
	command *exec.Cmd
	done    chan error
}

func (p *execWorkspaceAgentProcess) Done() <-chan error { return p.done }

func (p *execWorkspaceAgentProcess) Stop(ctx context.Context) error {
	if p == nil || ctx == nil {
		return errors.New("Workspace Agent shutdown context is required")
	}
	if p.command.Process == nil {
		return nil
	}
	_ = p.command.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-p.done:
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return nil
	case <-ctx.Done():
		_ = p.command.Process.Kill()
		<-p.done
		return ctx.Err()
	}
}

var _ WorkspaceRuntimeBinder = (*WorkspaceAgentSupervisor)(nil)
var _ http.Handler = (*WorkspaceAgentSupervisor)(nil)
