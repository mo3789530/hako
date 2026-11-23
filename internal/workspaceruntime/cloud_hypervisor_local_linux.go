//go:build linux

package workspaceruntime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	"github.com/mo3789530/hako/internal/workspacejournal"
	"golang.org/x/sys/unix"
)

const (
	localCloudHypervisorAPIPath        = "/api.sock"
	localCloudHypervisorPIDPath        = "/cloud-hypervisor-process.json"
	localCloudHypervisorLogPath        = "/cloud-hypervisor.log"
	localCloudHypervisorLaunchSpecPath = "/launch-spec.json"
	localCloudHypervisorVsockPath      = "/vsock.sock"
	localWorkspaceDiskPath             = "/workspace.ext4"
	localGuestQuiescedPath             = "/guest-quiesced.json"
	localGuestQuiescedMarker           = "HAKO_LOCAL_GUEST_QUIESCENT_V1"
	localGuestAgentVsockPort           = uint32(7000)
	localGuestControlVsockPort         = uint32(7001)
)

var localAllocationIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var localImageDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// CloudHypervisorLocalDriverConfig selects host-owned executable and storage
// locations. The API socket, PID record, quiesce record, and Workspace disk are
// always derived from the journal's opaque allocation ID.
type CloudHypervisorLocalDriverConfig struct {
	BinaryPath     string
	StateDirectory string
	MkfsExt4Path   string
	StartupTimeout time.Duration
	StopTimeout    time.Duration
}

type CloudHypervisorLocalDriver struct {
	binaryPath string
	stateDir   string
	mkfsPath   string
	startup    time.Duration
	stop       time.Duration
	mu         sync.Mutex
	processes  map[string]*cloudHypervisorProcess
}

type CloudHypervisorLocalLaunchSpec struct {
	KernelImagePath      string `json:"kernel_image_path"`
	KernelImageSHA256    string `json:"kernel_image_sha256"`
	InitrdPath           string `json:"initrd_path"`
	InitrdSHA256         string `json:"initrd_sha256"`
	KernelBootArgs       string `json:"kernel_boot_args,omitempty"`
	VCPUCount            int    `json:"vcpu_count,omitempty"`
	MemoryMiB            int    `json:"memory_mib,omitempty"`
	WorkspaceDiskSizeMiB int64  `json:"workspace_disk_size_mib,omitempty"`
}

type cloudHypervisorProcessRecord struct {
	PID        int    `json:"pid"`
	StartTicks uint64 `json:"start_ticks"`
	BootID     string `json:"boot_id"`
}

type cloudHypervisorGuestQuiescedRecord struct {
	Version       int    `json:"version"`
	AllocationID  string `json:"allocation_id"`
	LaunchSpecSHA string `json:"launch_spec_sha256"`
	WorkspaceDisk string `json:"workspace_disk"`
}

type cloudHypervisorProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
}

type StaticCloudHypervisorLocalLaunchResolver struct{ payload []byte }

func NewStaticCloudHypervisorLocalLaunchResolver(payload []byte) (*StaticCloudHypervisorLocalLaunchResolver, error) {
	spec, err := decodeCloudHypervisorLaunchSpec(payload)
	if err != nil {
		return nil, err
	}
	if err := prepareCloudHypervisorLaunchSpec(&spec); err != nil {
		return nil, err
	}
	return &StaticCloudHypervisorLocalLaunchResolver{payload: append([]byte(nil), payload...)}, nil
}

func (r *StaticCloudHypervisorLocalLaunchResolver) ResolveLocal(ctx context.Context, command resourcecontroller.Command) ([]byte, error) {
	if r == nil || ctx == nil || ctx.Err() != nil || command.TenantID == "" || command.WorkspaceID == "" || command.ResourcePlaneID == "" ||
		command.Type != domain.OperationEnsureRunning {
		return nil, ErrSpecification
	}
	return append([]byte(nil), r.payload...), nil
}

func NewCloudHypervisorLocalDriver(config CloudHypervisorLocalDriverConfig) (*CloudHypervisorLocalDriver, error) {
	if runtime.GOOS != "linux" || !filepath.IsAbs(config.BinaryPath) || filepath.Clean(config.BinaryPath) != config.BinaryPath ||
		!filepath.IsAbs(config.StateDirectory) || filepath.Clean(config.StateDirectory) != config.StateDirectory {
		return nil, errors.New("local CloudHypervisor binary and state directory must be absolute clean Linux paths")
	}
	kvmFD, err := unix.Open("/dev/kvm", unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("local CloudHypervisor requires readable and writable /dev/kvm")
	}
	apiVersion, ioctlErr := unix.IoctlRetInt(kvmFD, 0xAE00) // KVM_GET_API_VERSION returns the version as ioctl's result.
	_ = unix.Close(kvmFD)
	if ioctlErr != nil || apiVersion != 12 {
		return nil, errors.New("local CloudHypervisor host KVM API is unavailable or unsupported")
	}
	binary, err := filepath.EvalSymlinks(config.BinaryPath)
	if err != nil {
		return nil, errors.New("resolve local CloudHypervisor binary")
	}
	binaryInfo, err := os.Stat(binary)
	if err != nil || !binaryInfo.Mode().IsRegular() || binaryInfo.Mode().Perm()&0111 == 0 {
		return nil, errors.New("local CloudHypervisor binary must be an executable regular file")
	}
	if err := os.MkdirAll(config.StateDirectory, 0700); err != nil {
		return nil, errors.New("create local CloudHypervisor state directory")
	}
	rootInfo, err := os.Lstat(config.StateDirectory)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("local CloudHypervisor state directory cannot be a symlink")
	}
	rootStat, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok || rootStat.Uid != uint32(os.Getuid()) {
		return nil, errors.New("local CloudHypervisor state directory must be owned by the current user")
	}
	if err := os.Chmod(config.StateDirectory, 0700); err != nil {
		return nil, errors.New("restrict local CloudHypervisor state directory")
	}
	stateDir, err := filepath.EvalSymlinks(config.StateDirectory)
	if err != nil {
		return nil, errors.New("resolve local CloudHypervisor state directory")
	}
	// LocalMicroVM provider IDs are 35 characters; allow small direct-driver
	// IDs too while leaving room for the socket suffix under Linux sun_path.
	if len(filepath.Join(stateDir, strings.Repeat("x", 40), "api.sock")) >= 104 ||
		len(filepath.Join(stateDir, strings.Repeat("x", 40), strings.TrimPrefix(localCloudHypervisorVsockPath, "/"))) >= 104 {
		return nil, errors.New("local CloudHypervisor state path is too long for Unix API sockets")
	}
	mkfsPath := config.MkfsExt4Path
	if mkfsPath == "" {
		mkfsPath, err = exec.LookPath("mkfs.ext4")
		if err != nil {
			return nil, errors.New("mkfs.ext4 is required for local Workspace disks")
		}
	}
	if !filepath.IsAbs(mkfsPath) {
		mkfsPath, err = exec.LookPath(mkfsPath)
		if err != nil {
			return nil, errors.New("resolve mkfs.ext4")
		}
	}
	if config.StartupTimeout == 0 {
		config.StartupTimeout = 2 * time.Minute
	}
	if config.StopTimeout == 0 {
		config.StopTimeout = 15 * time.Second
	}
	if config.StartupTimeout < 100*time.Millisecond || config.StartupTimeout > 3*time.Minute ||
		config.StopTimeout < 100*time.Millisecond || config.StopTimeout > 2*time.Minute {
		return nil, errors.New("local CloudHypervisor startup and stop timeouts are out of bounds")
	}
	return &CloudHypervisorLocalDriver{binaryPath: binary, stateDir: stateDir, mkfsPath: mkfsPath,
		startup: config.StartupTimeout, stop: config.StopTimeout, processes: map[string]*cloudHypervisorProcess{}}, nil
}

