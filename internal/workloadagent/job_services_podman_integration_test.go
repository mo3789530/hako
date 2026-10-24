//go:build integration

package workloadagent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/mo3789530/hako/internal/factoryconfig"
)

// podmanJobServiceCommandRunner adapts the production nerdctl command contract
// to the host's rootless Podman for local container acceptance. It intentionally
// remaps the fixed MicroVM loopback port when the developer DB already owns it
// and substitutes a locally available architecture-compatible test image.
type podmanJobServiceCommandRunner struct {
	image string
	port  string
}

func (r *podmanJobServiceCommandRunner) Run(ctx context.Context, binary string, args, _ []string) ([]byte, error) {
	return r.run(ctx, binary, args, nil)
}

func (r *podmanJobServiceCommandRunner) RunInput(ctx context.Context, binary string, args, _ []string, input []byte) ([]byte, error) {
	return r.run(ctx, binary, args, input)
}

func (r *podmanJobServiceCommandRunner) run(ctx context.Context, binary string, args []string, input []byte) ([]byte, error) {
	if len(args) < 5 || args[0] != "--address" || args[2] != "--namespace" || args[4] != "--cgroup-manager=systemd" {
		return nil, errors.New("unexpected nerdctl global options")
	}
	args = append([]string(nil), args[5:]...)
	if len(args) >= 3 && args[0] == "network" && args[1] == "inspect" && args[2] == "--mode=native" {
		args = append(args[:2], args[3:]...)
	}
	for i := 0; i < len(args); i++ {
		if args[i] == "--publish" && i+1 < len(args) && args[i+1] == "127.0.0.1:5432:5432/tcp" {
			args[i+1] = "127.0.0.1:" + r.port + ":5432/tcp"
		}
		if strings.HasPrefix(args[i], "docker.io/library/postgres@sha256:") {
			args[i] = r.image
		}
	}
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = podmanIntegrationEnvironment()
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		// Keep this integration helper from surfacing service env or runtime output.
		return nil, errors.New("Podman Job Service command failed")
	}
	if len(args) >= 2 && args[0] == "network" && args[1] == "inspect" {
		var inspection struct {
			Internal bool              `json:"internal"`
			Labels   map[string]string `json:"labels"`
		}
		if err := json.Unmarshal(output, &inspection); err != nil {
			return nil, errors.New("Podman network inspection output is invalid")
		}
		output, err = json.Marshal([]struct {
			Internal bool              `json:"Internal"`
			Labels   map[string]string `json:"Labels"`
		}{{Internal: inspection.Internal, Labels: inspection.Labels}})
		if err != nil {
			return nil, errors.New("Podman network inspection conversion failed")
		}
	}
	if len(args) >= 3 && args[0] == "inspect" && args[1] == "--format" && args[2] == "{{json .}}" {
		var inspection map[string]any
		if err := json.Unmarshal(output, &inspection); err != nil {
			return nil, errors.New("Podman container inspection output is invalid")
		}
		hostConfig, ok := inspection["HostConfig"].(map[string]any)
		if !ok {
			return nil, errors.New("Podman container host configuration is missing")
		}
		bindings, ok := hostConfig["PortBindings"].(map[string]any)
		if !ok {
			return nil, errors.New("Podman container port bindings are missing")
		}
		for _, entries := range bindings {
			list, ok := entries.([]any)
			if !ok {
				return nil, errors.New("Podman container port binding entries are invalid")
			}
			for _, entry := range list {
				binding, ok := entry.(map[string]any)
				if !ok {
					return nil, errors.New("Podman container port binding is invalid")
				}
				binding["HostPort"] = "5432"
			}
		}
		output, err = json.Marshal(inspection)
		if err != nil {
			return nil, errors.New("Podman container inspection conversion failed")
		}
	}
	return output, nil
}

func podmanIntegrationEnvironment() []string {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "XDG_RUNTIME_DIR=" + runtimeDir}
}

