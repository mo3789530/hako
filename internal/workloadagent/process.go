package workloadagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/factoryconfig"
	"github.com/mo3789530/hako/internal/runtime/fake"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

const defaultAgentOutputLimit = 1 << 20

var (
	ErrExecutionPlan   = errors.New("trusted Workload execution plan is invalid")
	ErrFactoryManifest = errors.New("Hako factory manifest is invalid for this Run")
	ErrProcessExit     = errors.New("Workload process exited unsuccessfully")
)

type ExecutionPlan struct {
	Identity         workloadjournal.Command
	RepositoryPath   string
	SourceDigest     string
	CommitSHA        string
	TriggerEvent     string
	TriggerBranch    string
	FactoryPipelines []factoryconfig.ExecutionPlan
	Deadline         time.Time
}

// ExecutionResolver must resolve a repository path from the trusted attempt
// record. No path or command is accepted from the repository or HTTP request.
type ExecutionResolver interface {
	ResolveExecution(context.Context, TicketClaims) (ExecutionPlan, error)
}

type ManagedProcess interface {
	Wait() (int, error)
}

type ProcessFactory interface {
	Start(context.Context, ExecutionPlan, io.Writer) (ManagedProcess, error)
}

type AttemptLogWriter interface {
	PutAttemptLogs(context.Context, workloadjournal.Command, []byte, string) (workloadjobs.WorkloadLogArtifact, error)
}

type ProcessConfig struct {
	Resolver  ExecutionResolver
	Factory   ProcessFactory
	MaxOutput int
	Now       func() time.Time
}

type ProcessBackend struct {
	mu        sync.Mutex
	config    ProcessConfig
	root      context.Context
	cancel    context.CancelFunc
	attempt   *processAttempt
	waitGroup sync.WaitGroup
	closed    bool
}

type processAttempt struct {
	claims TicketClaims
	plan   ExecutionPlan
	state  workloadjournal.ExecutionState
	result workloadjobs.WorkloadResult
	logs   []byte
	output *boundedOutput
	cancel context.CancelFunc
}