func (d *CloudHypervisorLocalDriver) EnsureRunning(ctx context.Context, allocationID string, payload []byte) error {
	spec, err := decodeCloudHypervisorLaunchSpec(payload)
	if err != nil {
		return err
	}
	unlock, dir, err := d.lockAllocation(ctx, allocationID, true)
	if err != nil {
		return err
	}
	defer unlock()
	if err := prepareCloudHypervisorLaunchSpec(&spec); err != nil {
		return err
	}
	if err := pinCloudHypervisorLaunchSpec(dir, payload); err != nil {
		return err
	}
	if err := d.ensureWorkspaceDisk(ctx, dir, spec.WorkspaceDiskSizeMiB); err != nil {
		return err
	}
	state, apiErr := d.vmState(ctx, dir)
	if apiErr == nil {
		if state == "Running" {
			if !d.hasGuestQuiesced(dir, allocationID) {
				return d.waitForGuestReadyOrStop(ctx, allocationID, dir, spec)
			}
			if err := d.stopProcess(ctx, allocationID, dir); err != nil {
				return err
			}
		}
		if state == "Paused" || state == "Shutdown" {
			if !d.hasGuestQuiesced(dir, allocationID) {
				return errors.New("refusing to replace a paused or shut down Cloud Hypervisor VM without guest filesystem quiescence")
			}
			if err := d.stopProcess(ctx, allocationID, dir); err != nil {
				return err
			}
		} else if state == "Created" {
			if err := d.removeGuestQuiesced(dir); err != nil {
				return err
			}
			if err := d.stopProcess(ctx, allocationID, dir); err != nil {
				return err
			}
		} else if state != "Running" {
			return fmt.Errorf("local Cloud Hypervisor returned unsupported VM state %q", state)
		}
	}
	alive, processErr := d.processAlive(dir)
	if processErr != nil {
		return processErr
	}
	if alive {
		return errors.New("local CloudHypervisor API is unavailable while its process is still live")
	}
	if err := d.removeStaleProcess(ctx, allocationID, dir); err != nil {
		return err
	}
	if err := d.removeSocket(dir); err != nil {
		return err
	}
	if err := d.removeGuestQuiesced(dir); err != nil {
		return err
	}
	if err := d.startProcess(allocationID, dir, spec); err != nil {
		return err
	}
	if err := d.waitForAPI(ctx, dir); err != nil {
		_ = d.stopProcess(context.Background(), allocationID, dir)
		return err
	}
	return d.waitForGuestReadyOrStop(ctx, allocationID, dir, spec)
}

