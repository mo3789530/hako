//go:build linux

package workspaceagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/servicecatalog"
)

type ServiceCommandRunner interface {
	Run(context.Context, string, []string, []string) ([]byte, error)
}

type execServiceCommandRunner struct{}

type serviceCommandInputRunner interface {
	RunInput(context.Context, string, []string, []string, []byte) ([]byte, error)
}

func (execServiceCommandRunner) Run(ctx context.Context, binary string, args, env []string) ([]byte, error) {
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = env
	output, err := command.CombinedOutput()
	if err != nil {
		// Container output is intentionally discarded: it may contain user data
		// or credentials. Callers receive only a generic operation error.
		return nil, errors.New("containerd command failed")
	}
	return output, nil
}

func (execServiceCommandRunner) RunInput(ctx context.Context, binary string, args, env []string, input []byte) ([]byte, error) {
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = env
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, errors.New("containerd command failed")
	}
	return output, nil
}

type NerdctlServiceEngineConfig struct {
	BinaryPath       string
	ContainerdSocket string
	WorkspaceRoot    string
	WorkspaceID      domain.WorkspaceID
	RunAsUID         uint32
	NetworkName      string
	EnvFileDirectory string
	Runner           ServiceCommandRunner
}

const (
	NerdctlBinaryEnvironment    = "HAKO_NERDCTL_BINARY"
	ContainerdSocketEnvironment = "HAKO_CONTAINERD_SOCKET"
)

// NerdctlServiceEngine is a rootless containerd adapter. It invokes nerdctl
// with an explicit environment and only derives images, ports, mounts, and
// health argv from the immutable built-in Service Catalog.
type NerdctlServiceEngine struct {
	config  NerdctlServiceEngineConfig
	runner  ServiceCommandRunner
	env     []string
	isTmpfs func(string) bool
}

func NewNerdctlServiceEngine(config NerdctlServiceEngineConfig) (*NerdctlServiceEngine, error) {
	config.BinaryPath = strings.TrimSpace(config.BinaryPath)
	config.ContainerdSocket = strings.TrimSpace(config.ContainerdSocket)
	config.WorkspaceRoot = filepath.Clean(strings.TrimSpace(config.WorkspaceRoot))
	config.NetworkName = strings.TrimSpace(config.NetworkName)
	config.EnvFileDirectory = strings.TrimSpace(config.EnvFileDirectory)
	if config.BinaryPath == "" {
		config.BinaryPath = "/usr/bin/nerdctl"
	}
	if config.NetworkName == "" {
		config.NetworkName = "hako-" + string(config.WorkspaceID)
	}
	if config.EnvFileDirectory == "" {
		config.EnvFileDirectory = "/run/user/" + strconv.FormatUint(uint64(config.RunAsUID), 10)
	}
	if !filepath.IsAbs(config.BinaryPath) || !filepath.IsAbs(config.ContainerdSocket) || !filepath.IsAbs(config.WorkspaceRoot) ||
		config.WorkspaceRoot == "/" || config.WorkspaceID == "" || !agentIdentityPattern.MatchString(string(config.WorkspaceID)) ||
		config.RunAsUID < 100000 || !agentIdentityPattern.MatchString(config.NetworkName) || !filepath.IsAbs(config.EnvFileDirectory) ||
		filepath.Clean(config.EnvFileDirectory) != config.EnvFileDirectory || config.EnvFileDirectory == "/" {
		return nil, errors.New("rootless containerd Service engine configuration is invalid")
	}
	if config.Runner == nil {
		config.Runner = execServiceCommandRunner{}
	}
	runtimeDir := "/run/user/" + strconv.FormatUint(uint64(config.RunAsUID), 10)
	return &NerdctlServiceEngine{config: config, runner: config.Runner, isTmpfs: isTmpfsDirectory,
		env: []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + config.WorkspaceRoot,
			"XDG_RUNTIME_DIR=" + runtimeDir, "CONTAINERD_SNAPSHOTTER=native"}}, nil
}

func isTmpfsDirectory(path string) bool {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return false
	}
	return uint64(stat.Type) == uint64(unix.TMPFS_MAGIC)
}