func TestNerdctlJobServicePodmanRuntimeContract(t *testing.T) {
	if os.Getenv("HAKO_JOB_SERVICES_PODMAN_TEST") != "1" {
		t.Skip("set HAKO_JOB_SERVICES_PODMAN_TEST=1 to run the rootless Podman acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	podman, err := exec.LookPath("podman")
	if err != nil {
		t.Fatal("Podman is required for this explicit integration test")
	}
	podman, err = filepath.Abs(podman)
	if err != nil {
		t.Fatal(err)
	}
	image := strings.TrimSpace(os.Getenv("HAKO_JOB_SERVICES_PODMAN_IMAGE"))
	if image == "" {
		image = "docker.io/library/postgres:18"
	}
	if output, err := exec.CommandContext(ctx, podman, "image", "exists", image).CombinedOutput(); err != nil {
		t.Fatalf("local PostgreSQL test image %q is unavailable; pull it before running this offline test: %s", image, strings.TrimSpace(string(output)))
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	envDir, err := os.MkdirTemp("/dev/shm", "hako-job-service-env-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(envDir)
	runner := &podmanJobServiceCommandRunner{image: image, port: port}
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	engine, err := NewNerdctlJobServiceEngine(NerdctlJobServiceEngineConfig{
		BinaryPath: podman, ContainerdSocket: filepath.Join(runtimeDir, "podman-compat.sock"),
		RuntimeDirectory: runtimeDir, EnvFileDirectory: envDir, Runner: runner,
	})
	if err != nil {
		t.Fatal("configure Job Service engine", err)
	}
	manager, err := NewCatalogJobServiceManager(engine, strings.NewReader(strings.Repeat("p", 64)), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	identity := testIdentity()
	pipeline := factoryconfig.ExecutionPlan{Pipeline: "podman-integration", RuntimeClass: "standard", TimeoutSecond: 60,
		MemoryMB: 2048, CPUMillis: 1500, Network: "registry-only", Services: []string{"postgres"},
		Commands: []factoryconfig.Command{{Step: "go-test", Executable: "/usr/local/go/bin/go", Args: []string{"test", "./..."}}}}
	session, err := manager.Start(ctx, identity, pipeline)
	if err != nil {
		t.Fatal("start catalog PostgreSQL service", err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer closeCancel()
	defer func() {
		if err := session.Close(closeCtx); err != nil {
			t.Errorf("remove Job Service runtime: %v", err)
		}
	}()
	if len(session.SecretValues()) != 1 || !strings.Contains(strings.Join(session.Environment(), "\n"), "PGHOST=127.0.0.1") {
		t.Fatal("Job Service session did not produce its attempt-scoped PostgreSQL environment")
	}
	jobEnvironment := make(map[string]string)
	for _, entry := range session.Environment() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			jobEnvironment[key] = value
		}
	}
	databaseURL, err := url.Parse(jobEnvironment["DATABASE_URL"])
	if err != nil || databaseURL.User == nil || databaseURL.User.Username() != "postgres" ||
		jobEnvironment["PGPASSWORD"] == "" || jobEnvironment["PGPASSWORD"] != session.SecretValues()[0] {
		t.Fatal("Job Service generated credential environment is incomplete")
	}
	databaseURL.Host = net.JoinHostPort("127.0.0.1", port)
	db, err := sql.Open("pgx", databaseURL.String())
	if err != nil {
		t.Fatal("configure local Job Service connection")
	}
	defer db.Close()
	probeCtx, probeCancel := context.WithTimeout(ctx, 10*time.Second)
	defer probeCancel()
	if err := db.PingContext(probeCtx); err != nil {
		t.Fatal("Job Service rejected its generated credential or loopback connection")
	}
	var one int
	if err := db.QueryRowContext(probeCtx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatal("Job Service query did not complete")
	}
	// Verify the production engine's inspected container is published only on
	// host loopback; the actual host port is ephemeral for developer isolation.
	container, _ := jobServiceNames(identity, pipeline.Pipeline)
	container = "hako-js-" + container[len("hako-jn-"):][:20] + "-postgres"
	_, owner := jobServiceNames(identity, pipeline.Pipeline)
	output, err := exec.CommandContext(ctx, podman, "port", container, "5432/tcp").CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "127.0.0.1:"+port {
		t.Fatalf("Job Service port binding escaped loopback: binding=%q err=%v", strings.TrimSpace(string(output)), err)
	}
	output, err = exec.CommandContext(ctx, podman, "inspect", "--format", `{{.HostConfig.Memory}}|{{.HostConfig.NanoCpus}}|{{.HostConfig.ReadonlyRootfs}}|{{.HostConfig.PidsLimit}}|{{.Config.User}}|{{json .HostConfig.Tmpfs}}|{{index .Config.Labels "hako.job-scope-key"}}`, container).CombinedOutput()
	inspection := strings.TrimSpace(string(output))
	if err != nil || !strings.HasPrefix(inspection, "1073741824|750000000|true|256|999:999|") || !strings.HasSuffix(inspection, "|"+owner) ||
		!strings.Contains(inspection, "/var/lib/postgresql") || !strings.Contains(inspection, "/run/postgresql") {
		t.Fatalf("Job Service runtime limits or ephemeral mounts differ from the catalog contract: inspection=%q err=%v", inspection, err)
	}
	if entries, err := os.ReadDir(envDir); err != nil || len(entries) != 0 {
		t.Fatalf("temporary credential env file was not removed after container creation: entries=%d err=%v", len(entries), err)
	}
	if err := session.Close(closeCtx); err != nil {
		t.Fatal("idempotent Job Service cleanup", err)
	}
	if entries, err := os.ReadDir(envDir); err != nil || len(entries) != 0 {
		t.Fatalf("credential temp directory is not empty after cleanup: entries=%d err=%v", len(entries), err)
	}
}