func (d *CloudHypervisorLocalDriver) Suspend(ctx context.Context, allocationID string) error {
	unlock, dir, err := d.lockAllocation(ctx, allocationID, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer unlock()
	state, stateErr := d.vmState(ctx, dir)
	if stateErr != nil {
		alive, processErr := d.processAlive(dir)
		if processErr != nil {
			return processErr
		}
		if alive {
			return errors.New("local Cloud Hypervisor API is unavailable while its process is still live")
		}
		return nil
	}
	if state != "Running" && state != "Paused" {
		return fmt.Errorf("cannot suspend local CloudHypervisor VM in state %q", state)
	}
	if !d.hasGuestQuiesced(dir, allocationID) {
		logOffset, err := cloudHypervisorLogOffset(dir)
		if err != nil {
			return err
		}
		if err := requestLocalGuestShutdown(ctx, dir); err != nil {
			// Keep compatibility with older local Guest initrd versions. A new
			// Guest handles this request in PID 1 and confirms disk unmount; the
			// legacy path still waits for CloudHypervisor's orderly reboot/exit.
			if apiErr := d.api(ctx, dir, http.MethodPut, "/api/v1/vm.power-button", nil, nil); apiErr != nil {
				return errors.Join(err, apiErr)
			}
		}
		if err := d.waitForGuestQuiescence(ctx, dir, logOffset); err != nil {
			return err
		}
		if err := d.writeGuestQuiescedRecord(dir, allocationID); err != nil {
			return err
		}
	}
	return d.stopProcess(ctx, allocationID, dir)
}

func (d *CloudHypervisorLocalDriver) Resume(ctx context.Context, allocationID string, payload []byte) error {
	spec, err := decodeCloudHypervisorLaunchSpec(payload)
	if err != nil {
		return err
	}
	if err := prepareCloudHypervisorLaunchSpec(&spec); err != nil {
		return err
	}
	unlock, dir, err := d.lockAllocation(ctx, allocationID, false)
	if err != nil {
		return err
	}
	defer unlock()
	if err := pinCloudHypervisorLaunchSpec(dir, payload); err != nil {
		return err
	}
	state, stateErr := d.vmState(ctx, dir)
	if stateErr == nil {
		if state == "Running" && !d.hasGuestQuiesced(dir, allocationID) {
			return nil
		}
		if (state != "Running" && state != "Paused" && state != "Created" && state != "Shutdown") ||
			((state == "Paused" || state == "Shutdown") && !d.hasGuestQuiesced(dir, allocationID)) {
			return fmt.Errorf("cannot cold-resume local CloudHypervisor VM in state %q without guest quiescence", state)
		}
		if err := d.stopProcess(ctx, allocationID, dir); err != nil {
			return err
		}
	} else {
		alive, processErr := d.processAlive(dir)
		if processErr != nil {
			return processErr
		}
		if alive {
			return errors.New("local CloudHypervisor API is unavailable while its process is still live")
		}
		if err := d.removeStaleProcess(ctx, allocationID, dir); err != nil {
			return err
		}
	}
	if err := d.ensureWorkspaceDisk(ctx, dir, spec.WorkspaceDiskSizeMiB); err != nil {
		return err
	}
	if err := d.removeSocket(dir); err != nil {
		return err
	}
	if err := d.removeGuestQuiesced(dir); err != nil {
		return err
	}
	if err := d.startProcess(allocationID, dir, spec); err != nil {
		return err
	}
	if err := d.waitForAPI(ctx, dir); err != nil {
		_ = d.stopProcess(context.Background(), allocationID, dir)
		return err
	}
	return d.waitForGuestReadyOrStop(ctx, allocationID, dir, spec)
}

type localGuestIdentity struct {
	tenantID        string
	workspaceID     string
	resourcePlaneID string
	microVMID       string
	gatewayOrigin   string
}

func localGuestIdentityFromKernelArgs(kernelArgs string) (localGuestIdentity, error) {
	var identity localGuestIdentity
	seen := make(map[string]bool, 5)
	for _, field := range strings.Fields(kernelArgs) {
		if !strings.HasPrefix(field, "hako.") {
			continue
		}
		key, value, found := strings.Cut(field, "=")
		if !found || value == "" || seen[key] {
			return localGuestIdentity{}, errors.New("local Workspace Guest identity is invalid")
		}
		seen[key] = true
		switch key {
		case "hako.tenant_id":
			identity.tenantID = value
		case "hako.workspace_id":
			identity.workspaceID = value
		case "hako.resource_plane_id":
			identity.resourcePlaneID = value
		case "hako.microvm_id":
			identity.microVMID = value
		case "hako.gateway_origin":
			identity.gatewayOrigin = value
		default:
			return localGuestIdentity{}, errors.New("local Workspace Guest identity is invalid")
		}
	}
	if !localAllocationIDPattern.MatchString(identity.tenantID) || !localAllocationIDPattern.MatchString(identity.workspaceID) ||
		!localAllocationIDPattern.MatchString(identity.resourcePlaneID) || !cloudHypervisorWorkspaceProviderIDPattern.MatchString(identity.microVMID) ||
		!validLocalGatewayOrigin(identity.gatewayOrigin) {
		return localGuestIdentity{}, errors.New("local Workspace Guest identity is invalid")
	}
	return identity, nil
}

func (d *CloudHypervisorLocalDriver) waitForGuestReady(ctx context.Context, dir string, spec CloudHypervisorLocalLaunchSpec) error {
	identity, err := localGuestIdentityFromKernelArgs(spec.KernelBootArgs)
	if err != nil {
		return err
	}
	readyCtx, cancel := context.WithTimeout(ctx, d.startup)
	defer cancel()
	var lastDialErr error
	lastHeartbeatFailure := ""
	for {
		if stage := localGuestFailureStage(dir); stage != "" {
			return fmt.Errorf("local Workspace Guest initialization failed at %s", stage)
		}
		dialCtx, dialCancel := context.WithTimeout(readyCtx, time.Second)
		conn, dialErr := DialCloudHypervisorLocalGuest(dialCtx, dir)
		dialCancel()
		if dialErr == nil {
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			_, requestErr := io.WriteString(conn, "GET /internal/heartbeat HTTP/1.1\r\nHost: workspace-agent.vsock\r\nConnection: close\r\n\r\n")
			if requestErr == nil {
				response, readErr := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
				if readErr == nil {
					var heartbeat struct {
						ProtocolVersion int    `json:"protocol_version"`
						TenantID        string `json:"tenant_id"`
						WorkspaceID     string `json:"workspace_id"`
						ResourcePlaneID string `json:"resource_plane_id"`
						MicroVMID       string `json:"microvm_id"`
					}
					decoder := json.NewDecoder(io.LimitReader(response.Body, 4097))
					decoder.DisallowUnknownFields()
					decodeErr := decoder.Decode(&heartbeat)
					if decodeErr == nil && !errors.Is(decoder.Decode(new(any)), io.EOF) {
						decodeErr = errors.New("invalid heartbeat response")
					}
					_ = response.Body.Close()
					if decodeErr == nil && response.StatusCode == http.StatusOK && heartbeat.ProtocolVersion == 1 &&
						heartbeat.TenantID == identity.tenantID && heartbeat.WorkspaceID == identity.workspaceID &&
						heartbeat.ResourcePlaneID == identity.resourcePlaneID && heartbeat.MicroVMID == identity.microVMID {
						_ = conn.Close()
						return nil
					}
					lastHeartbeatFailure = "heartbeat_identity_mismatch"
					if decodeErr != nil {
						lastHeartbeatFailure = "heartbeat_invalid"
					} else if response.StatusCode != http.StatusOK {
						lastHeartbeatFailure = "heartbeat_status"
					}
				} else {
					lastHeartbeatFailure = "heartbeat_unavailable"
					if errors.Is(readErr, io.EOF) {
						lastHeartbeatFailure = "heartbeat_eof"
					} else if errors.Is(readErr, os.ErrDeadlineExceeded) {
						lastHeartbeatFailure = "heartbeat_timeout"
					} else {
						lastHeartbeatFailure = "heartbeat_read_error"
					}
				}
			} else {
				lastHeartbeatFailure = "heartbeat_request_failed"
			}
			_ = conn.Close()
		} else {
			lastDialErr = dialErr
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-readyCtx.Done():
			timer.Stop()
			if lastDialErr != nil {
				return fmt.Errorf("local Workspace Guest did not become ready with the pinned identity: %w", lastDialErr)
			}
			if lastHeartbeatFailure != "" {
				return fmt.Errorf("local Workspace Guest readiness failed (%s)", lastHeartbeatFailure)
			}
			return errors.New("local Workspace Guest did not become ready with the pinned identity")
		case <-timer.C:
		}
	}
}

func localGuestFailureStage(dir string) string {
	logPath := filepath.Join(dir, "cloud-hypervisor.log")
	info, err := os.Stat(logPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > 32<<20 {
		return ""
	}
	contents, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	for _, stage := range []string{"kernel_arguments", "workspace_identity", "workspace_disk", "agent_configuration",
		"agent_start_ssh_config", "agent_start_ssh_paths", "agent_start_ssh_identity", "agent_start_ssh_directories",
		"agent_start_ssh_host_key", "agent_start_ssh_key_open", "agent_start_ssh_key_stat", "agent_start_ssh_key_type",
		"agent_start_ssh_key_permissions", "agent_start_ssh_key_empty", "agent_start_ssh_key_oversized", "agent_start_ssh_key_contents",
		"agent_start_ssh_read_key", "agent_start_ssh_runtime_key", "agent_start_ssh_write_config",
		"agent_start_ssh_validate_config", "agent_start_ssh_ready", "agent_start_ssh_process", "agent_start_ssh",
		"agent_start_readiness", "agent_start_child", "agent_start", "agent_stop", "workspace_unmount"} {
		if strings.Contains(string(contents), `Hako local Workspace Guest stopped (failure_class=guest_init stage="`+stage+`")`) {
			return stage
		}
	}
	return ""
}

func (d *CloudHypervisorLocalDriver) waitForGuestReadyOrStop(ctx context.Context, allocationID, dir string, spec CloudHypervisorLocalLaunchSpec) error {
	if err := d.waitForGuestReady(ctx, dir, spec); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), d.stop)
		_ = d.stopProcess(cleanupCtx, allocationID, dir)
		cancel()
		return err
	}
	return nil
}