func (e *NerdctlServiceEngine) Ensure(ctx context.Context, serviceID domain.ServiceID, instanceName string, template servicecatalog.Template, secrets map[string]string) error {
	if e == nil || ctx == nil || !validServiceID(serviceID) || !servicecatalog.ValidateInstanceName(instanceName) || validateBuiltinTemplate(template) != nil {
		return errors.New("invalid or unsupported Workspace Service runtime configuration")
	}
	if !validServiceSecrets(template, secrets) {
		return ErrServiceSecretPolicy
	}
	if err := e.ensureNetwork(ctx); err != nil {
		return err
	}
	containerName := serviceContainerName(serviceID)
	exists, running, err := e.containerState(ctx, containerName)
	if err != nil {
		return err
	}
	if exists {
		if !running {
			if _, err := e.run(ctx, "start", containerName); err != nil {
				return errors.New("start Workspace Service container")
			}
		}
		return e.applyGeneratedCredentials(ctx, containerName, template, secrets)
	}
	if err := e.prepareVolumes(template, serviceID); err != nil {
		return errors.New("prepare Workspace Service volume")
	}
	args := []string{"run", "--detach", "--restart=always", "--name", containerName,
		"--network", e.config.NetworkName,
		"--label", "hako.managed-by=hako",
		"--label", "hako.workspace-id=" + string(e.config.WorkspaceID),
		"--label", "hako.service-id=" + string(serviceID),
		"--label", "hako.service-name=" + instanceName,
	}
	for _, port := range template.Ports {
		args = append(args, "--publish", fmt.Sprintf("127.0.0.1:%d:%d/%s", port.Number, port.Number, port.Protocol))
	}
	for _, volume := range template.Volumes {
		path := e.volumePath(serviceID, volume.Name)
		args = append(args, "--volume", path+":"+volume.MountPath)
	}
	args = append(args, "--memory", strconv.FormatInt(template.Resources.MemoryBytes, 10),
		"--cpus", strconv.FormatFloat(float64(template.Resources.MilliCPU)/1000, 'f', 3, 64))
	for key, value := range template.Environment {
		if strings.ContainsAny(key+value, "\x00\r\n") {
			return errors.New("invalid Workspace Service environment template")
		}
		args = append(args, "--env", key+"="+value)
	}
	if len(template.GeneratedSecrets) != 0 {
		envFile, err := e.writeSecretEnvFile(secrets)
		if err != nil {
			return errors.New("prepare Workspace Service credentials")
		}
		defer os.Remove(envFile)
		args = append(args, "--env-file", envFile)
	}
	args = append(args, template.Image)
	if _, err := e.run(ctx, args...); err != nil {
		return errors.New("create Workspace Service container")
	}
	return nil
}

func (e *NerdctlServiceEngine) applyGeneratedCredentials(ctx context.Context, containerName string, template servicecatalog.Template, secrets map[string]string) error {
	if len(template.GeneratedSecrets) == 0 {
		return nil
	}
	if template.Name != "postgres" || len(template.GeneratedSecrets) != 1 || template.GeneratedSecrets[0] != "POSTGRES_PASSWORD" {
		return ErrServiceSecretPolicy
	}
	value := secrets["POSTGRES_PASSWORD"]
	if !validServiceSecrets(template, secrets) {
		return ErrServiceSecretPolicy
	}
	inputRunner, ok := e.runner.(serviceCommandInputRunner)
	if !ok {
		return errors.New("container runtime does not support safe credential input")
	}
	// The generated password alphabet is URL-safe base64, so it cannot escape
	// this fixed SQL string. It is sent through stdin to psql, never argv/env.
	sql := []byte("ALTER ROLE postgres WITH PASSWORD '" + value + "';\n")
	defer clear(sql)
	if _, err := inputRunner.RunInput(ctx, e.config.BinaryPath,
		[]string{"--address", e.config.ContainerdSocket, "exec", "--interactive", "--user", "postgres", containerName,
			"psql", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "postgres"}, e.env, sql); err != nil {
		return errors.New("rotate Workspace PostgreSQL credential")
	}
	return nil
}

