//go:build linux

// hako-local-workspace-guest-init is PID 1 for a host-local CloudHypervisor Guest.
// The host supplies immutable Workspace identity in CloudHypervisor kernel args;
// the only Workspace storage it mounts is the dedicated virtio data disk.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/mo3789530/hako/internal/workspaceagent"
	"golang.org/x/sys/unix"
)

const (
	localWorkspaceDevice  = "/dev/vda"
	localWorkspacePath    = "/workspace"
	localWorkspaceUID     = uint32(100000)
	localWorkspaceKeys    = "/etc/hako/workspace-agent-public-keys.json"
	localModulesManifest  = "/etc/hako/local-workspace-kernel-modules"
	localModulesDirectory = "/lib/hako-local-workspace/modules"
)

var (
	localIDPattern       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	localProviderPattern = regexp.MustCompile(`^ch_[a-f0-9]{32}$`)
)

type guestConfig struct {
	TenantID        string
	WorkspaceID     string
	ResourcePlaneID string
	MicroVMID       string
	GatewayOrigin   string
}

type guestInitStageError struct {
	stage string
	err   error
}

func (e *guestInitStageError) Error() string { return e.stage + ": " + e.err.Error() }

func (e *guestInitStageError) Unwrap() error { return e.err }

func guestInitFailure(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &guestInitStageError{stage: stage, err: err}
}

func guestInitFailureLogLine(err error) string {
	stage := "unknown"
	var staged *guestInitStageError
	if errors.As(err, &staged) && staged.stage != "" {
		stage = staged.stage
		if stage == "agent_start" {
			stage = agentStartupDiagnosticStage(err.Error())
		}
	}
	return fmt.Sprintf("Hako local Workspace Guest stopped (failure_class=guest_init stage=%q)", stage)
}

func agentStartupDiagnosticStage(message string) string {
	if !strings.Contains(message, "Workspace SSH server") {
		switch {
		case strings.Contains(message, "Workspace Agent did not become ready"):
			return "agent_start_readiness"
		case strings.Contains(message, "unprivileged Workspace Agent"):
			return "agent_start_child"
		default:
			return "agent_start"
		}
	}
	for _, stage := range []struct{ cause, code string }{
		{"SSH server configuration is invalid", "config"},
		{"SSH server paths are invalid", "paths"},
		{"prepare Workspace SSH account", "identity"},
		{"prepare Workspace SSH directories", "directories"},
		{"prepare Workspace SSH host key", "host_key"},
		{"SSH private key open failed", "key_open"},
		{"SSH private key stat failed", "key_stat"},
		{"SSH private key type rejected", "key_type"},
		{"SSH private key permissions rejected", "key_permissions"},
		{"SSH private key is empty", "key_empty"},
		{"SSH private key is oversized", "key_oversized"},
		{"SSH private key contents rejected", "key_contents"},
		{"read Workspace SSH host key", "read_key"},
		{"prepare runtime Workspace SSH host key", "runtime_key"},
		{"write Workspace SSH server configuration", "write_config"},
		{"validate Workspace SSH server configuration", "validate_config"},
		{"Workspace SSH server did not become ready", "ready"},
		{"start Workspace SSH server", "process"},
	} {
		if strings.Contains(message, stage.cause) {
			return "agent_start_ssh_" + stage.code
		}
	}
	return "agent_start_ssh"
}

func main() {
	if err := run(); err != nil {
		// Log only a fixed stage label. The wrapped cause can contain paths or
		// provider details and must stay inside the process boundary.
		stageLine := guestInitFailureLogLine(err)
		fmt.Fprintln(os.Stderr, stageLine)
		if strings.Contains(stageLine, `stage="agent_start"`) {
			if status, readErr := os.ReadFile("/proc/self/status"); readErr == nil {
				for _, line := range strings.Split(string(status), "\n") {
					if strings.HasPrefix(line, "Uid:") || strings.HasPrefix(line, "Gid:") ||
						strings.HasPrefix(line, "Groups:") || strings.HasPrefix(line, "CapEff:") ||
						strings.HasPrefix(line, "CapBnd:") || strings.HasPrefix(line, "NoNewPrivs:") ||
						strings.HasPrefix(line, "Seccomp:") {
						fmt.Fprintln(os.Stderr, line)
					}
				}
			}
		}
		// PID 1 must not vanish instantly, and the brief pause lets the
		// host-private serial console drain the bounded diagnostic line.
		time.Sleep(100 * time.Millisecond)
		os.Exit(1)
	}
}