func (d *CloudHypervisorLocalDriver) Delete(ctx context.Context, allocationID string) error {
	unlock, dir, err := d.lockAllocation(ctx, allocationID, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer unlock()
	if _, err := d.vmState(ctx, dir); err == nil {
		if err := d.stopProcess(ctx, allocationID, dir); err != nil {
			return err
		}
	} else {
		alive, processErr := d.processAlive(dir)
		if processErr != nil {
			return processErr
		}
		if alive {
			if err := d.stopProcess(ctx, allocationID, dir); err != nil {
				return err
			}
		} else if err := d.removeStaleProcess(ctx, allocationID, dir); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return errors.New("remove local CloudHypervisor allocation")
	}
	return syncLocalDirectory(d.stateDir)
}

func decodeCloudHypervisorLaunchSpec(payload []byte) (CloudHypervisorLocalLaunchSpec, error) {
	if len(payload) == 0 || len(payload) > 16<<10 {
		return CloudHypervisorLocalLaunchSpec{}, errors.New("local CloudHypervisor launch specification size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var spec CloudHypervisorLocalLaunchSpec
	if err := decoder.Decode(&spec); err != nil {
		return CloudHypervisorLocalLaunchSpec{}, errors.New("decode local CloudHypervisor launch specification")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return CloudHypervisorLocalLaunchSpec{}, errors.New("local CloudHypervisor launch specification must contain one JSON object")
	}
	return spec, nil
}

func prepareCloudHypervisorLaunchSpec(spec *CloudHypervisorLocalLaunchSpec) error {
	if spec == nil {
		return errors.New("local CloudHypervisor launch specification is required")
	}
	for name, item := range map[string]struct {
		path   *string
		digest string
	}{
		"kernel": {path: &spec.KernelImagePath, digest: spec.KernelImageSHA256},
		"initrd": {path: &spec.InitrdPath, digest: spec.InitrdSHA256},
	} {
		path := item.path
		if *path == "" || !filepath.IsAbs(*path) || filepath.Clean(*path) != *path {
			return fmt.Errorf("local CloudHypervisor %s image path must be absolute and clean", name)
		}
		resolved, err := filepath.EvalSymlinks(*path)
		if err != nil || resolved != *path {
			return fmt.Errorf("resolve local CloudHypervisor %s image", name)
		}
		info, err := os.Lstat(resolved)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 {
			return fmt.Errorf("local CloudHypervisor %s image must be a regular file", name)
		}
		if !localImageDigestPattern.MatchString(item.digest) {
			return fmt.Errorf("local CloudHypervisor %s image SHA-256 is required", name)
		}
		actual, err := fileSHA256(resolved)
		if err != nil || actual != item.digest {
			return fmt.Errorf("local CloudHypervisor %s image digest does not match its pin", name)
		}
	}
	if spec.VCPUCount == 0 {
		spec.VCPUCount = 1
	}
	if spec.MemoryMiB == 0 {
		spec.MemoryMiB = 512
	}
	if spec.WorkspaceDiskSizeMiB == 0 {
		spec.WorkspaceDiskSizeMiB = 1024
	}
	if spec.VCPUCount != 1 && (spec.VCPUCount < 2 || spec.VCPUCount%2 != 0 || spec.VCPUCount > 32) {
		return errors.New("local CloudHypervisor vCPU count must be 1 or an even value up to 32")
	}
	if spec.MemoryMiB < 128 || spec.MemoryMiB > 65536 || spec.WorkspaceDiskSizeMiB < 64 || spec.WorkspaceDiskSizeMiB > 1<<20 {
		return errors.New("local CloudHypervisor memory or Workspace disk size is out of bounds")
	}
	if spec.KernelBootArgs == "" {
		spec.KernelBootArgs = "console=ttyS0 panic=1 reboot=k rdinit=/init"
	}
	if len(spec.KernelBootArgs) > 2048 || strings.ContainsAny(spec.KernelBootArgs, "\r\n\x00") {
		return errors.New("local CloudHypervisor kernel boot arguments are invalid")
	}
	return nil
}

func (d *CloudHypervisorLocalDriver) lockAllocation(ctx context.Context, allocationID string, create bool) (func(), string, error) {
	if d == nil || ctx == nil || !localAllocationIDPattern.MatchString(allocationID) {
		return nil, "", errors.New("local CloudHypervisor allocation identity is invalid")
	}
	dir := filepath.Join(d.stateDir, allocationID)
	if create {
		if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, "", errors.New("create local CloudHypervisor allocation directory")
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || stat.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0077 != 0 {
		return nil, "", errors.New("local CloudHypervisor allocation directory is unsafe")
	}
	lockPath := filepath.Join(dir, ".driver.lock")
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, "", errors.New("open local CloudHypervisor allocation lock")
	}
	var lockStat unix.Stat_t
	if err := unix.Fstat(fd, &lockStat); err != nil || lockStat.Mode&unix.S_IFMT != unix.S_IFREG || lockStat.Mode&0077 != 0 || lockStat.Uid != uint32(os.Getuid()) {
		_ = unix.Close(fd)
		return nil, "", errors.New("local CloudHypervisor allocation lock is unsafe")
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = unix.Close(fd)
			return nil, "", err
		}
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = unix.Close(fd) }, dir, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			_ = unix.Close(fd)
			return nil, "", errors.New("lock local CloudHypervisor allocation")
		}
		select {
		case <-ctx.Done():
			_ = unix.Close(fd)
			return nil, "", ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (d *CloudHypervisorLocalDriver) ensureWorkspaceDisk(ctx context.Context, dir string, sizeMiB int64) error {
	path := filepath.Join(dir, strings.TrimPrefix(localWorkspaceDiskPath, "/"))
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return errors.New("open existing local Workspace disk")
		}
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0077 != 0 || stat.Uid != uint32(os.Getuid()) || stat.Size != sizeMiB*(1<<20) {
			_ = unix.Close(fd)
			return errors.New("existing local Workspace disk has invalid owner, type, mode, or size")
		}
		if err := verifyExt4WorkspaceDisk(fd); err != nil {
			_ = unix.Close(fd)
			return err
		}
		return unix.Close(fd)
	}
	if err != nil {
		return errors.New("create local Workspace disk")
	}
	if err := unix.Ftruncate(fd, sizeMiB*(1<<20)); err != nil {
		_ = unix.Close(fd)
		_ = os.Remove(path)
		return errors.New("size local Workspace disk")
	}
	if err := unix.Fsync(fd); err != nil {
		_ = unix.Close(fd)
		_ = os.Remove(path)
		return errors.New("sync local Workspace disk")
	}
	if err := unix.Close(fd); err != nil {
		return errors.New("close local Workspace disk")
	}
	command := exec.CommandContext(ctx, d.mkfsPath, "-q", "-F", "-L", "HAKO_WORKSPACE", path)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	if output, err := command.CombinedOutput(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("format local Workspace disk (mkfs.ext4 exit %v, output bytes=%d)", err, len(output))
	}
	fd, err = unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("reopen formatted local Workspace disk")
	}
	if err := unix.Fsync(fd); err != nil {
		_ = unix.Close(fd)
		return errors.New("sync formatted local Workspace disk")
	}
	if err := unix.Close(fd); err != nil {
		return errors.New("close formatted local Workspace disk")
	}
	fd, err = unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("reopen formatted local Workspace disk")
	}
	if err := verifyExt4WorkspaceDisk(fd); err != nil {
		_ = unix.Close(fd)
		return err
	}
	if err := unix.Close(fd); err != nil {
		return errors.New("close verified local Workspace disk")
	}
	if err := syncLocalDirectory(dir); err != nil {
		return err
	}
	return nil
}

