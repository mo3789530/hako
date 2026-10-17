package actionsrunner

import (
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrRunnerProcess = errors.New("ephemeral GitHub Actions runner process failed")

type RunnerProcessResult struct {
	IntegrationID string
	WorkflowJobID int64
	WorkflowRunID int64
	RunAttempt    int64
	ExitCode      int
	FailurePhase  string
	Cancelled     bool
	TimedOut      bool
	CleanupFailed bool
}

type RunnerProcessConfig struct {
	RootDirectory string
	SetupTimeout  time.Duration
	MaximumRun    time.Duration
	OnExit        func(RunnerProcessResult)
}

// RunnerProcessConsumer configures the official runner as ephemeral and starts
// it for exactly one bootstrap. It passes the model key only in the runner
// process environment, never to config.sh or process arguments.
type RunnerProcessConsumer struct {
	configPath string
	runPath    string
	root       string
	work       string
	config     RunnerProcessConfig

	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc
}

func NewRunnerProcessConsumer(config RunnerProcessConfig) (*RunnerProcessConsumer, error) {
	if config.SetupTimeout == 0 {
		config.SetupTimeout = 10 * time.Second
	}
	if config.MaximumRun == 0 {
		config.MaximumRun = 8 * time.Hour
	}
	if config.SetupTimeout < time.Second || config.SetupTimeout > time.Minute || config.MaximumRun < time.Minute || config.MaximumRun > 8*time.Hour ||
		strings.TrimSpace(config.RootDirectory) != config.RootDirectory || config.RootDirectory == "" {
		return nil, ErrRunnerProcess
	}
	root, err := filepath.Abs(config.RootDirectory)
	if err != nil {
		return nil, ErrRunnerProcess
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root || root == string(filepath.Separator) {
		return nil, ErrRunnerProcess
	}
	configPath, err := runnerExecutable(root, "config.sh")
	if err != nil {
		return nil, ErrRunnerProcess
	}
	runPath, err := runnerExecutable(root, "run.sh")
	if err != nil {
		return nil, ErrRunnerProcess
	}
	work := filepath.Join(root, "_work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, ErrRunnerProcess
	}
	if err := os.MkdirAll(filepath.Join(root, "_temp"), 0o700); err != nil {
		return nil, ErrRunnerProcess
	}
	return &RunnerProcessConsumer{configPath: configPath, runPath: runPath, root: root, work: work, config: config}, nil
}

func runnerExecutable(root, name string) (string, error) {
	path := filepath.Join(root, name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "", ErrRunnerProcess
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return "", ErrRunnerProcess
	}
	return path, nil
}

func (c *RunnerProcessConsumer) Consume(ctx context.Context, claims RunnerBootstrapClaims) error {
	if c == nil || ctx == nil || !validBootstrapModelCredential(claims.ModelCredential) || !validRunnerBootstrapClaims(claims) {
		return ErrRunnerProcess
	}
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return ErrRunnerProcess
	}
	c.started = true
	c.mu.Unlock()

	setupCtx, setupCancel := context.WithTimeout(ctx, c.config.SetupTimeout)
	defer setupCancel()
	configure := exec.CommandContext(setupCtx, c.configPath, runnerConfigureArgs(claims)...)
	configure.Dir = c.root
	configure.Env = runnerBaseEnvironment(c.root)
	configure.Stdout, configure.Stderr = io.Discard, io.Discard
	setRunnerProcessGroup(configure)
	if err := configure.Run(); err != nil {
		result := runnerProcessFailureResult(claims, "configure", err)
		result.CleanupFailed = c.cleanupLocalRunnerState() != nil
		c.reportExit(result)
		return ErrRunnerProcess
	}

	runCtx, cancel := context.WithTimeout(context.Background(), c.config.MaximumRun)
	command := exec.CommandContext(runCtx, c.runPath)
	command.Dir = c.root
	command.Env = runnerExecutionEnvironment(c.root, c.work, claims.ModelCredential)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	command.WaitDelay = 3 * time.Second
	setRunnerProcessGroup(command)
	if err := command.Start(); err != nil {
		cancel()
		result := runnerProcessFailureResult(claims, "start", err)
		result.CleanupFailed = c.cleanupLocalRunnerState() != nil
		c.reportExit(result)
		return ErrRunnerProcess
	}
	command.Env = nil
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()
	result := RunnerProcessResult{IntegrationID: claims.IntegrationID, WorkflowJobID: claims.WorkflowJobID,
		WorkflowRunID: claims.WorkflowRunID, RunAttempt: claims.RunAttempt}
	go func() {
		err := command.Wait()
		if err != nil {
			result.FailurePhase = "run"
		}
		result.Cancelled = errors.Is(runCtx.Err(), context.Canceled)
		result.TimedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
		if err != nil {
			var exitError *exec.ExitError
			if errors.As(err, &exitError) {
				result.ExitCode = exitError.ExitCode()
			} else {
				result.ExitCode = -1
			}
		}
		cancel()
		result.CleanupFailed = c.cleanupLocalRunnerState() != nil
		c.reportExit(result)
		c.mu.Lock()
		c.cancel = nil
		c.mu.Unlock()
	}()
	return nil
}

func runnerProcessFailureResult(claims RunnerBootstrapClaims, phase string, err error) RunnerProcessResult {
	result := RunnerProcessResult{IntegrationID: claims.IntegrationID, WorkflowJobID: claims.WorkflowJobID,
		WorkflowRunID: claims.WorkflowRunID, RunAttempt: claims.RunAttempt, ExitCode: -1, FailurePhase: phase}
	result.TimedOut = errors.Is(err, context.DeadlineExceeded)
	result.Cancelled = errors.Is(err, context.Canceled)
	return result
}

func (c *RunnerProcessConsumer) reportExit(result RunnerProcessResult) {
	if c != nil && c.config.OnExit != nil {
		c.config.OnExit(result)
	}
}

func (c *RunnerProcessConsumer) cleanupLocalRunnerState() error {
	if c == nil {
		return ErrRunnerProcess
	}
	var cleanupErr error
	for _, name := range []string{"_work", "_temp", "_diag", ".credentials", ".credentials_rsaparams", ".runner", ".service", ".path"} {
		if err := os.RemoveAll(filepath.Join(c.root, name)); err != nil {
			cleanupErr = ErrRunnerProcess
		}
	}
	return cleanupErr
}

func (c *RunnerProcessConsumer) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func runnerConfigureArgs(claims RunnerBootstrapClaims) []string {
	return []string{"--unattended", "--disableupdate", "--ephemeral", "--url", claims.RepositoryURL,
		"--token", claims.Token, "--name", claims.RunnerName, "--labels", strings.Join(claims.Labels, ","), "--work", "_work"}
}

func runnerBaseEnvironment(root string) []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = root
	}
	temp := filepath.Join(root, "_temp")
	return []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + home,
		"RUNNER_ALLOW_RUNASROOT=0", "RUNNER_TEMP=" + temp, "TMPDIR=" + temp}
}