func run() error {
	if os.Geteuid() != 0 {
		return guestInitFailure("root_identity", errors.New("local Workspace Guest init requires root"))
	}
	groups, err := os.Getgroups()
	if err != nil || len(groups) != 0 {
		return guestInitFailure("supplementary_groups", errors.New("local Workspace Guest init must have no supplementary groups"))
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGPWR)
	defer signal.Stop(signals)
	if err := mountGuestPseudoFilesystems(); err != nil {
		return guestInitFailure("pseudo_mount", err)
	}
	if err := bringLoopbackUp(); err != nil {
		return guestInitFailure("loopback", errors.New("enable Guest loopback interface"))
	}
	if err := loadGuestKernelModules(); err != nil {
		return guestInitFailure("kernel_modules", errors.New("load local Workspace Guest kernel modules"))
	}
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return guestInitFailure("kernel_arguments", errors.New("read trusted CloudHypervisor kernel command line"))
	}
	config, err := parseGuestConfig(cmdline)
	if err != nil {
		return guestInitFailure("workspace_identity", err)
	}
	if err := mountWorkspaceDisk(); err != nil {
		return guestInitFailure("workspace_disk", err)
	}
	controlListener, err := workspaceagent.ListenWorkspaceAgentVsock(7001)
	if err != nil {
		return guestInitFailure("shutdown_control", errors.New("listen on local Guest shutdown control"))
	}
	defer controlListener.Close()
	shutdownRequests := make(chan struct{}, 1)
	controlServer := &http.Server{Handler: localGuestShutdownHandler(config, shutdownRequests), ReadHeaderTimeout: 2 * time.Second,
		MaxHeaderBytes: 4096}
	defer controlServer.Close()
	go func() { _ = controlServer.Serve(controlListener) }()
	mounted := true
	defer func() {
		if mounted {
			syscall.Sync()
			_ = syscall.Unmount(localWorkspacePath, 0)
		}
	}()

	supervisor, err := workspaceagent.NewLinuxWorkspaceAgentSupervisor(workspaceagent.WorkspaceAgentSupervisorConfig{
		TrustedKeysFile: localWorkspaceKeys,
		GatewayOrigin:   config.GatewayOrigin,
		EnableVsock:     true,
		EnableSSH:       true,
	})
	if err != nil {
		return guestInitFailure("agent_configuration", errors.New("configure local Workspace Agent"))
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = supervisor.Stop(stopCtx)
	}()
	if err := supervisor.BindWorkspaceRuntime(config.MicroVMID, workspaceMountSpec(config)); err != nil {
		return guestInitFailure("agent_start", fmt.Errorf("start local Workspace Agent: %w", err))
	}
	select {
	case <-signals:
	case <-shutdownRequests:
	}
	if guestDebugDiagnostics {
		fmt.Fprintln(os.Stdout, "Hako local Guest SSH host key before agent stop:", guestDebugSSHKeyState())
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := supervisor.Stop(stopCtx); err != nil {
		return guestInitFailure("agent_stop", errors.New("stop local Workspace Agent"))
	}
	if guestDebugDiagnostics {
		fmt.Fprintln(os.Stdout, "Hako local Guest SSH host key after agent stop:", guestDebugSSHKeyState())
	}
	syscall.Sync()
	if err := syscall.Unmount(localWorkspacePath, 0); err != nil {
		return guestInitFailure("workspace_unmount", errors.New("unmount local Workspace disk"))
	}
	mounted = false
	fmt.Fprintln(os.Stdout, "HAKO_LOCAL_GUEST_QUIESCENT_V1")
	return nil
}

func bringLoopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	request, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, request); err != nil {
		return err
	}
	if request.Uint16()&unix.IFF_UP != 0 {
		return nil
	}
	request.SetUint16(request.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, request)
}

func loadGuestKernelModules() error {
	manifest, err := os.ReadFile(localModulesManifest)
	if errors.Is(err, os.ErrNotExist) {
		return nil // the Guest kernel may have these drivers built in
	}
	if err != nil {
		return err
	}
	modules, err := parseGuestKernelModules(manifest)
	if err != nil {
		return err
	}
	for _, name := range modules {
		path := localModulesDirectory + "/" + name
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > 64<<20 {
			return errors.New("local Workspace Guest kernel module is unavailable")
		}
		module, err := os.Open(path)
		if err != nil {
			return errors.New("open local Workspace Guest kernel module")
		}
		loadErr := unix.FinitModule(int(module.Fd()), "", 0)
		_ = module.Close()
		if loadErr != nil && !errors.Is(loadErr, unix.EEXIST) {
			return errors.New("initialize local Workspace Guest kernel module")
		}
	}
	return nil
}

func parseGuestKernelModules(manifest []byte) ([]string, error) {
	if len(manifest) > 256 || len(manifest) > 0 && manifest[len(manifest)-1] != '\n' {
		return nil, errors.New("invalid local Guest kernel module manifest")
	}
	allowed := map[string]int{
		"virtio_mmio.ko": 1, "virtio_blk.ko": 2, "vsock.ko": 3,
		"vmw_vsock_virtio_transport_common.ko": 4, "vmw_vsock_virtio_transport.ko": 5,
	}
	modules := []string{}
	lastOrder := 0
	for _, name := range strings.Split(strings.TrimSuffix(string(manifest), "\n"), "\n") {
		if name == "" && len(manifest) == 0 {
			return modules, nil
		}
		order, ok := allowed[name]
		if !ok || order <= lastOrder {
			return nil, errors.New("invalid local Guest kernel module order")
		}
		modules = append(modules, name)
		lastOrder = order
	}
	return modules, nil
}

func parseGuestConfig(cmdline []byte) (guestConfig, error) {
	values := map[string]string{}
	allowed := map[string]bool{
		"hako.tenant_id": true, "hako.workspace_id": true, "hako.resource_plane_id": true,
		"hako.microvm_id": true, "hako.gateway_origin": true,
	}
	for _, field := range strings.Fields(string(cmdline)) {
		if !strings.HasPrefix(field, "hako.") {
			continue
		}
		key, value, found := strings.Cut(field, "=")
		if !found || !allowed[key] || value == "" || strings.Contains(value, "=") {
			return guestConfig{}, errors.New("CloudHypervisor Workspace identity arguments are invalid")
		}
		if _, exists := values[key]; exists {
			return guestConfig{}, errors.New("duplicate CloudHypervisor Workspace identity argument")
		}
		values[key] = value
	}
	config := guestConfig{TenantID: values["hako.tenant_id"], WorkspaceID: values["hako.workspace_id"],
		ResourcePlaneID: values["hako.resource_plane_id"], MicroVMID: values["hako.microvm_id"],
		GatewayOrigin: values["hako.gateway_origin"]}
	if !localIDPattern.MatchString(config.TenantID) || !localIDPattern.MatchString(config.WorkspaceID) ||
		!localIDPattern.MatchString(config.ResourcePlaneID) || !localProviderPattern.MatchString(config.MicroVMID) || !validGatewayOrigin(config.GatewayOrigin) {
		return guestConfig{}, errors.New("CloudHypervisor Workspace identity arguments are incomplete or invalid")
	}
	return config, nil
}