func (e *NerdctlServiceEngine) writeSecretEnvFile(secrets map[string]string) (string, error) {
	info, err := os.Lstat(e.config.EnvFileDirectory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 || e.isTmpfs == nil || !e.isTmpfs(e.config.EnvFileDirectory) {
		return "", ErrServiceSecretPolicy
	}
	file, err := os.CreateTemp(e.config.EnvFileDirectory, "hako-service-env-")
	if err != nil {
		return "", ErrServiceSecretPolicy
	}
	path := file.Name()
	failed := true
	defer func() {
		if failed {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0600); err != nil {
		return "", ErrServiceSecretPolicy
	}
	if _, err := file.WriteString("POSTGRES_PASSWORD=" + secrets["POSTGRES_PASSWORD"] + "\n"); err != nil {
		return "", ErrServiceSecretPolicy
	}
	if err := file.Close(); err != nil {
		return "", ErrServiceSecretPolicy
	}
	failed = false
	return path, nil
}

func (e *NerdctlServiceEngine) Stop(ctx context.Context, serviceID domain.ServiceID) error {
	if e == nil || ctx == nil || !validServiceID(serviceID) {
		return errors.New("invalid Workspace Service identity")
	}
	name := serviceContainerName(serviceID)
	exists, running, err := e.containerState(ctx, name)
	if err != nil || !exists || !running {
		return err
	}
	if _, err := e.run(ctx, "stop", "--time", "10", name); err != nil {
		return errors.New("stop Workspace Service container")
	}
	return nil
}

func (e *NerdctlServiceEngine) Remove(ctx context.Context, serviceID domain.ServiceID) error {
	if e == nil || ctx == nil || !validServiceID(serviceID) {
		return errors.New("invalid Workspace Service identity")
	}
	name := serviceContainerName(serviceID)
	exists, _, err := e.containerState(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		if _, err := e.run(ctx, "rm", "--force", name); err != nil {
			return errors.New("remove Workspace Service container")
		}
	}
	stillExists, stillRunning, err := e.containerState(ctx, name)
	if err != nil || stillExists || stillRunning {
		return errors.New("confirm Workspace Service container removal")
	}
	if err := e.removeServiceData(serviceID); err != nil {
		return errors.New("remove Workspace Service data")
	}
	return nil
}

func (e *NerdctlServiceEngine) Healthy(ctx context.Context, serviceID domain.ServiceID, healthcheck servicecatalog.HealthCheck) (bool, error) {
	if e == nil || ctx == nil || !validServiceID(serviceID) || len(healthcheck.Command) == 0 {
		return false, errors.New("invalid Workspace Service health check")
	}
	approvedHealthcheck := false
	for _, template := range servicecatalog.List() {
		if reflect.DeepEqual(template.HealthCheck, healthcheck) {
			approvedHealthcheck = true
			break
		}
	}
	if !approvedHealthcheck {
		return false, errors.New("Workspace Service health check is not in the approved catalog")
	}
	exists, running, err := e.containerState(ctx, serviceContainerName(serviceID))
	if err != nil || !exists || !running {
		return false, err
	}
	args := []string{"exec", serviceContainerName(serviceID)}
	args = append(args, healthcheck.Command...)
	if _, err := e.run(ctx, args...); err != nil {
		return false, nil
	}
	return true, nil
}

func (e *NerdctlServiceEngine) ensureNetwork(ctx context.Context) error {
	output, err := e.run(ctx, "network", "ls", "--format", "{{.Name}}")
	if err != nil {
		return errors.New("inspect Workspace Service network")
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == e.config.NetworkName {
			return nil
		}
	}
	if _, err := e.run(ctx, "network", "create", e.config.NetworkName); err != nil {
		return errors.New("create Workspace Service network")
	}
	return nil
}

func (e *NerdctlServiceEngine) containerState(ctx context.Context, containerName string) (bool, bool, error) {
	output, err := e.run(ctx, "ps", "--all", "--filter", "name=^"+containerName+"$", "--format", "{{.Names}} {{.State}}")
	if err != nil {
		return false, false, errors.New("inspect Workspace Service container")
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == containerName {
			return true, strings.EqualFold(fields[1], "running"), nil
		}
	}
	return false, false, nil
}

func (e *NerdctlServiceEngine) prepareVolumes(template servicecatalog.Template, serviceID domain.ServiceID) error {
	resolvedRoot, err := filepath.EvalSymlinks(e.config.WorkspaceRoot)
	if err != nil {
		return err
	}
	for _, volume := range template.Volumes {
		path := e.volumePath(serviceID, volume.Name)
		relative, err := filepath.Rel(resolvedRoot, path)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("Workspace Service volume escapes Workspace root")
		}
		current := resolvedRoot
		for _, component := range strings.Split(relative, string(filepath.Separator)) {
			current = filepath.Join(current, component)
			info, statErr := os.Lstat(current)
			if errors.Is(statErr, os.ErrNotExist) {
				if err := os.Mkdir(current, 0700); err != nil {
					return err
				}
				info, statErr = os.Lstat(current)
			}
			if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("Workspace Service volume path contains a non-directory or symbolic link")
			}
		}
		resolvedPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		resolvedRelative, err := filepath.Rel(resolvedRoot, resolvedPath)
		if err != nil || resolvedRelative == "." || resolvedRelative == ".." || strings.HasPrefix(resolvedRelative, ".."+string(filepath.Separator)) {
			return errors.New("Workspace Service volume escapes Workspace root")
		}
	}
	return nil
}

