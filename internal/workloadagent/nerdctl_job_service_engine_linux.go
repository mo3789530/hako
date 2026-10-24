//go:build linux

package workloadagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/mo3789530/hako/internal/servicecatalog"
)

const (
	jobNerdctlNamespace      = "hako-job-services"
	jobNerdctlGlobalArgCount = 5
	jobScopeLabel            = "hako.job-scope-key"
)

type JobServiceCommandRunner interface {
	Run(context.Context, string, []string, []string) ([]byte, error)
	RunInput(context.Context, string, []string, []string, []byte) ([]byte, error)
}

type execJobServiceCommandRunner struct{}

func (execJobServiceCommandRunner) Run(ctx context.Context, binary string, args, env []string) ([]byte, error) {
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = env
	output, err := command.CombinedOutput()
	if err != nil {
		// Runtime output can contain image or application data. Never return it.
		return nil, errors.New("Job Service container runtime command failed")
	}
	return output, nil
}

func (execJobServiceCommandRunner) RunInput(ctx context.Context, binary string, args, env []string, input []byte) ([]byte, error) {
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = env
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, errors.New("Job Service container runtime command failed")
	}
	return output, nil
}

type NerdctlJobServiceEngineConfig struct {
	BinaryPath       string
	ContainerdSocket string
	RuntimeDirectory string
	EnvFileDirectory string
	Runner           JobServiceCommandRunner
}

// NerdctlJobServiceEngine manages only ephemeral, fixed-catalog Job sidecars.
// It intentionally has no Workspace ID, durable volume path, arbitrary image,
// command, or mount input.
type NerdctlJobServiceEngine struct {
	config  NerdctlJobServiceEngineConfig
	runner  JobServiceCommandRunner
	env     []string
	isTmpfs func(string) bool
}

func NewNerdctlJobServiceEngine(config NerdctlJobServiceEngineConfig) (*NerdctlJobServiceEngine, error) {
	config.BinaryPath = strings.TrimSpace(config.BinaryPath)
	config.ContainerdSocket = filepath.Clean(strings.TrimSpace(config.ContainerdSocket))
	config.RuntimeDirectory = filepath.Clean(strings.TrimSpace(config.RuntimeDirectory))
	config.EnvFileDirectory = filepath.Clean(strings.TrimSpace(config.EnvFileDirectory))
	if config.BinaryPath == "" {
		config.BinaryPath = "/usr/bin/nerdctl"
	}
	if !filepath.IsAbs(config.BinaryPath) || !filepath.IsAbs(config.ContainerdSocket) || config.ContainerdSocket == "/" ||
		!filepath.IsAbs(config.RuntimeDirectory) || config.RuntimeDirectory == "/" ||
		!filepath.IsAbs(config.EnvFileDirectory) || config.EnvFileDirectory == "/" {
		return nil, ErrJobServicesUnavailable
	}
	if config.Runner == nil {
		config.Runner = execJobServiceCommandRunner{}
	}
	return &NerdctlJobServiceEngine{config: config, runner: config.Runner,
		env: []string{"PATH=/usr/bin:/bin", "HOME=/tmp", "XDG_RUNTIME_DIR=" + config.RuntimeDirectory,
			"CONTAINERD_SNAPSHOTTER=native"}, isTmpfs: isJobServiceTmpfs}, nil
}

func isJobServiceTmpfs(path string) bool {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return false
	}
	return uint64(stat.Type) == uint64(unix.TMPFS_MAGIC)
}