func guestDebugSSHKeyState() string {
	info, err := os.Lstat("/workspace/.hako/ssh/ssh_host_ed25519_key")
	if errors.Is(err, os.ErrNotExist) {
		return "missing"
	}
	if err != nil {
		return "unavailable"
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "wrong_type"
	}
	if info.Mode().Perm() != 0600 {
		return "wrong_mode"
	}
	if info.Size() < 1 {
		return "empty"
	}
	if info.Size() > 16*1024 {
		return "oversized"
	}
	return "valid"
}

func validGatewayOrigin(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Path == "" &&
		parsed.RawQuery == "" && parsed.Fragment == "" && parsed.Hostname() != ""
}

func workspaceMountSpec(config guestConfig) workspaceagent.WorkspaceMountSpec {
	return workspaceagent.WorkspaceMountSpec{TenantID: config.TenantID, WorkspaceID: config.WorkspaceID,
		ResourcePlaneID: config.ResourcePlaneID, MountPath: localWorkspacePath, UID: localWorkspaceUID,
		GID: localWorkspaceUID, LocalDisk: true}
}

func mountGuestPseudoFilesystems() error {
	return mountGuestPseudoFilesystemsWith(os.MkdirAll, syscall.Mount, os.Remove, os.Symlink)
}

func mountGuestPseudoFilesystemsWith(mkdirAll func(string, os.FileMode) error,
	mount func(string, string, string, uintptr, string) error,
	remove func(string) error, symlink func(string, string) error) error {
	if mkdirAll == nil || mount == nil || remove == nil || symlink == nil {
		return errors.New("Guest pseudo-filesystem dependencies are required")
	}
	for _, path := range []string{"/dev", "/proc", "/sys", localWorkspacePath} {
		if err := mkdirAll(path, 0755); err != nil {
			return errors.New("prepare local Workspace Guest mountpoint")
		}
	}
	if err := mount("devtmpfs", "/dev", "devtmpfs", syscall.MS_NOSUID, "mode=0755"); err != nil {
		return errors.New("mount Guest devtmpfs")
	}
	// devtmpfs covers the initrd's original /dev tree, so create this mount
	// point after mounting devtmpfs rather than before it.
	if err := mkdirAll("/dev/pts", 0755); err != nil {
		return errors.New("prepare Guest devpts mountpoint")
	}
	if err := mount("devpts", "/dev/pts", "devpts", syscall.MS_NOSUID|syscall.MS_NOEXEC,
		"newinstance,ptmxmode=0666,mode=0620,gid=5"); err != nil {
		return errors.New("mount Guest devpts")
	}
	if err := configureGuestPTYDevice(remove, symlink); err != nil {
		return err
	}
	if err := mount("proc", "/proc", "proc", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return errors.New("mount Guest procfs")
	}
	if err := mount("sysfs", "/sys", "sysfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return errors.New("mount Guest sysfs")
	}
	return nil
}

func configureGuestPTYDevice(remove func(string) error, symlink func(string, string) error) error {
	if remove == nil || symlink == nil {
		return errors.New("Guest PTY device dependencies are required")
	}
	if err := remove("/dev/ptmx"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("replace Guest PTY multiplexer")
	}
	if err := symlink("pts/ptmx", "/dev/ptmx"); err != nil {
		return errors.New("link Guest PTY multiplexer to its private devpts instance")
	}
	return nil
}

func mountWorkspaceDisk() error {
	info, err := os.Lstat(localWorkspaceDevice)
	if err != nil || info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return errors.New("local Workspace virtio disk is unavailable")
	}
	pathInfo, err := os.Lstat(localWorkspacePath)
	if err != nil || !pathInfo.IsDir() || pathInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("local Workspace mount path is unsafe")
	}
	if err := syscall.Mount(localWorkspaceDevice, localWorkspacePath, "ext4", syscall.MS_NODEV|syscall.MS_NOSUID, ""); err != nil {
		return errors.New("mount persistent local Workspace disk")
	}
	if err := os.Chown(localWorkspacePath, int(localWorkspaceUID), int(localWorkspaceUID)); err != nil {
		_ = syscall.Unmount(localWorkspacePath, 0)
		return errors.New("set local Workspace owner")
	}
	return nil
}