func verifyExt4WorkspaceDisk(fd int) error {
	var superblock [1024]byte
	n, err := unix.Pread(fd, superblock[:], 1024)
	if err != nil || n != len(superblock) || superblock[56] != 0x53 || superblock[57] != 0xef ||
		strings.TrimRight(string(superblock[120:136]), "\x00") != "HAKO_WORKSPACE" {
		return errors.New("local Workspace disk is not the expected ext4 filesystem")
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("local CloudHypervisor image is not a regular file")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (d *CloudHypervisorLocalDriver) vmState(ctx context.Context, dir string) (string, error) {
	var response struct {
		State string `json:"state"`
	}
	if err := d.api(ctx, dir, http.MethodGet, "/api/v1/vm.info", nil, &response); err != nil {
		return "", err
	}
	if response.State == "" {
		return "", errors.New("local CloudHypervisor returned an empty VM state")
	}
	return response.State, nil
}

func (d *CloudHypervisorLocalDriver) api(ctx context.Context, dir, method, path string, request any, response any) error {
	var body io.Reader
	if request != nil {
		encoded, err := json.Marshal(request)
		if err != nil {
			return errors.New("encode local CloudHypervisor API request")
		}
		body = bytes.NewReader(encoded)
	}
	socket := filepath.Join(dir, strings.TrimPrefix(localCloudHypervisorAPIPath, "/"))
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socket)
	}}
	client := &http.Client{Transport: transport}
	defer transport.CloseIdleConnections()
	requestHTTP, err := http.NewRequestWithContext(ctx, method, "http://cloud-hypervisor"+path, body)
	if err != nil {
		return errors.New("create local CloudHypervisor API request")
	}
	if request != nil {
		requestHTTP.Header.Set("Content-Type", "application/json")
	}
	result, err := client.Do(requestHTTP)
	if err != nil {
		return fmt.Errorf("local CloudHypervisor API %s %s failed", method, path)
	}
	defer result.Body.Close()
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(result.Body, 1024))
		return fmt.Errorf("local CloudHypervisor API %s %s returned HTTP %d", method, path, result.StatusCode)
	}
	if response != nil {
		if err := json.NewDecoder(io.LimitReader(result.Body, 8<<10)).Decode(response); err != nil {
			return errors.New("decode local CloudHypervisor API response")
		}
	}
	return nil
}