func (e *NerdctlJobServiceEngine) EnsureNetwork(ctx context.Context, name, owner string) error {
	if !e.valid() || ctx == nil || ctx.Err() != nil || !validJobNetworkName(name) || !validJobScope(owner) {
		return ErrJobServicesUnavailable
	}
	exists, err := e.networkExists(ctx, name)
	if err != nil {
		return ErrJobServicesUnavailable
	}
	if exists {
		internal, actualOwner, managedBy, err := e.inspectNetwork(ctx, name)
		if err != nil || !internal || actualOwner != owner || managedBy != "hako" {
			return ErrJobServicesUnavailable
		}
		return nil
	}
	_, err = e.run(ctx, "network", "create", "--internal", "--label", "hako.managed-by=hako",
		"--label", jobScopeLabel+"="+owner, name)
	if err != nil {
		return ErrJobServicesUnavailable
	}
	internal, actualOwner, managedBy, err := e.inspectNetwork(ctx, name)
	if err != nil || !internal || actualOwner != owner || managedBy != "hako" {
		return ErrJobServicesUnavailable
	}
	return nil
}

func (e *NerdctlJobServiceEngine) EnsureService(ctx context.Context, name, network, owner string, template servicecatalog.Template, secrets map[string]string) error {
	if !e.valid() || ctx == nil || ctx.Err() != nil || !validJobServiceContainerName(name) || !validJobNetworkName(network) ||
		!validJobScope(owner) || validateJobServiceTemplate(template, secrets) != nil {
		return ErrJobServicesUnavailable
	}
	internal, actualOwner, managedBy, err := e.inspectNetwork(ctx, network)
	if err != nil || !internal || actualOwner != owner || managedBy != "hako" {
		return ErrJobServicesUnavailable
	}
	exists, err := e.containerExists(ctx, name)
	if err != nil || exists {
		// A previous attempt must be removed by its exact ownership scope before
		// creating the fresh ephemeral instance. Never adopt a running container.
		return ErrJobServicesUnavailable
	}
	args := []string{"run", "--detach", "--pull=never", "--restart=no", "--name", name,
		"--network", network, "--read-only", "--user", "999:999", "--cap-drop=ALL", "--security-opt", "no-new-privileges",
		"--pids-limit", "256", "--memory", strconv.FormatInt(template.Resources.MemoryBytes, 10),
		"--cpus", strconv.FormatFloat(float64(template.Resources.MilliCPU)/1000, 'f', 3, 64),
		"--label", "hako.managed-by=hako", "--label", jobScopeLabel + "=" + owner,
		"--label", "hako.service-name=" + template.Name,
		"--log-driver=none"}
	for _, port := range template.Ports {
		if port.Protocol != "tcp" {
			return ErrJobServicesUnavailable
		}
		args = append(args, "--publish", fmt.Sprintf("127.0.0.1:%d:%d/tcp", port.Number, port.Number))
	}
	for _, tmpfs := range jobServiceTmpfs(template) {
		args = append(args, "--tmpfs", tmpfs)
	}
	environmentNames := make([]string, 0, len(template.Environment))
	for key := range template.Environment {
		environmentNames = append(environmentNames, key)
	}
	sort.Strings(environmentNames)
	for _, key := range environmentNames {
		args = append(args, "--env", key+"="+template.Environment[key])
	}
	envFile := ""
	if len(secrets) != 0 {
		var err error
		envFile, err = e.writeSecretEnvFile(secrets)
		if err != nil {
			if errors.Is(err, ErrJobServiceCleanup) {
				return ErrJobServiceCleanup
			}
			return ErrJobServicesUnavailable
		}
		args = append(args, "--env-file", envFile)
	}
	args = append(args, template.Image)
	_, runErr := e.run(ctx, args...)
	if envFile != "" {
		removeErr := os.Remove(envFile)
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return ErrJobServiceCleanup
		}
	}
	if runErr != nil {
		return ErrJobServicesUnavailable
	}
	if err := e.verifyCreatedService(ctx, name, owner, template); err != nil {
		if cleanupErr := e.RemoveService(ctx, name, owner); cleanupErr != nil {
			return ErrJobServiceCleanup
		}
		return ErrJobServicesUnavailable
	}
	return nil
}