func runnerExecutionEnvironment(root, work string, modelCredential []byte) []string {
	env := runnerBaseEnvironment(root)
	env = append(env, "RUNNER_WORKDIR="+work, "CI=true")
	if len(modelCredential) > 0 {
		env = append(env, "ANTHROPIC_API_KEY="+string(modelCredential))
	}
	return env
}

func validRunnerBootstrapClaims(claims RunnerBootstrapClaims) bool {
	now := time.Now().UTC().Unix()
	if claims.SchemaVersion != 1 || !runnerMicroVMIdentityPattern.MatchString(claims.MicroVMID) || claims.IntegrationID == "" ||
		claims.WorkflowJobID <= 0 || claims.WorkflowRunID <= 0 || claims.RunAttempt <= 0 || claims.Token == "" || len(claims.Token) > 4096 ||
		strings.ContainsAny(claims.Token, "\x00\r\n ") || claims.ExpiresAt <= now || claims.ExpiresAt > now+int64((5*time.Minute)/time.Second) ||
		claims.RunnerName != "hako-"+strconv.FormatInt(claims.WorkflowJobID, 10)+"-"+strconv.FormatInt(claims.RunAttempt, 10) ||
		!validRunnerRepositoryURL(claims.RepositoryURL) || len(claims.Labels) == 0 || len(claims.Labels) > 100 {
		return false
	}
	seen := make(map[string]struct{}, len(claims.Labels))
	for _, label := range claims.Labels {
		if label == "" || len(label) > 128 || strings.TrimSpace(label) != label || strings.ContainsAny(label, "\x00\r\n,") {
			return false
		}
		if _, duplicate := seen[label]; duplicate {
			return false
		}
		seen[label] = struct{}{}
	}
	return true
}

func validRunnerRepositoryURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || parsed.Opaque != "" || parsed.RawPath != "" || parsed.Path != "/"+strings.TrimPrefix(parsed.EscapedPath(), "/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	return len(parts) == 2 && validRepositoryPart(parts[0]) && validRepositoryPart(parts[1])
}