func (d *CloudHypervisorLocalDriver) waitForAPI(ctx context.Context, dir string) error {
	deadline, cancel := context.WithTimeout(ctx, d.startup)
	defer cancel()
	for {
		var response map[string]any
		if err := d.api(deadline, dir, http.MethodGet, "/api/v1/vmm.ping", nil, &response); err == nil {
			return nil
		}
		select {
		case <-deadline.Done():
			return errors.New("local Cloud Hypervisor API did not become ready before timeout")
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (d *CloudHypervisorLocalDriver) startProcess(allocationID, dir string, spec CloudHypervisorLocalLaunchSpec) error {
	apiSocket := filepath.Join(dir, strings.TrimPrefix(localCloudHypervisorAPIPath, "/"))
	if len(apiSocket) >= 104 {
		return errors.New("local CloudHypervisor API socket path is too long")
	}
	logFile, err := os.OpenFile(filepath.Join(dir, strings.TrimPrefix(localCloudHypervisorLogPath, "/")), os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("open local CloudHypervisor log")
	}
	// A tiny constant shell wrapper sets umask before exec; no user value is
	// interpolated into shell source.
	diskPath := filepath.Join(dir, strings.TrimPrefix(localWorkspaceDiskPath, "/"))
	vsockPath := filepath.Join(dir, strings.TrimPrefix(localCloudHypervisorVsockPath, "/"))
	if len(vsockPath) >= 104 {
		_ = logFile.Close()
		return errors.New("local Cloud Hypervisor Guest vsock path is too long")
	}
	args := cloudHypervisorArgs(apiSocket, diskPath, vsockPath, spec)
	commandArgs := append([]string{d.binaryPath}, args...)
	command := exec.Command("/bin/sh", append([]string{"-c", "umask 077; exec \"$@\"", "hako-cloud-hypervisor"}, commandArgs...)...)
	command.Stdout, command.Stderr = logFile, logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		return errors.New("start local CloudHypervisor process")
	}
	_ = logFile.Close()
	proc := &cloudHypervisorProcess{cmd: command, done: make(chan struct{})}
	d.mu.Lock()
	d.processes[allocationID] = proc
	d.mu.Unlock()
	go func() {
		_ = command.Wait()
		close(proc.done)
		d.mu.Lock()
		if d.processes[allocationID] == proc {
			delete(d.processes, allocationID)
		}
		d.mu.Unlock()
	}()
	record, err := readCloudHypervisorProcessRecord(command.Process.Pid)
	if err != nil {
		_ = command.Process.Signal(syscall.SIGTERM)
		return errors.New("record local CloudHypervisor process identity")
	}
	return writeJSONAtomic(filepath.Join(dir, strings.TrimPrefix(localCloudHypervisorPIDPath, "/")), record, 0600)
}

func cloudHypervisorArgs(apiSocket, diskPath, vsockPath string, spec CloudHypervisorLocalLaunchSpec) []string {
	return []string{"--api-socket", "path=" + apiSocket,
		"--kernel", spec.KernelImagePath,
		"--initramfs", spec.InitrdPath,
		"--cmdline", spec.KernelBootArgs,
		"--cpus", fmt.Sprintf("boot=%d,max=%d", spec.VCPUCount, spec.VCPUCount),
		"--memory", fmt.Sprintf("size=%dM", spec.MemoryMiB),
		"--disk", "path=" + diskPath + ",image_type=raw",
		"--vsock", "cid=3,socket=" + vsockPath,
		"--serial", "tty"}
}

func (d *CloudHypervisorLocalDriver) removeStaleProcess(ctx context.Context, allocationID, dir string) error {
	record, err := readJSON[cloudHypervisorProcessRecord](filepath.Join(dir, strings.TrimPrefix(localCloudHypervisorPIDPath, "/")))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("read local CloudHypervisor process identity")
	}
	alive, valid := processMatches(record, d.binaryPath, filepath.Join(dir, strings.TrimPrefix(localCloudHypervisorAPIPath, "/")))
	if !valid {
		if alive {
			return errors.New("local CloudHypervisor PID belongs to an unexpected process; refusing to signal it")
		}
		return os.Remove(filepath.Join(dir, strings.TrimPrefix(localCloudHypervisorPIDPath, "/")))
	}
	return errors.New("local CloudHypervisor process is live but its API is unavailable")
}

func (d *CloudHypervisorLocalDriver) processAlive(dir string) (bool, error) {
	path := filepath.Join(dir, strings.TrimPrefix(localCloudHypervisorPIDPath, "/"))
	record, err := readJSON[cloudHypervisorProcessRecord](path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("read local CloudHypervisor process identity")
	}
	alive, valid := processMatches(record, d.binaryPath, filepath.Join(dir, strings.TrimPrefix(localCloudHypervisorAPIPath, "/")))
	if alive && !valid {
		return false, errors.New("local CloudHypervisor PID belongs to an unexpected process")
	}
	if !alive {
		_ = os.Remove(path)
	}
	return alive, nil
}

func (d *CloudHypervisorLocalDriver) stopProcess(ctx context.Context, allocationID, dir string) error {
	path := filepath.Join(dir, strings.TrimPrefix(localCloudHypervisorPIDPath, "/"))
	record, err := readJSON[cloudHypervisorProcessRecord](path)
	if errors.Is(err, os.ErrNotExist) {
		return d.removeSocket(dir)
	}
	if err != nil {
		return errors.New("read local CloudHypervisor process identity")
	}
	apiSocket := filepath.Join(dir, strings.TrimPrefix(localCloudHypervisorAPIPath, "/"))
	alive, valid := processMatches(record, d.binaryPath, apiSocket)
	if !valid {
		if alive {
			return errors.New("local CloudHypervisor PID identity changed; refusing to signal it")
		}
		_ = os.Remove(path)
		return d.removeSocket(dir)
	}
	d.mu.Lock()
	proc := d.processes[allocationID]
	d.mu.Unlock()
	process, err := os.FindProcess(record.PID)
	if err != nil {
		return errors.New("find local CloudHypervisor process")
	}
	if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return errors.New("stop local CloudHypervisor process")
	}
	deadline, cancel := context.WithTimeout(ctx, d.stop)
	defer cancel()
	for {
		if proc != nil {
			select {
			case <-proc.done:
				_ = os.Remove(path)
				return d.removeSocket(dir)
			default:
			}
		}
		stillAlive, same := processMatches(record, d.binaryPath, apiSocket)
		if !stillAlive || !same {
			_ = os.Remove(path)
			return d.removeSocket(dir)
		}
		select {
		case <-deadline.Done():
			return errors.New("local CloudHypervisor process did not stop before timeout")
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (d *CloudHypervisorLocalDriver) removeSocket(dir string) error {
	for _, name := range []string{localCloudHypervisorAPIPath, localCloudHypervisorVsockPath} {
		path := filepath.Join(dir, strings.TrimPrefix(name, "/"))
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			return errors.New("local CloudHypervisor socket path is not a socket")
		}
		if err := os.Remove(path); err != nil {
			return errors.New("remove stale local CloudHypervisor socket")
		}
	}
	return nil
}

// DialCloudHypervisorLocalGuest connects only to the fixed Workspace Agent vsock
// port. Callers cannot choose a Guest IP or arbitrary service port.
func DialCloudHypervisorLocalGuest(ctx context.Context, stateDirectory string) (net.Conn, error) {
	return dialCloudHypervisorLocalGuestPort(ctx, stateDirectory, localGuestAgentVsockPort)
}

func DialCloudHypervisorLocalGuestControl(ctx context.Context, stateDirectory string) (net.Conn, error) {
	return dialCloudHypervisorLocalGuestPort(ctx, stateDirectory, localGuestControlVsockPort)
}

func requestLocalGuestShutdown(ctx context.Context, stateDirectory string) error {
	launchPath := filepath.Join(stateDirectory, strings.TrimPrefix(localCloudHypervisorLaunchSpecPath, "/"))
	launchData, err := os.ReadFile(launchPath)
	if err != nil || len(launchData) == 0 || len(launchData) > 1<<20 {
		return errors.New("local Workspace Guest shutdown identity is unavailable")
	}
	var launch CloudHypervisorLocalLaunchSpec
	if json.Unmarshal(launchData, &launch) != nil {
		return errors.New("local Workspace Guest shutdown identity is invalid")
	}
	identity, err := localGuestIdentityFromKernelArgs(launch.KernelBootArgs)
	if err != nil {
		return errors.New("local Workspace Guest shutdown identity is invalid")
	}
	requestBody, err := json.Marshal(struct {
		TenantID        string `json:"tenant_id"`
		WorkspaceID     string `json:"workspace_id"`
		ResourcePlaneID string `json:"resource_plane_id"`
		MicroVMID       string `json:"microvm_id"`
	}{identity.tenantID, identity.workspaceID, identity.resourcePlaneID, identity.microVMID})
	if err != nil {
		return errors.New("encode local Workspace Guest shutdown identity")
	}
	conn, err := DialCloudHypervisorLocalGuestControl(ctx, stateDirectory)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(6 * time.Second))
	request, err := http.NewRequest(http.MethodPost, "http://workspace-control.vsock/internal/shutdown", bytes.NewReader(requestBody))
	if err != nil {
		return errors.New("create local Workspace Guest shutdown request")
	}
	request.Header.Set("Content-Type", "application/json")
	if err := request.Write(conn); err != nil {
		return errors.New("send local Workspace Guest shutdown request")
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), request)
	if err != nil {
		return errors.New("read local Workspace Guest shutdown response")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent || response.ContentLength > 0 {
		return errors.New("local Workspace Guest did not accept shutdown request")
	}
	return nil
}

func dialCloudHypervisorLocalGuestPort(ctx context.Context, stateDirectory string, guestPort uint32) (net.Conn, error) {
	if ctx == nil || ctx.Err() != nil || !filepath.IsAbs(stateDirectory) || filepath.Clean(stateDirectory) != stateDirectory {
		return nil, errors.New("local CloudHypervisor Guest dial configuration is invalid")
	}
	if guestPort != localGuestAgentVsockPort && guestPort != localGuestControlVsockPort {
		return nil, errors.New("local CloudHypervisor Guest port is not allowlisted")
	}
	dirInfo, err := os.Lstat(stateDirectory)
	dirStat, ok := func() (*syscall.Stat_t, bool) {
		if dirInfo == nil {
			return nil, false
		}
		value, valid := dirInfo.Sys().(*syscall.Stat_t)
		return value, valid
	}()
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 || !ok || dirStat.Uid != uint32(os.Getuid()) || dirInfo.Mode().Perm()&0077 != 0 {
		return nil, errors.New("local CloudHypervisor Guest state directory is unsafe")
	}
	socket := filepath.Join(stateDirectory, strings.TrimPrefix(localCloudHypervisorVsockPath, "/"))
	if len(socket) >= 104 {
		return nil, errors.New("local CloudHypervisor Guest vsock path is too long")
	}
	socketInfo, err := os.Lstat(socket)
	socketStat, ok := func() (*syscall.Stat_t, bool) {
		if socketInfo == nil {
			return nil, false
		}
		value, valid := socketInfo.Sys().(*syscall.Stat_t)
		return value, valid
	}()
	if err != nil || socketInfo.Mode()&os.ModeSocket == 0 || socketInfo.Mode().Perm()&0077 != 0 || !ok || socketStat.Uid != uint32(os.Getuid()) {
		return nil, errors.New("local CloudHypervisor Guest vsock socket is unsafe or unavailable")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", socket)
	if err != nil {
		return nil, errors.New("connect to local CloudHypervisor Guest vsock")
	}
	if deadline, ok := dialCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := io.WriteString(conn, fmt.Sprintf("CONNECT %d\n", guestPort)); err != nil {
		_ = conn.Close()
		return nil, errors.New("request local CloudHypervisor Guest vsock port")
	}
	response, err := readVsockHandshakeLine(conn, 64)
	fields := strings.Fields(response)
	assignedPort := uint64(0)
	if len(fields) == 2 && fields[0] == "OK" {
		assignedPort, _ = strconv.ParseUint(fields[1], 10, 32)
	}
	if err != nil || len(fields) != 2 || fields[0] != "OK" || assignedPort == 0 {
		_ = conn.Close()
		return nil, errors.New("local CloudHypervisor Guest rejected the fixed vsock port")
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, errors.New("clear local CloudHypervisor Guest vsock deadline")
	}
	return conn, nil
}

// readVsockHandshakeLine reads only the connection acknowledgement. An eager
// Guest write can be coalesced with that line, so buffered readers are unsafe.
func readVsockHandshakeLine(reader io.Reader, maxBytes int) (string, error) {
	if reader == nil || maxBytes < 1 || maxBytes > 256 {
		return "", errors.New("invalid CloudHypervisor vsock handshake reader")
	}
	line := make([]byte, 0, maxBytes)
	var one [1]byte
	for range maxBytes {
		if _, err := io.ReadFull(reader, one[:]); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return string(line), nil
		}
		line = append(line, one[0])
	}
	return "", errors.New("CloudHypervisor vsock handshake line exceeds its limit")
}

func (d *CloudHypervisorLocalDriver) writeGuestQuiescedRecord(dir, allocationID string) error {
	launchPath := filepath.Join(dir, "launch-spec.json")
	payload, err := os.ReadFile(launchPath)
	if err != nil {
		return errors.New("read pinned local CloudHypervisor launch specification")
	}
	record := cloudHypervisorGuestQuiescedRecord{Version: 1, AllocationID: allocationID,
		LaunchSpecSHA: workspacejournal.DigestLaunchSpec(payload), WorkspaceDisk: strings.TrimPrefix(localWorkspaceDiskPath, "/")}
	return writeJSONAtomic(filepath.Join(dir, strings.TrimPrefix(localGuestQuiescedPath, "/")), record, 0600)
}

func (d *CloudHypervisorLocalDriver) hasGuestQuiesced(dir, allocationID string) bool {
	_, ok := d.readGuestQuiescedRecord(dir, allocationID)
	return ok
}

func (d *CloudHypervisorLocalDriver) readGuestQuiescedRecord(dir, allocationID string) (cloudHypervisorGuestQuiescedRecord, bool) {
	path := filepath.Join(dir, strings.TrimPrefix(localGuestQuiescedPath, "/"))
	record, err := readJSON[cloudHypervisorGuestQuiescedRecord](path)
	if err != nil || record.Version != 1 || record.AllocationID != allocationID ||
		record.WorkspaceDisk != strings.TrimPrefix(localWorkspaceDiskPath, "/") {
		return cloudHypervisorGuestQuiescedRecord{}, false
	}
	payload, err := os.ReadFile(filepath.Join(dir, "launch-spec.json"))
	if err != nil || workspacejournal.DigestLaunchSpec(payload) != record.LaunchSpecSHA {
		return cloudHypervisorGuestQuiescedRecord{}, false
	}
	return record, true
}

func (d *CloudHypervisorLocalDriver) removeGuestQuiesced(dir string) error {
	path := filepath.Join(dir, strings.TrimPrefix(localGuestQuiescedPath, "/"))
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("remove local CloudHypervisor guest quiescence record")
	}
	return syncLocalDirectory(dir)
}