func (e *NerdctlJobServiceEngine) verifyCreatedService(ctx context.Context, name, owner string, template servicecatalog.Template) error {
	output, err := e.run(ctx, "inspect", "--format", "{{json .}}", name)
	if err != nil {
		return ErrJobServicesUnavailable
	}
	var actual struct {
		Config struct {
			User   string            `json:"User"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		HostConfig struct {
			Memory       int64 `json:"Memory"`
			NanoCPUs     int64 `json:"NanoCpus"`
			PidsLimit    int64 `json:"PidsLimit"`
			ReadonlyRoot bool  `json:"ReadonlyRootfs"`
			PortBindings map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			} `json:"PortBindings"`
			Tmpfs map[string]string `json:"Tmpfs"`
		} `json:"HostConfig"`
	}
	if json.Unmarshal(output, &actual) != nil || actual.Config.Labels == nil {
		return ErrJobServicesUnavailable
	}
	labels := actual.Config.Labels
	wantTmpfs := make(map[string]string)
	for _, entry := range jobServiceTmpfs(template) {
		mountPath, options, ok := strings.Cut(entry, ":")
		if !ok {
			return ErrJobServicesUnavailable
		}
		wantTmpfs[mountPath] = options
	}
	wantPorts := make(map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	})
	for _, port := range template.Ports {
		key := fmt.Sprintf("%d/%s", port.Number, port.Protocol)
		wantPorts[key] = []struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		}{{HostIP: "127.0.0.1", HostPort: strconv.Itoa(int(port.Number))}}
	}
	if labels["hako.managed-by"] != "hako" || labels[jobScopeLabel] != owner || labels["hako.service-name"] != template.Name ||
		actual.Config.User != "999:999" || actual.HostConfig.Memory != template.Resources.MemoryBytes ||
		actual.HostConfig.NanoCPUs != int64(template.Resources.MilliCPU)*1_000_000 || actual.HostConfig.PidsLimit != 256 ||
		!actual.HostConfig.ReadonlyRoot || !reflect.DeepEqual(actual.HostConfig.PortBindings, wantPorts) ||
		!jobServiceTmpfsMatches(actual.HostConfig.Tmpfs, wantTmpfs) {
		return ErrJobServicesUnavailable
	}
	return nil
}

func jobServiceTmpfsMatches(actual, expected map[string]string) bool {
	if len(actual) != len(expected) {
		return false
	}
	for path, options := range expected {
		actualOptions, ok := actual[path]
		if !ok {
			return false
		}
		got := make(map[string]struct{})
		for _, option := range strings.Split(actualOptions, ",") {
			got[option] = struct{}{}
		}
		for _, option := range strings.Split(options, ",") {
			if _, ok := got[option]; !ok {
				return false
			}
			delete(got, option)
		}
		for option := range got {
			if option != "rprivate" && option != "tmpcopyup" {
				return false
			}
		}
	}
	return true
}

func (e *NerdctlJobServiceEngine) Healthy(ctx context.Context, name string, check servicecatalog.HealthCheck) (bool, error) {
	if !e.valid() || ctx == nil || ctx.Err() != nil || !validJobServiceContainerName(name) || !validBuiltinHealthCheck(check) {
		return false, ErrJobServicesUnavailable
	}
	exists, err := e.containerExists(ctx, name)
	if err != nil || !exists {
		return false, err
	}
	args := []string{"exec", name}
	args = append(args, check.Command...)
	if _, err := e.run(ctx, args...); err != nil {
		return false, nil
	}
	return true, nil
}

func (e *NerdctlJobServiceEngine) RemoveService(ctx context.Context, name, owner string) error {
	if !e.valid() || ctx == nil || !validJobServiceContainerName(name) || !validJobScope(owner) {
		return ErrJobServiceCleanup
	}
	exists, err := e.containerExists(ctx, name)
	if err != nil {
		return ErrJobServiceCleanup
	}
	if !exists {
		return nil
	}
	labels, err := e.inspectContainerLabels(ctx, name)
	if err != nil || labels["hako.managed-by"] != "hako" || labels[jobScopeLabel] != owner {
		return ErrJobServiceCleanup
	}
	if _, err := e.run(ctx, "rm", "--force", "--time", "5", name); err != nil {
		return ErrJobServiceCleanup
	}
	exists, err = e.containerExists(ctx, name)
	if err != nil || exists {
		return ErrJobServiceCleanup
	}
	return nil
}

func (e *NerdctlJobServiceEngine) RemoveNetwork(ctx context.Context, name, owner string) error {
	if !e.valid() || ctx == nil || !validJobNetworkName(name) || !validJobScope(owner) {
		return ErrJobServiceCleanup
	}
	exists, err := e.networkExists(ctx, name)
	if err != nil {
		return ErrJobServiceCleanup
	}
	if !exists {
		return nil
	}
	internal, actualOwner, managedBy, err := e.inspectNetwork(ctx, name)
	if err != nil || !internal || actualOwner != owner || managedBy != "hako" {
		return ErrJobServiceCleanup
	}
	if _, err := e.run(ctx, "network", "rm", name); err != nil {
		return ErrJobServiceCleanup
	}
	exists, err = e.networkExists(ctx, name)
	if err != nil || exists {
		return ErrJobServiceCleanup
	}
	return nil
}

func validateJobServiceTemplate(template servicecatalog.Template, secrets map[string]string) error {
	builtin, err := servicecatalog.Resolve(template.Name)
	if err != nil {
		return ErrJobServicesUnavailable
	}
	want, err := servicecatalog.CanonicalJSON(builtin)
	if err != nil {
		return ErrJobServicesUnavailable
	}
	got, err := servicecatalog.CanonicalJSON(template)
	if err != nil || !reflect.DeepEqual(got, want) {
		return ErrJobServicesUnavailable
	}
	if len(template.GeneratedSecrets) == 0 {
		if len(secrets) != 0 {
			return ErrJobServicesUnavailable
		}
		return nil
	}
	if template.Name != "postgres" || len(template.GeneratedSecrets) != 1 || template.GeneratedSecrets[0] != "POSTGRES_PASSWORD" ||
		len(secrets) != 1 || len(secrets["POSTGRES_PASSWORD"]) < 32 || strings.ContainsAny(secrets["POSTGRES_PASSWORD"], "\x00\r\n") {
		return ErrJobServicesUnavailable
	}
	return nil
}

func jobServiceTmpfs(template servicecatalog.Template) []string {
	switch template.Name {
	case "postgres":
		return []string{"/var/lib/postgresql:rw,noexec,nosuid,nodev,size=805306368,mode=1777",
			"/run/postgresql:rw,noexec,nosuid,nodev,size=16m,mode=1777", "/tmp:rw,noexec,nosuid,nodev,size=16m,mode=1777"}
	case "redis":
		return []string{"/data:rw,noexec,nosuid,nodev,size=64m,mode=1777", "/tmp:rw,noexec,nosuid,nodev,size=16m,mode=1777"}
	default:
		return nil
	}
}

func validBuiltinHealthCheck(check servicecatalog.HealthCheck) bool {
	for _, template := range servicecatalog.List() {
		if reflect.DeepEqual(template.HealthCheck, check) {
			return true
		}
	}
	return false
}

func (e *NerdctlJobServiceEngine) writeSecretEnvFile(secrets map[string]string) (string, error) {
	info, err := os.Lstat(e.config.EnvFileDirectory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 ||
		e.isTmpfs == nil || !e.isTmpfs(e.config.EnvFileDirectory) {
		return "", ErrJobServicesUnavailable
	}
	file, err := os.CreateTemp(e.config.EnvFileDirectory, "hako-job-service-env-")
	if err != nil {
		return "", ErrJobServicesUnavailable
	}
	path := file.Name()
	cleanupFailed := func() error {
		_ = file.Close()
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return ErrJobServiceCleanup
		}
		return ErrJobServicesUnavailable
	}
	if err := file.Chmod(0600); err != nil {
		return "", cleanupFailed()
	}
	for _, key := range []string{"POSTGRES_PASSWORD"} {
		if value, ok := secrets[key]; ok {
			if strings.ContainsAny(value, "\x00\r\n") {
				return "", cleanupFailed()
			}
			if _, err := file.WriteString(key + "=" + value + "\n"); err != nil {
				return "", cleanupFailed()
			}
		}
	}
	if err := file.Sync(); err != nil {
		return "", cleanupFailed()
	}
	if err := file.Close(); err != nil {
		return "", cleanupFailed()
	}
	return path, nil
}

func (e *NerdctlJobServiceEngine) networkExists(ctx context.Context, name string) (bool, error) {
	output, err := e.run(ctx, "network", "ls", "--format", "{{.Name}}")
	if err != nil {
		return false, ErrJobServicesUnavailable
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
}

func (e *NerdctlJobServiceEngine) inspectNetwork(ctx context.Context, name string) (bool, string, string, error) {
	output, err := e.run(ctx, "network", "inspect", "--mode=native", "--format", "{{json .}}", name)
	if err != nil {
		return false, "", "", ErrJobServicesUnavailable
	}
	var values []struct {
		Internal bool              `json:"Internal"`
		Labels   map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(output, &values); err != nil || len(values) != 1 {
		return false, "", "", ErrJobServicesUnavailable
	}
	return values[0].Internal, values[0].Labels[jobScopeLabel], values[0].Labels["hako.managed-by"], nil
}

func (e *NerdctlJobServiceEngine) containerExists(ctx context.Context, name string) (bool, error) {
	output, err := e.run(ctx, "ps", "--all", "--filter", "name=^"+name+"$", "--format", "{{.Names}}")
	if err != nil {
		return false, ErrJobServicesUnavailable
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
}

func (e *NerdctlJobServiceEngine) inspectContainerLabels(ctx context.Context, name string) (map[string]string, error) {
	output, err := e.run(ctx, "inspect", "--format", "{{json .Config.Labels}}", name)
	if err != nil {
		return nil, ErrJobServicesUnavailable
	}
	var labels map[string]string
	if err := json.Unmarshal(output, &labels); err != nil || labels == nil {
		return nil, ErrJobServicesUnavailable
	}
	return labels, nil
}

func (e *NerdctlJobServiceEngine) run(ctx context.Context, args ...string) ([]byte, error) {
	base := []string{"--address", e.config.ContainerdSocket, "--namespace", jobNerdctlNamespace, "--cgroup-manager=systemd"}
	return e.runner.Run(ctx, e.config.BinaryPath, append(base, args...), append([]string(nil), e.env...))
}

func (e *NerdctlJobServiceEngine) valid() bool {
	return e != nil && e.runner != nil && e.config.BinaryPath != "" && e.config.ContainerdSocket != "" && e.config.RuntimeDirectory != "" && e.config.EnvFileDirectory != ""
}

func validJobScope(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func validJobNetworkName(value string) bool {
	return len(value) == len("hako-jn-")+24 && strings.HasPrefix(value, "hako-jn-") && isLowerHex(value[len("hako-jn-"):])
}

func validJobServiceContainerName(value string) bool {
	const prefix = "hako-js-"
	if !strings.HasPrefix(value, prefix) || len(value) < len(prefix)+20+2 {
		return false
	}
	rest := value[len(prefix):]
	if len(rest) != 20+1+len("postgres") && len(rest) != 20+1+len("redis") {
		return false
	}
	return isLowerHex(rest[:20]) && rest[20] == '-' && (rest[21:] == "postgres" || rest[21:] == "redis")
}

func isLowerHex(value string) bool {
	for _, character := range value {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return false
		}
	}
	return value != ""
}

var _ JobServiceEngine = (*NerdctlJobServiceEngine)(nil)
