// hako-agent runs inside a Lambda MicroVM. It reads only image-provisioned
// public verification keys; ticket signing and all AWS credentials remain in
// the Resource Plane Controller.
package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mo3789530/hako/internal/workloadagent"
)

const agentListenAddress = "0.0.0.0:8080"

func main() {
	if err := run(); err != nil {
		log.Fatal("Hako Agent stopped (failure_class=process_exit)")
	}
}

func run() error {
	keyPath := strings.TrimSpace(os.Getenv("HAKO_AGENT_TRUSTED_KEYS_FILE"))
	sourceDirectory := strings.TrimSpace(os.Getenv("HAKO_AGENT_SOURCE_DIRECTORY"))
	if keyPath == "" || sourceDirectory == "" {
		return errors.New("HAKO_AGENT_TRUSTED_KEYS_FILE and HAKO_AGENT_SOURCE_DIRECTORY are required")
	}
	egressCtx, egressCancel := context.WithTimeout(context.Background(), 10*time.Second)
	egressErr := (workloadagent.LinuxJobEgressPolicy{}).Enforce(egressCtx)
	egressCancel()
	if egressErr != nil {
		return errors.New("install required Job egress policy")
	}
	keyFile, err := os.Open(keyPath)
	if err != nil {
		return errors.New("open image-provisioned Hako Agent public keys")
	}
	keys, keyErr := workloadagent.LoadTrustedKeys(keyFile)
	closeErr := keyFile.Close()
	if keyErr != nil || closeErr != nil {
		return errors.New("load image-provisioned Hako Agent public keys")
	}
	jobServices, jobContainerd, err := configureJobServices()
	if err != nil {
		return errors.New("configure Job Service runtime")
	}
	jobCgroupRoot := strings.TrimSpace(os.Getenv("HAKO_JOB_CGROUP_ROOT"))
	if jobCgroupRoot == "" {
		jobCgroupRoot = "/sys/fs/cgroup"
	}
	nativeAgentEnabled, err := parseNativeAgentEnabled(os.Getenv("HAKO_NATIVE_AGENT_ENABLED"))
	if err != nil {
		return errors.New("configure Native Agent execution")
	}
	processFactory, err := configureProcessFactory(nativeAgentEnabled, strings.TrimSpace(os.Getenv("HAKO_NATIVE_AGENT_MODEL")),
		jobServices, jobCgroupRoot, nil)
	if err != nil {
		return errors.New("configure workload process factory")
	}
	if jobContainerd != nil {
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := jobContainerd.Close(ctx); err != nil {
				log.Printf("Job rootless containerd shutdown did not finish cleanly")
			}
		}()
	}
	agent, err := workloadagent.NewFilesystemAgent(workloadagent.FilesystemAgentConfig{
		SourceDirectory:       sourceDirectory,
		TrustedKeys:           keys,
		RequireRuntimeBinding: true,
		AllowJobServices:      jobServices != nil,
		ProcessFactory:        processFactory,
	})
	if err != nil {
		return errors.New("configure Hako Agent")
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := agent.Close(ctx); err != nil {
			log.Printf("Hako Agent process shutdown did not finish cleanly")
		}
	}()

	listener, err := net.Listen("tcp", agentListenAddress)
	if err != nil {
		return errors.New("listen on the private Lambda MicroVM Agent port")
	}
	server := &http.Server{Handler: agent.Handler(), ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case err := <-serveResult:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("Hako Agent HTTP listener stopped unexpectedly")
		}
		return nil
	case <-signals:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
			return errors.New("shut down Hako Agent HTTP listener")
		}
		return nil
	}
}