func (e *NerdctlServiceEngine) volumePath(serviceID domain.ServiceID, volumeName string) string {
	return filepath.Join(e.config.WorkspaceRoot, ".hako", "services", string(serviceID), volumeName)
}

// removeServiceData removes only the Service-owned subtree. Every path
// component is opened relative to a directory descriptor with O_NOFOLLOW;
// recursive deletion never traverses symlinks, including ones planted by the
// Workspace user. A failure is returned so the caller keeps its deletion
// tombstone and retries instead of reporting cleanup as complete.
func (e *NerdctlServiceEngine) removeServiceData(serviceID domain.ServiceID) error {
	if e == nil || !validServiceID(serviceID) {
		return errors.New("invalid Workspace Service identity")
	}
	rootPath, err := filepath.EvalSymlinks(e.config.WorkspaceRoot)
	if err != nil {
		return err
	}
	rootFD, err := unix.Open(rootPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(rootFD)

	hakoFD, err := openWorkspaceServiceDirectory(rootFD, ".hako")
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(hakoFD)
	servicesFD, err := openWorkspaceServiceDirectory(hakoFD, "services")
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(servicesFD)
	serviceFD, err := openWorkspaceServiceDirectory(servicesFD, string(serviceID))
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := removeWorkspaceServiceDirectoryContents(serviceFD); err != nil {
		unix.Close(serviceFD)
		return err
	}
	if err := unix.Close(serviceFD); err != nil {
		return err
	}
	return unix.Unlinkat(servicesFD, string(serviceID), unix.AT_REMOVEDIR)
}

func openWorkspaceServiceDirectory(parentFD int, name string) (int, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\x00') {
		return -1, errors.New("invalid Workspace Service data path")
	}
	return unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
}

func removeWorkspaceServiceDirectoryContents(directoryFD int) error {
	copyFD, err := unix.Dup(directoryFD)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(copyFD), "workspace-service-data")
	if file == nil {
		unix.Close(copyFD)
		return errors.New("open Workspace Service data directory")
	}
	entries, err := file.ReadDir(-1)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\x00') {
			return errors.New("invalid Workspace Service data entry")
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(directoryFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return err
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			// Unlinking a symlink removes the link itself and never follows it.
			if err := unix.Unlinkat(directoryFD, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				return err
			}
			continue
		}
		childFD, err := openWorkspaceServiceDirectory(directoryFD, name)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return err
		}
		if err := removeWorkspaceServiceDirectoryContents(childFD); err != nil {
			unix.Close(childFD)
			return err
		}
		if err := unix.Close(childFD); err != nil {
			return err
		}
		if err := unix.Unlinkat(directoryFD, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
	}
	return nil
}

func (e *NerdctlServiceEngine) run(ctx context.Context, args ...string) ([]byte, error) {
	base := []string{"--address", e.config.ContainerdSocket, "--namespace", "hako-services"}
	return e.runner.Run(ctx, e.config.BinaryPath, append(base, args...), append([]string(nil), e.env...))
}

func serviceContainerName(serviceID domain.ServiceID) string { return "hako-" + string(serviceID) }

func validServiceID(serviceID domain.ServiceID) bool {
	value := string(serviceID)
	return len(value) > 4 && strings.HasPrefix(value, "svc_") && agentIdentityPattern.MatchString(value)
}

func validateBuiltinTemplate(template servicecatalog.Template) error {
	builtin, err := servicecatalog.Resolve(template.Name)
	if err != nil {
		return err
	}
	got, err := servicecatalog.CanonicalJSON(template)
	if err != nil {
		return err
	}
	want, err := servicecatalog.CanonicalJSON(builtin)
	if err != nil || string(got) != string(want) {
		return errors.New("Service template differs from approved catalog")
	}
	return nil
}
