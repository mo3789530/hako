//go:build linux

package workloadagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/mo3789530/hako/internal/servicecatalog"
)

type fakeJobNerdctlRunner struct {
	commands              [][]string
	networkName           string
	networkOwner          string
	containerName         string
	containerLabel        map[string]string
	containerInspect      map[string]any
	networkInternal       bool
	envFileContents       string
	envFilePath           string
	failRun               bool
	inspectMemoryOverride int64
	inspectPrivileged     bool
	inspectCapAdd         []string
}

func (r *fakeJobNerdctlRunner) Run(_ context.Context, _ string, args, _ []string) ([]byte, error) {
	r.commands = append(r.commands, append([]string(nil), args...))
	command := args[jobNerdctlGlobalArgCount:]
	switch {
	case len(command) >= 3 && command[0] == "network" && command[1] == "ls":
		if r.networkName == "" {
			return []byte("NAME\n"), nil
		}
		return []byte(r.networkName + "\n"), nil
	case len(command) >= 4 && command[0] == "network" && command[1] == "inspect":
		if command[len(command)-1] != r.networkName || r.networkName == "" {
			return nil, errors.New("missing network")
		}
		payload, _ := json.Marshal([]map[string]any{{"Internal": r.networkInternal,
			"Labels": map[string]string{"hako.managed-by": "hako", jobScopeLabel: r.networkOwner}}})
		return payload, nil
	case len(command) > 0 && command[0] == "network" && command[1] == "create":
		for i := 2; i < len(command)-1; i++ {
			if command[i] == "--label" && strings.HasPrefix(command[i+1], jobScopeLabel+"=") {
				r.networkOwner = strings.TrimPrefix(command[i+1], jobScopeLabel+"=")
			}
		}
		r.networkName = command[len(command)-1]
		r.networkInternal = containsArg(command, "--internal")
		return []byte(r.networkName + "\n"), nil
	case len(command) >= 2 && command[0] == "network" && command[1] == "rm":
		r.networkName = ""
		return []byte("\n"), nil
	case len(command) > 0 && command[0] == "ps":
		if r.containerName != "" && commandContains(command, "name=^"+r.containerName+"$") {
			return []byte(r.containerName + "\n"), nil
		}
		return []byte("\n"), nil
	case len(command) >= 3 && command[0] == "inspect":
		if command[1] == "--format" && command[2] == "{{json .Config.Labels}}" {
			payload, _ := json.Marshal(r.containerLabel)
			return payload, nil
		}
		payload, _ := json.Marshal(r.containerInspect)
		return payload, nil
	case len(command) > 0 && command[0] == "run":
		r.containerName = argValue(command, "--name")
		r.containerLabel = make(map[string]string)
		portBindings := make(map[string][]map[string]string)
		tmpfs := make(map[string]string)
		for i := 0; i < len(command)-1; i++ {
			if command[i] == "--label" {
				parts := strings.SplitN(command[i+1], "=", 2)
				if len(parts) == 2 {
					r.containerLabel[parts[0]] = parts[1]
				}
			}
			if command[i] == "--env-file" {
				r.envFilePath = command[i+1]
				content, err := os.ReadFile(command[i+1])
				if err != nil {
					return nil, err
				}
				r.envFileContents = string(content)
			}
			if command[i] == "--publish" {
				parts := strings.Split(command[i+1], ":")
				port := strings.Split(parts[2], "/")
				portBindings[port[0]+"/"+port[1]] = []map[string]string{{"HostIp": parts[0], "HostPort": parts[1]}}
			}
			if command[i] == "--tmpfs" {
				mount, options, _ := strings.Cut(command[i+1], ":")
				tmpfs[mount] = options
			}
		}
		if r.failRun {
			return nil, errors.New("synthetic container create failure")
		}
		memory, _ := strconv.ParseInt(argValue(command, "--memory"), 10, 64)
		if r.inspectMemoryOverride != 0 {
			memory = r.inspectMemoryOverride
		}
		cpu, _ := strconv.ParseFloat(argValue(command, "--cpus"), 64)
		pids, _ := strconv.ParseInt(argValue(command, "--pids-limit"), 10, 64)
		capAdd := r.inspectCapAdd
		if capAdd == nil {
			capAdd = []string{}
		}
		r.containerInspect = map[string]any{
			"Config": map[string]any{"User": argValue(command, "--user"), "Labels": r.containerLabel},
			"HostConfig": map[string]any{"Memory": memory, "NanoCpus": int64(cpu * 1_000_000_000),
				"PidsLimit": pids, "ReadonlyRootfs": containsArg(command, "--read-only"),
				"Privileged": r.inspectPrivileged, "CapAdd": capAdd, "SecurityOpt": []string{"no-new-privileges:true"},
				"Binds":        []string{},
				"PortBindings": portBindings, "Tmpfs": tmpfs},
		}
		return []byte("container-id\n"), nil
	case len(command) > 0 && command[0] == "rm":
		r.containerName = ""
		r.containerLabel = nil
		r.containerInspect = nil
		return []byte("\n"), nil
	case len(command) > 1 && command[0] == "exec":
		return []byte("ok\n"), nil
	default:
		return nil, errors.New("unexpected nerdctl invocation")
	}
}

