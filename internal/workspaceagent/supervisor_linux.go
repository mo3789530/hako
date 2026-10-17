//go:build linux

package workspaceagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

const (
	workspaceAgentSocketPath                 = "/run/hako/workspace-agent.sock"
	workspaceAgentBinaryPath                 = "/usr/local/bin/hako-workspace-agent"
	WorkspaceAgentTrustedKeysFileEnvironment = "HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE"
	WorkspaceGatewayOriginEnvironment        = "HAKO_GATEWAY_ORIGIN"
	ChildListenerFileDescriptor              = 3
)

// WorkspaceAgentLaunch contains only signed /run identity and image-owned
// configuration. It deliberately has no client-controlled command or env.
type WorkspaceAgentLaunch struct {
	MicroVMID       string             `json:"microvm_id"`
	Mount           WorkspaceMountSpec `json:"mount"`
	GatewayOrigin   string             `json:"gateway_origin"`
	TrustedKeysFile string             `json:"trusted_keys_file"`
}

type WorkspaceAgentProcess interface {
	Done() <-chan error
	Stop(context.Context) error
}

type WorkspaceAgentLauncher interface {
	Start(WorkspaceAgentLaunch, net.Listener) (WorkspaceAgentProcess, error)
}

type WorkspaceAgentSupervisorConfig struct {
	SocketPath      string
	BinaryPath      string
	TrustedKeysFile string
	GatewayOrigin   string
	Launcher        WorkspaceAgentLauncher
	isRoot          func() bool
}

// WorkspaceAgentSupervisor keeps the privileged lifecycle listener in the
// root init process while serving interactive routes from a dedicated
// unprivileged child over a root-owned Unix socket.
type WorkspaceAgentSupervisor struct {
	mu      sync.Mutex
	config  WorkspaceAgentSupervisorConfig
	process WorkspaceAgentProcess
	bound   AgentTicket
	proxy   http.Handler
	isRoot  func() bool
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
	if config.SocketPath == "" {
		config.SocketPath = workspaceAgentSocketPath
	}
	if config.BinaryPath == "" {
		config.BinaryPath = workspaceAgentBinaryPath
	}
	if config.Launcher == nil {
		config.Launcher = execWorkspaceAgentLauncher{binaryPath: config.BinaryPath}
	}
	if !filepath.IsAbs(config.SocketPath) || !filepath.IsAbs(config.BinaryPath) || !filepath.IsAbs(config.TrustedKeysFile) ||
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
	ready := s.process != nil
	s.mu.Unlock()
	if !ready {
		http.Error(w, "Workspace Agent is not ready", http.StatusServiceUnavailable)
		return
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
		case err := <-s.process.Done():
			if err != nil {
				return errors.New("Workspace Agent child process exited")
			}
		default:
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.config.SocketPath), 0700); err != nil {
		return errors.New("prepare Workspace Agent socket directory")
	}
	if err := os.Remove(s.config.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("remove stale Workspace Agent socket")
	}
	listener, err := net.Listen("unix", s.config.SocketPath)
	if err != nil {
		return errors.New("listen on Workspace Agent Unix socket")
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	if err := os.Chmod(s.config.SocketPath, 0600); err != nil {
		_ = listener.Close()
		_ = os.Remove(s.config.SocketPath)
		return errors.New("secure Workspace Agent Unix socket")
	}
	process, err := s.config.Launcher.Start(WorkspaceAgentLaunch{MicroVMID: microVMID, Mount: spec,
		GatewayOrigin: s.config.GatewayOrigin, TrustedKeysFile: s.config.TrustedKeysFile}, listener)
	_ = listener.Close()
	if err != nil || process == nil {
		_ = os.Remove(s.config.SocketPath)
		return errors.New("start unprivileged Workspace Agent")
	}
	if err := s.waitReady(process); err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = process.Stop(ctx)
		cancel()
		_ = os.Remove(s.config.SocketPath)
		return err
	}
	s.process, s.bound = process, identity
	return nil
}

func (s *WorkspaceAgentSupervisor) waitReady(process WorkspaceAgentProcess) error {
	client := &http.Client{Timeout: 250 * time.Millisecond, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(ctx, "unix", s.config.SocketPath)
		}, DisableKeepAlives: true}}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-process.Done():
			return errors.New("Workspace Agent exited before becoming ready")
		default:
		}
		request, _ := http.NewRequest(http.MethodGet, "http://workspace-agent/internal/ready", nil)
		response, err := client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 128))
			_ = response.Body.Close()
			if response.StatusCode == http.StatusNoContent {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("Workspace Agent did not become ready")
}

func (s *WorkspaceAgentSupervisor) Stop(ctx context.Context) error {
	if s == nil || ctx == nil {
		return errors.New("Workspace Agent shutdown context is required")
	}
	s.mu.Lock()
	process := s.process
	s.process = nil
	s.mu.Unlock()
	if process == nil {
		return nil
	}
	err := process.Stop(ctx)
	if removeErr := os.Remove(s.config.SocketPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && err == nil {
		err = removeErr
	}
	return err
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