func cloudHypervisorLogOffset(dir string) (int64, error) {
	path := filepath.Join(dir, strings.TrimPrefix(localCloudHypervisorLogPath, "/"))
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return 0, errors.New("inspect local CloudHypervisor log")
	}
	return info.Size(), nil
}

func (d *CloudHypervisorLocalDriver) waitForGuestQuiescence(ctx context.Context, dir string, offset int64) error {
	deadline, cancel := context.WithTimeout(ctx, d.stop)
	defer cancel()
	path := filepath.Join(dir, strings.TrimPrefix(localCloudHypervisorLogPath, "/"))
	for {
		file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		markerFound := false
		if err == nil {
			_, seekErr := file.Seek(offset, io.SeekStart)
			chunk, readErr := io.ReadAll(io.LimitReader(file, 64<<10))
			_ = file.Close()
			if seekErr == nil && readErr == nil {
				markerFound = strings.Contains(string(chunk), localGuestQuiescedMarker)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("read local CloudHypervisor serial log")
		}
		if markerFound {
			return nil
		}
		if state, stateErr := d.vmState(deadline, dir); stateErr == nil && state == "Shutdown" {
			return nil
		}
		alive, processErr := d.processAlive(dir)
		if processErr != nil {
			return processErr
		}
		if !alive && cloudHypervisorExitedCleanly(path) {
			return nil
		}
		select {
		case <-deadline.Done():
			return errors.New("local CloudHypervisor Guest did not confirm filesystem quiescence before timeout")
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func cloudHypervisorExitedCleanly(path string) bool {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	const tailLimit = 64 << 10
	start := info.Size() - tailLimit
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return false
	}
	tail, err := io.ReadAll(io.LimitReader(file, tailLimit))
	return err == nil && strings.Contains(string(tail), "Cloud Hypervisor exiting successfully. exit_code=0")
}

func pinCloudHypervisorLaunchSpec(dir string, payload []byte) error {
	path := filepath.Join(dir, "launch-spec.json")
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if errors.Is(err, unix.EEXIST) {
		file, openErr := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if openErr != nil {
			return errors.New("open pinned local CloudHypervisor launch specification")
		}
		defer file.Close()
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > 16<<10 {
			return errors.New("pinned local CloudHypervisor launch specification is unsafe")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) {
			return errors.New("pinned local CloudHypervisor launch specification has an unexpected owner")
		}
		stored, readErr := io.ReadAll(io.LimitReader(file, (16<<10)+1))
		if readErr != nil || !bytes.Equal(stored, payload) {
			return workspacejournal.ErrConflict
		}
		return nil
	}
	if err != nil {
		return errors.New("create pinned local CloudHypervisor launch specification")
	}
	file := os.NewFile(uintptr(fd), path)
	if err := writeSyncClose(file, payload); err != nil {
		return err
	}
	return syncLocalDirectory(dir)
}

func writeSyncClose(file *os.File, body []byte) error {
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return errors.New("write pinned local CloudHypervisor launch specification")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errors.New("sync pinned local CloudHypervisor launch specification")
	}
	if err := file.Close(); err != nil {
		return errors.New("close pinned local CloudHypervisor launch specification")
	}
	return nil
}

func readCloudHypervisorProcessRecord(pid int) (cloudHypervisorProcessRecord, error) {
	ticks, err := processStartTicks(pid)
	if err != nil {
		return cloudHypervisorProcessRecord{}, err
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return cloudHypervisorProcessRecord{}, err
	}
	return cloudHypervisorProcessRecord{PID: pid, StartTicks: ticks, BootID: strings.TrimSpace(string(bootID))}, nil
}

func processStartTicks(pid int) (uint64, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	closeParen := bytes.LastIndexByte(data, ')')
	if closeParen < 0 || closeParen+2 >= len(data) {
		return 0, errors.New("parse process stat")
	}
	fields := strings.Fields(string(data[closeParen+2:]))
	if len(fields) <= 19 {
		return 0, errors.New("parse process start time")
	}
	return strconv.ParseUint(fields[19], 10, 64)
}

func processMatches(record cloudHypervisorProcessRecord, binary, socket string) (alive bool, valid bool) {
	if record.PID < 1 || record.BootID == "" || record.StartTicks == 0 {
		return false, false
	}
	ticks, err := processStartTicks(record.PID)
	if errors.Is(err, os.ErrNotExist) {
		return false, false
	}
	if err != nil {
		return true, false
	}
	if ticks != record.StartTicks {
		return false, false
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(bootID)) != record.BootID {
		return false, false
	}
	exe, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(record.PID), "exe"))
	if err != nil || filepath.Clean(exe) != binary {
		return true, false
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(record.PID), "cmdline"))
	if err != nil {
		return true, false
	}
	args := strings.Split(string(cmdline), "\x00")
	found := false
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--api-socket" && args[i+1] == "path="+socket {
			found = true
			break
		}
	}
	return true, found
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	body, err := json.Marshal(value)
	if err != nil {
		return errors.New("encode local CloudHypervisor state record")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".cloud-hypervisor-*.tmp")
	if err != nil {
		return errors.New("create local CloudHypervisor state temp file")
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return errors.New("restrict local CloudHypervisor state record")
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return errors.New("write local CloudHypervisor state record")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errors.New("sync local CloudHypervisor state record")
	}
	if err := file.Close(); err != nil {
		return errors.New("close local CloudHypervisor state record")
	}
	if err := os.Rename(tempPath, path); err != nil {
		return errors.New("replace local CloudHypervisor state record")
	}
	return syncLocalDirectory(filepath.Dir(path))
}

func readJSON[T any](path string) (T, error) {
	var value T
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return value, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > 8<<10 {
		return value, errors.New("local CloudHypervisor state record is unsafe")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return value, errors.New("local CloudHypervisor state record has an unexpected owner")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 8<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return value, errors.New("local CloudHypervisor state record must contain one JSON object")
	}
	return value, nil
}

func syncLocalDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return errors.New("open local CloudHypervisor directory for sync")
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return errors.New("sync local CloudHypervisor directory")
	}
	return nil
}

var _ LocalMicroVMDriver = (*CloudHypervisorLocalDriver)(nil)