func (r *fakeJobNerdctlRunner) RunInput(context.Context, string, []string, []string, []byte) ([]byte, error) {
	return nil, errors.New("RunInput must not be used for service credentials")
}

func TestNerdctlJobServiceEngineUsesInternalEphemeralLoopbackRuntime(t *testing.T) {
	const owner = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const network = "hako-jn-0123456789abcdef01234567"
	const container = "hako-js-0123456789abcdef0123-postgres"
	runner := &fakeJobNerdctlRunner{}
	engine, err := NewNerdctlJobServiceEngine(NerdctlJobServiceEngineConfig{ContainerdSocket: "/run/user/65532/containerd/containerd.sock",
		RuntimeDirectory: "/run/user/65532", EnvFileDirectory: t.TempDir(), Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	engine.isTmpfs = func(string) bool { return true }
	if err := engine.EnsureNetwork(context.Background(), network, owner); err != nil {
		t.Fatal(err)
	}
	template, _ := servicecatalog.Resolve("postgres")
	secret := "abcdefghijklmnopqrstuvwxyz0123456789ABCDEF"
	if err := engine.EnsureService(context.Background(), container, network, owner, template, map[string]string{"POSTGRES_PASSWORD": secret}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(runner.envFileContents, "POSTGRES_PASSWORD="+secret+"\n") {
		t.Fatal("generated credential was not supplied through the private env file")
	}
	if len(runner.commands) < 4 {
		t.Fatalf("nerdctl command sequence is incomplete: %v", runner.commands)
	}
	for _, command := range runner.commands {
		if len(command) < jobNerdctlGlobalArgCount || command[jobNerdctlGlobalArgCount-1] != "--cgroup-manager=systemd" {
			t.Fatalf("Job Service command did not require systemd cgroup limits: %v", command)
		}
	}
	createNetwork := findJobCommand(t, runner.commands, "network", "create")
	runService := findJobCommand(t, runner.commands, "run")
	if !containsArg(createNetwork, "--internal") || !containsArg(runService, "--read-only") ||
		!containsArg(runService, "--pull=never") || !containsArg(runService, "--cap-drop=ALL") ||
		argValue(runService, "--user") != "999:999" ||
		!commandContains(runService, "127.0.0.1:5432:5432/tcp") ||
		!commandContains(runService, "/var/lib/postgresql:rw,noexec,nosuid,nodev,size=805306368,mode=1777") {
		t.Fatalf("service runtime is missing isolation controls: create=%v run=%v", createNetwork, runService)
	}
	for _, arg := range runService {
		if strings.Contains(arg, secret) || arg == "--volume" || strings.Contains(arg, ":/var/lib/postgresql") {
			t.Fatalf("credential or persistent mount leaked into nerdctl arguments: %v", runService)
		}
	}
	if _, err := os.Stat(argValue(runService, "--env-file")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary credential file was not removed after container create: %v", err)
	}
	if healthy, err := engine.Healthy(context.Background(), container, template.HealthCheck); err != nil || !healthy {
		t.Fatalf("catalog health check failed: healthy=%t err=%v", healthy, err)
	}
	if err := engine.RemoveService(context.Background(), container, owner); err != nil {
		t.Fatal(err)
	}
	if err := engine.RemoveNetwork(context.Background(), network, owner); err != nil {
		t.Fatal(err)
	}
}

func TestNerdctlJobServiceEngineRefusesForeignOwnershipAndMutableCatalog(t *testing.T) {
	const owner = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const network = "hako-jn-0123456789abcdef01234567"
	const container = "hako-js-0123456789abcdef0123-redis"
	runner := &fakeJobNerdctlRunner{networkName: network, networkOwner: strings.Repeat("f", 64), networkInternal: true}
	engine, err := NewNerdctlJobServiceEngine(NerdctlJobServiceEngineConfig{ContainerdSocket: "/run/user/65532/containerd/containerd.sock",
		RuntimeDirectory: "/run/user/65532", EnvFileDirectory: t.TempDir(), Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.EnsureNetwork(context.Background(), network, owner); !errors.Is(err, ErrJobServicesUnavailable) {
		t.Fatalf("foreign network was adopted: %v", err)
	}
	template, _ := servicecatalog.Resolve("redis")
	template.Image = "docker.io/library/redis:latest"
	if err := engine.EnsureService(context.Background(), container, network, owner, template, nil); !errors.Is(err, ErrJobServicesUnavailable) {
		t.Fatalf("mutable service image was accepted: %v", err)
	}
	if len(runner.commands) != 2 || commandPrefixSeen(runner.commands, "network", "create") || commandPrefixSeen(runner.commands, "run") {
		t.Fatalf("invalid service caused runtime mutations: %v", runner.commands)
	}
}

func TestNerdctlJobServiceEngineRejectsRuntimeThatIgnoresResourceLimit(t *testing.T) {
	const owner = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const network = "hako-jn-0123456789abcdef01234567"
	const container = "hako-js-0123456789abcdef0123-redis"
	mutations := []struct {
		name   string
		mutate func(*fakeJobNerdctlRunner)
	}{
		{name: "memory", mutate: func(runner *fakeJobNerdctlRunner) { runner.inspectMemoryOverride = 1 << 30 }},
		{name: "privileged", mutate: func(runner *fakeJobNerdctlRunner) { runner.inspectPrivileged = true }},
		{name: "added capability", mutate: func(runner *fakeJobNerdctlRunner) { runner.inspectCapAdd = []string{"SYS_ADMIN"} }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			runner := &fakeJobNerdctlRunner{networkName: network, networkOwner: owner, networkInternal: true}
			mutation.mutate(runner)
			engine, err := NewNerdctlJobServiceEngine(NerdctlJobServiceEngineConfig{ContainerdSocket: "/run/user/65532/containerd/containerd.sock",
				RuntimeDirectory: "/run/user/65532", EnvFileDirectory: t.TempDir(), Runner: runner})
			if err != nil {
				t.Fatal(err)
			}
			engine.isTmpfs = func(string) bool { return true }
			if err := engine.EnsureService(context.Background(), container, network, owner, mustResolveService(t, "redis"), nil); !errors.Is(err, ErrJobServicesUnavailable) {
				t.Fatalf("service with mismatched inspected settings was accepted: %v", err)
			}
			if !commandPrefixSeen(runner.commands, "run") || !commandPrefixSeen(runner.commands, "inspect") {
				t.Fatalf("runtime settings were not checked after creation: %v", runner.commands)
			}
			if runner.containerName != "" {
				t.Fatal("service with unverified runtime settings was left running")
			}
		})
	}
}

func TestNerdctlJobServiceTemplateRequiresExactGeneratedSecretSet(t *testing.T) {
	postgres, _ := servicecatalog.Resolve("postgres")
	redis, _ := servicecatalog.Resolve("redis")
	if err := validateJobServiceTemplate(postgres, map[string]string{"POSTGRES_PASSWORD": strings.Repeat("x", 32)}); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []map[string]string{
		nil,
		{"POSTGRES_PASSWORD": "short"},
		{"POSTGRES_PASSWORD": strings.Repeat("x", 32), "EXTRA": "value"},
		{"POSTGRES_PASSWORD": "contains\nnewline"},
	} {
		if err := validateJobServiceTemplate(postgres, invalid); err == nil {
			t.Fatalf("invalid generated credential set accepted: %#v", invalid)
		}
	}
	if err := validateJobServiceTemplate(redis, map[string]string{"POSTGRES_PASSWORD": strings.Repeat("x", 32)}); err == nil {
		t.Fatal("PostgreSQL credential was accepted for Redis")
	}
}

func TestNerdctlJobServiceEngineRemovesCredentialFileWhenCreateFails(t *testing.T) {
	const owner = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const network = "hako-jn-0123456789abcdef01234567"
	const container = "hako-js-0123456789abcdef0123-postgres"
	runner := &fakeJobNerdctlRunner{networkName: network, networkOwner: owner, networkInternal: true, failRun: true}
	engine, err := NewNerdctlJobServiceEngine(NerdctlJobServiceEngineConfig{ContainerdSocket: "/run/user/65532/containerd/containerd.sock",
		RuntimeDirectory: "/run/user/65532", EnvFileDirectory: t.TempDir(), Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	engine.isTmpfs = func(string) bool { return true }
	postgres := mustResolveService(t, "postgres")
	if err := engine.EnsureService(context.Background(), container, network, owner, postgres,
		map[string]string{"POSTGRES_PASSWORD": strings.Repeat("x", 40)}); !errors.Is(err, ErrJobServicesUnavailable) {
		t.Fatalf("synthetic run failure was not surfaced: %v", err)
	}
	if runner.envFilePath == "" {
		t.Fatal("service create did not use its private temporary credential file")
	}
	if _, err := os.Stat(runner.envFilePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary credential file survived failed create: %v", err)
	}
}

func TestNerdctlJobServiceEngineRequiresTmpfsForCredentialFile(t *testing.T) {
	const owner = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const network = "hako-jn-0123456789abcdef01234567"
	const container = "hako-js-0123456789abcdef0123-postgres"
	runner := &fakeJobNerdctlRunner{networkName: network, networkOwner: owner, networkInternal: true}
	envDirectory := t.TempDir()
	engine, err := NewNerdctlJobServiceEngine(NerdctlJobServiceEngineConfig{ContainerdSocket: "/run/user/65532/containerd/containerd.sock",
		RuntimeDirectory: "/run/user/65532", EnvFileDirectory: envDirectory, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	engine.isTmpfs = func(string) bool { return false }
	postgres := mustResolveService(t, "postgres")
	if err := engine.EnsureService(context.Background(), container, network, owner, postgres,
		map[string]string{"POSTGRES_PASSWORD": strings.Repeat("x", 40)}); !errors.Is(err, ErrJobServicesUnavailable) {
		t.Fatalf("credential file on a non-tmpfs path was accepted: %v", err)
	}
	entries, err := os.ReadDir(envDirectory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed tmpfs validation left credential material: entries=%v err=%v", entries, err)
	}
	if commandPrefixSeen(runner.commands, "run") {
		t.Fatal("container was created without a verified tmpfs credential file")
	}
}

func TestJobServiceRuntimeArgumentNamesAreExact(t *testing.T) {
	if !validJobNetworkName("hako-jn-0123456789abcdef01234567") || validJobNetworkName("hako-jn-0123456789abcdef01234567-extra") ||
		!validJobServiceContainerName("hako-js-0123456789abcdef0123-postgres") || validJobServiceContainerName("hako-js-0123456789abcdef0123-arbitrary") {
		t.Fatal("Job Service runtime accepted an out-of-scope runtime name")
	}
	if !reflect.DeepEqual(jobServiceTmpfs(mustResolveService(t, "redis")), []string{"/data:rw,noexec,nosuid,nodev,size=64m,mode=1777", "/tmp:rw,noexec,nosuid,nodev,size=16m,mode=1777"}) {
		t.Fatal("Redis state is not confined to the expected ephemeral tmpfs")
	}
}

func mustResolveService(t *testing.T, name string) servicecatalog.Template {
	t.Helper()
	template, err := servicecatalog.Resolve(name)
	if err != nil {
		t.Fatal(err)
	}
	return template
}

func containsArg(args []string, want string) bool { return commandContains(args, want) }

func commandContains(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func commandPrefixSeen(commands [][]string, prefix ...string) bool {
	for _, command := range commands {
		if len(command) < jobNerdctlGlobalArgCount+len(prefix) {
			continue
		}
		matched := true
		for i, value := range prefix {
			if command[jobNerdctlGlobalArgCount+i] != value {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func argValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func findJobCommand(t *testing.T, commands [][]string, prefix ...string) []string {
	t.Helper()
	for _, command := range commands {
		if len(command) < jobNerdctlGlobalArgCount+len(prefix) {
			continue
		}
		matched := true
		for i, value := range prefix {
			if command[jobNerdctlGlobalArgCount+i] != value {
				matched = false
				break
			}
		}
		if matched {
			return command[4:]
		}
	}
	t.Fatalf("nerdctl command not found: prefix=%v commands=%v", prefix, commands)
	return nil
}

var _ JobServiceCommandRunner = (*fakeJobNerdctlRunner)(nil)