func configureJobServices() (workloadagent.JobServiceProvisioner, *workloadagent.JobRootlessContainerd, error) {
	enabled, err := parseJobServicesEnabled(os.Getenv("HAKO_JOB_SERVICES_ENABLED"))
	if err != nil {
		return nil, nil, err
	}
	if !enabled {
		return nil, nil, nil
	}
	daemon, err := workloadagent.StartLinuxJobRootlessContainerd(context.Background(), workloadagent.JobRootlessContainerdConfig{})
	if err != nil {
		return nil, nil, errors.New("start dedicated Job rootless containerd")
	}
	engine, err := workloadagent.NewNerdctlJobServiceEngine(workloadagent.NerdctlJobServiceEngineConfig{
		BinaryPath: "/usr/local/bin/nerdctl", ContainerdSocket: workloadagent.JobContainerdSocketPath,
		RuntimeDirectory: workloadagent.JobContainerdRuntimeDir, EnvFileDirectory: workloadagent.JobContainerdEnvFileDir,
	})
	if err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = daemon.Close(ctx)
		return nil, nil, errors.New("configure dedicated Job container engine")
	}
	manager, err := workloadagent.NewCatalogJobServiceManager(engine, nil, nil)
	if err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = daemon.Close(ctx)
		return nil, nil, errors.New("configure Job Service provisioner")
	}
	return manager, daemon, nil
}

func parseJobServicesEnabled(value string) (bool, error) {
	switch strings.TrimSpace(value) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errors.New("HAKO_JOB_SERVICES_ENABLED must be true or false")
	}
}

func parseNativeAgentEnabled(value string) (bool, error) {
	switch strings.TrimSpace(value) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errors.New("HAKO_NATIVE_AGENT_ENABLED must be true or false")
	}
}

func configureProcessFactory(nativeAgentEnabled bool, modelName string, jobServices workloadagent.JobServiceProvisioner,
	cgroupRoot string, modelDoer workloadagent.AnthropicMessagesDoer) (workloadagent.ProcessFactory, error) {
	resourceLimits := workloadagent.LinuxJobCgroupManager{Root: cgroupRoot}
	jobs := workloadagent.GoTestProcessFactory{
		GoBinary: "/usr/local/go/bin/go", GoCache: "/tmp/hako-go-cache",
		ModuleCache: strings.TrimSpace(os.Getenv("HAKO_GO_MODULE_CACHE")), JobServices: jobServices,
		EgressPolicy: workloadagent.LinuxJobEgressPolicy{}, ResourceLimits: resourceLimits,
		CommandUID: 65532, CommandGID: 65532,
	}
	if !nativeAgentEnabled {
		return jobs, nil
	}
	if modelName == "" {
		return nil, errors.New("HAKO_NATIVE_AGENT_MODEL is required when Native Agent execution is enabled")
	}
	model, err := workloadagent.NewAnthropicMessagesClient(modelName, modelDoer)
	if err != nil {
		return nil, errors.New("configure fixed Native Agent model endpoint")
	}
	agent := workloadagent.NativeAgentProcessFactory{Model: model, Tests: workloadagent.GoTestNativeAgentVerifier{
		GoBinary: "/usr/local/go/bin/go", GoCache: "/tmp/hako-agent-go-cache",
		ModuleCache: strings.TrimSpace(os.Getenv("HAKO_GO_MODULE_CACHE")), CommandUID: 65532, CommandGID: 65532,
		MemoryMB: 2048, CPUMillis: 1000, ResourceLimits: resourceLimits,
	}}
	return workloadProcessFactory{jobs: jobs, agent: agent}, nil
}

type workloadProcessFactory struct {
	jobs  workloadagent.GoTestProcessFactory
	agent workloadagent.NativeAgentProcessFactory
}

func (f workloadProcessFactory) Start(ctx context.Context, plan workloadagent.ExecutionPlan, output io.Writer) (workloadagent.ManagedProcess, error) {
	if plan.Kind == "agent" {
		return f.agent.Start(ctx, plan, output)
	}
	return f.jobs.Start(ctx, plan, output)
}

func (f workloadProcessFactory) SupportsJobServices() bool { return f.jobs.SupportsJobServices() }

func (workloadProcessFactory) SupportsNativeAgentCredential() bool { return true }