func NewProcessBackend(config ProcessConfig) (*ProcessBackend, error) {
	if config.Resolver == nil {
		return nil, ErrExecutionPlan
	}
	if config.Factory == nil {
		config.Factory = GoTestProcessFactory{}
	}
	if config.MaxOutput == 0 {
		config.MaxOutput = defaultAgentOutputLimit
	}
	if config.MaxOutput < 1 || config.MaxOutput > 16<<20 {
		return nil, ErrExecutionPlan
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	root, cancel := context.WithCancel(context.Background())
	return &ProcessBackend{config: config, root: root, cancel: cancel}, nil
}

func (b *ProcessBackend) Start(ctx context.Context, claims TicketClaims, req StartRequest) error {
	if b == nil || ctx == nil || claims.Operation != OperationStart || claims.Execution == nil || claims.Identity.Command() != req.Identity.Command() || validateStart(req) != nil ||
		claims.Execution.SourceDigest != req.SourceDigest || claims.Execution.CommitSHA != req.CommitSHA || !claims.Execution.Deadline.Equal(req.Deadline) {
		return ErrProtocolRequest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.root.Err() != nil {
		return ErrAgentBusy
	}
	if b.attempt != nil {
		if b.attempt.claims.MicroVMID != claims.MicroVMID || b.attempt.claims.Identity.Command() != claims.Identity.Command() ||
			b.attempt.plan.SourceDigest != req.SourceDigest || b.attempt.plan.CommitSHA != req.CommitSHA || !b.attempt.plan.Deadline.Equal(req.Deadline) ||
			b.attempt.plan.TriggerEvent != claims.Execution.TriggerEvent || b.attempt.plan.TriggerBranch != claims.Execution.TriggerBranch {
			return ErrAttemptConflict
		}
		return nil
	}
	plan, err := b.config.Resolver.ResolveExecution(ctx, claims)
	if err != nil {
		if errors.Is(err, ErrFactoryManifest) {
			b.recordFactoryManifestFailure(claims, req)
			return nil
		}
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadlineIn := plan.Deadline.Sub(b.config.Now())
	if plan.Identity != claims.Identity.Command() || plan.SourceDigest != req.SourceDigest || plan.CommitSHA != req.CommitSHA ||
		plan.TriggerEvent != claims.Execution.TriggerEvent || plan.TriggerBranch != claims.Execution.TriggerBranch ||
		!plan.Deadline.Equal(req.Deadline) || deadlineIn <= 0 || deadlineIn > 8*time.Hour {
		return ErrExecutionPlan
	}
	processCtx, cancel := context.WithDeadline(b.root, plan.Deadline)
	output := &boundedOutput{limit: b.config.MaxOutput}
	process, err := b.config.Factory.Start(processCtx, plan, output)
	if err != nil {
		cancel()
		return ErrAgentBusy
	}
	attempt := &processAttempt{claims: claims, plan: plan, state: workloadjournal.Running,
		output: output, cancel: cancel}
	b.attempt = attempt
	b.waitGroup.Add(1)
	go b.wait(attempt, processCtx, process)
	return nil
}

func (b *ProcessBackend) recordFactoryManifestFailure(claims TicketClaims, req StartRequest) {
	completed := b.config.Now().UTC()
	output := &boundedOutput{limit: b.config.MaxOutput}
	_, _ = output.Write([]byte("[Hako] factory manifest rejected for this trigger\n"))
	identity := claims.Identity.Command()
	result := workloadjobs.WorkloadResult{SchemaVersion: 1, TenantID: domain.TenantID(identity.TenantID),
		WorkloadRunID: domain.WorkloadRunID(identity.WorkloadRunID), AttemptID: identity.AttemptID, Generation: identity.Generation,
		ResourcePlaneID: domain.ResourcePlaneID(identity.ResourcePlaneID), CommitSHA: req.CommitSHA,
		Status: domain.WorkloadFailed, ExitCode: 1, ErrorCode: "factory_manifest_invalid", CompletedAt: completed}
	b.attempt = &processAttempt{claims: claims, plan: ExecutionPlan{Identity: identity, SourceDigest: req.SourceDigest,
		CommitSHA: req.CommitSHA, Deadline: req.Deadline.UTC()}, state: workloadjournal.Failed, result: result, output: output,
		logs: []byte("[Hako] factory manifest rejected for this trigger\n")}
}

func (b *ProcessBackend) Status(_ context.Context, claims TicketClaims) (workloadjournal.ExecutionState, error) {
	if b == nil || claims.Operation != OperationStatus {
		return "", ErrAgentTicket
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	a := b.attempt
	if !matchesAttempt(a, claims) {
		return "", ErrAttemptNotFound
	}
	return a.state, nil
}

func (b *ProcessBackend) Result(ctx context.Context, claims TicketClaims) (workloadjobs.WorkloadResult, error) {
	if b == nil || ctx == nil || claims.Operation != OperationResult {
		return workloadjobs.WorkloadResult{}, ErrAgentTicket
	}
	b.mu.Lock()
	a := b.attempt
	if !matchesAttempt(a, claims) {
		b.mu.Unlock()
		return workloadjobs.WorkloadResult{}, ErrAttemptNotFound
	}
	if !terminalAgentState(a.state) {
		b.mu.Unlock()
		return workloadjobs.WorkloadResult{}, ErrAgentBusy
	}
	result := cloneWorkloadResult(a.result)
	b.mu.Unlock()
	return result, nil
}

func (b *ProcessBackend) Logs(ctx context.Context, claims TicketClaims) ([]byte, error) {
	if b == nil || ctx == nil || claims.Operation != OperationLogs {
		return nil, ErrAgentTicket
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	a := b.attempt
	if !matchesAttempt(a, claims) {
		return nil, ErrAttemptNotFound
	}
	if !terminalAgentState(a.state) {
		return nil, ErrAgentBusy
	}
	return append([]byte(nil), a.logs...), nil
}

// Result is an idempotent snapshot of the completed attempt. Copy pointer
// fields so a protocol adapter or caller cannot mutate the backend's cached
// result and change what a later retry observes.
func cloneWorkloadResult(result workloadjobs.WorkloadResult) workloadjobs.WorkloadResult {
	if result.LogArtifact != nil {
		artifact := *result.LogArtifact
		result.LogArtifact = &artifact
	}
	return result
}

func terminalAgentState(state workloadjournal.ExecutionState) bool {
	switch state {
	case workloadjournal.Succeeded, workloadjournal.Failed, workloadjournal.Cancelled, workloadjournal.TimedOut:
		return true
	default:
		return false
	}
}

func (b *ProcessBackend) Close(ctx context.Context) error {
	if b == nil || ctx == nil {
		return ErrExecutionPlan
	}
	b.mu.Lock()
	b.closed = true
	b.cancel()
	b.mu.Unlock()
	done := make(chan struct{})
	go func() {
		b.waitGroup.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *ProcessBackend) wait(a *processAttempt, processCtx context.Context, process ManagedProcess) {
	defer b.waitGroup.Done()
	exitCode, waitErr := process.Wait()
	completed := b.config.Now().UTC()
	state := workloadjournal.Succeeded
	errorCode := ""
	if errors.Is(processCtx.Err(), context.DeadlineExceeded) || errors.Is(waitErr, context.DeadlineExceeded) {
		state, exitCode, errorCode = workloadjournal.TimedOut, 124, "job_timeout"
	} else if errors.Is(processCtx.Err(), context.Canceled) {
		state, exitCode, errorCode = workloadjournal.Cancelled, 130, "cancelled"
	} else if waitErr != nil || exitCode != 0 {
		state, errorCode = workloadjournal.Failed, "test_failed"
		if exitCode == 0 {
			exitCode = 1
		}
	}
	logs, truncated := a.output.snapshot()
	if truncated {
		// Do not retain a prefix that could end in the middle of a secret.
		logs = []byte("[Hako Agent] output limit exceeded; logs withheld\n")
	}
	logs = fake.RedactJobLogs(logs, nil)
	if len(logs) > maxAgentLogBytes {
		logs = []byte("[Hako Agent] redacted logs exceeded the transfer limit; logs withheld\n")
	}
	result := workloadjobs.WorkloadResult{SchemaVersion: 1, TenantID: domain.TenantID(a.plan.Identity.TenantID),
		WorkloadRunID: domain.WorkloadRunID(a.plan.Identity.WorkloadRunID), AttemptID: a.plan.Identity.AttemptID,
		Generation: a.plan.Identity.Generation, ResourcePlaneID: domain.ResourcePlaneID(a.plan.Identity.ResourcePlaneID),
		CommitSHA: a.plan.CommitSHA, Status: domain.WorkloadRunState(state), ExitCode: exitCode, ErrorCode: errorCode, CompletedAt: completed}
	if workloadjobs.ValidateResult(result) != nil {
		result.Status, result.ExitCode, result.ErrorCode, result.LogArtifact = domain.WorkloadFailed, 1, "agent_result_invalid", nil
	}
	b.mu.Lock()
	a.state = workloadjournal.ExecutionState(result.Status)
	a.result = result
	a.logs = append([]byte(nil), logs...)
	b.mu.Unlock()
	a.cancel()
}

func matchesAttempt(attempt *processAttempt, claims TicketClaims) bool {
	return attempt != nil && attempt.claims.MicroVMID == claims.MicroVMID && attempt.claims.Identity.Command() == claims.Identity.Command()
}

type boundedOutput struct {
	mu        sync.Mutex
	limit     int
	data      []byte
	truncated bool
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	available := w.limit - len(w.data)
	if available > 0 {
		n := len(p)
		if n > available {
			n = available
		}
		w.data = append(w.data, p[:n]...)
	}
	if len(p) > available {
		w.truncated = true
	}
	return len(p), nil
}

func (w *boundedOutput) snapshot() ([]byte, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.data...), w.truncated
}

// GoTestProcessFactory deliberately runs one fixed command without a shell.
// Lambda MicroVM is the isolation boundary; this process receives a minimal
// environment and is killed as a process group on cancellation/deadline.
type GoTestProcessFactory struct {
	GoBinary    string
	GitBinary   string
	GoCache     string
	ModuleCache string
}

func (f GoTestProcessFactory) Start(ctx context.Context, plan ExecutionPlan, output io.Writer) (ManagedProcess, error) {
	repository, err := filepath.EvalSymlinks(plan.RepositoryPath)
	if err != nil {
		return nil, ErrExecutionPlan
	}
	repository, err = filepath.Abs(repository)
	if err != nil || repository == string(filepath.Separator) || strings.ContainsAny(repository, "\r\n,\\") {
		return nil, ErrExecutionPlan
	}
	info, err := os.Stat(repository)
	if err != nil || !info.IsDir() {
		return nil, ErrExecutionPlan
	}
	plan.RepositoryPath = repository
	if !validIdentity(plan.Identity) || !protocolDigest.MatchString(plan.SourceDigest) || !protocolCommit.MatchString(plan.CommitSHA) ||
		verifyInstalledExecutionSource(ctx, plan, workloadsource.DefaultMaxArchiveBytes) != nil {
		return nil, ErrExecutionPlan
	}
	if len(plan.FactoryPipelines) > 0 {
		return f.startFactoryPipelines(ctx, plan, output)
	}
	goBinary := f.GoBinary
	if goBinary == "" {
		goBinary = "/usr/local/go/bin/go"
	}
	if !filepath.IsAbs(goBinary) {
		return nil, ErrExecutionPlan
	}
	cache := f.GoCache
	if cache == "" {
		cache = "/tmp/hako-go-cache"
	}
	env := []string{"PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin", "HOME=/tmp", "TMPDIR=/tmp", "GOCACHE=" + cache,
		"GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOMAXPROCS=2"}
	if f.ModuleCache != "" {
		moduleCache, err := filepath.EvalSymlinks(f.ModuleCache)
		if err != nil {
			return nil, ErrExecutionPlan
		}
		moduleInfo, err := os.Stat(moduleCache)
		if err != nil || !moduleInfo.IsDir() {
			return nil, ErrExecutionPlan
		}
		env = append(env, "GOMODCACHE="+moduleCache)
	}
	cmd := exec.CommandContext(ctx, goBinary, "test", "./...")
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = repository, env, output, output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, ErrAgentBusy
	}
	return &execManagedProcess{cmd: cmd}, nil
}

func (f GoTestProcessFactory) startFactoryPipelines(ctx context.Context, plan ExecutionPlan, output io.Writer) (ManagedProcess, error) {
	if ctx == nil || output == nil || len(plan.FactoryPipelines) == 0 || len(plan.FactoryPipelines) > factoryconfig.MaxPipelines {
		return nil, ErrExecutionPlan
	}
	previousName := ""
	for _, pipeline := range plan.FactoryPipelines {
		if factoryconfig.ValidateExecutionPlan(pipeline) != nil || pipeline.RuntimeClass != "standard" ||
			pipeline.Network != "registry-only" || len(pipeline.Services) != 0 || (previousName != "" && pipeline.Pipeline <= previousName) {
			// Service containers and alternate runtime profiles must be provisioned
			// by their own trusted runtime adapters before this executor can accept them.
			return nil, ErrExecutionPlan
		}
		previousName = pipeline.Pipeline
	}
	process := &factoryPipelineProcess{done: make(chan processExit, 1)}
	go func() {
		var result processExit
		for _, pipeline := range plan.FactoryPipelines {
			pipelineCtx, cancel := context.WithTimeout(ctx, time.Duration(pipeline.TimeoutSecond)*time.Second)
			_, _ = fmt.Fprintf(output, "[Hako] pipeline %s\n", pipeline.Pipeline)
			for _, command := range pipeline.Commands {
				_, _ = fmt.Fprintf(output, "[Hako] step %s\n", command.Step)
				executable := command.Executable
				if command.Executable == "/usr/local/go/bin/go" && f.GoBinary != "" {
					if !filepath.IsAbs(f.GoBinary) {
						result = processExit{code: 1, err: ErrExecutionPlan}
						break
					}
					executable = f.GoBinary
				}
				env := []string{"PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin", "HOME=/tmp", "TMPDIR=/tmp", "GOMAXPROCS=2"}
				switch command.Step {
				case "go-test", "go-vet":
					cache := f.GoCache
					if cache == "" {
						cache = "/tmp/hako-go-cache"
					}
					env = append(env, "GOCACHE="+cache, "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "GOWORK=off")
					if f.ModuleCache != "" {
						moduleCache, err := filepath.EvalSymlinks(f.ModuleCache)
						if err != nil {
							result = processExit{code: 1, err: ErrExecutionPlan}
							break
						}
						info, err := os.Stat(moduleCache)
						if err != nil || !info.IsDir() {
							result = processExit{code: 1, err: ErrExecutionPlan}
							break
						}
						env = append(env, "GOMODCACHE="+moduleCache)
					}
				case "npm-test", "npm-build":
					env = append(env, "NPM_CONFIG_UPDATE_NOTIFIER=false", "NPM_CONFIG_FUND=false")
				case "python-test":
					env = append(env, "PYTHONNOUSERSITE=1", "PYTHONDONTWRITEBYTECODE=1")
				}
				if result.err != nil {
					break
				}
				if err := runCatalogCommand(pipelineCtx, plan.RepositoryPath, executable, command.Args, env, output); err != nil {
					if errors.Is(pipelineCtx.Err(), context.DeadlineExceeded) {
						result = processExit{code: 124, err: context.DeadlineExceeded}
					} else {
						result = processExit{code: catalogExitCode(err), err: err}
					}
					break
				}
			}
			cancel()
			if result.err != nil || ctx.Err() != nil {
				if ctx.Err() != nil {
					result = processExit{code: 130, err: ctx.Err()}
				}
				break
			}
		}
		process.done <- result
	}()
	return process, nil
}

type processExit struct {
	code int
	err  error
}

type factoryPipelineProcess struct{ done chan processExit }

func (p *factoryPipelineProcess) Wait() (int, error) {
	result := <-p.done
	return result.code, result.err
}

func runCatalogCommand(ctx context.Context, directory, executable string, args, env []string, output io.Writer) error {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = directory, env, output, output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return ErrAgentBusy
	}
	return cmd.Wait()
}

func catalogExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}

type execManagedProcess struct{ cmd *exec.Cmd }

func (p *execManagedProcess) Wait() (int, error) {
	err := p.cmd.Wait()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), ErrProcessExit
	}
	return 1, fmt.Errorf("wait for Workload process: %w", err)
}

var _ Backend = (*ProcessBackend)(nil)
