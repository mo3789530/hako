package fake

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

const (
	defaultJobLogLimit = 2 << 20
	maxJobLogLimit     = 16 << 20
)

var (
	ErrJobRunConflict = errors.New("Workload Run ID was reused for a different repository commit")
	ErrCommitMismatch = errors.New("checked-out Repository commit does not match Workload Run commit")
	jobImagePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,255}$`)
	jobCommitPattern  = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	secretPatterns    = []*regexp.Regexp{
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]{20,}\b`),
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]+=*`),
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
	}
)

type JobSpec struct {
	TenantID       domain.TenantID
	RunID          domain.WorkloadRunID
	RepositoryPath string
	CommitSHA      string
	Timeout        time.Duration
	SecretValues   []string
}

type JobResult struct {
	TenantID     domain.TenantID
	RunID        domain.WorkloadRunID
	CommitSHA    string
	ExitCode     int
	TimedOut     bool
	LogTruncated bool
	RedactedLogs []byte
	LogsSHA256   string
	StartedAt    time.Time
	CompletedAt  time.Time
}

type JobRuntimeConfig struct {
	PodmanPath    string
	GoImage       string
	ModuleCache   string
	MaxLogBytes   int
	CommandRunner CommandRunner
}

// CommandRunner is injectable so tests can inspect the exact command without
// executing repository code on the host.
type CommandRunner interface {
	Run(context.Context, string, []string, io.Writer) error
}

type CommandExitError struct {
	Code int
	Err  error
}

func (e *CommandExitError) Error() string {
	return fmt.Sprintf("command exited with status %d", e.Code)
}
func (e *CommandExitError) Unwrap() error { return e.Err }

type storedJobResult struct {
	tenantID  domain.TenantID
	commitSHA string
	result    JobResult
}

type JobRuntime struct {
	mu      sync.Mutex
	config  JobRuntimeConfig
	results map[domain.WorkloadRunID]storedJobResult
}

func NewJobRuntime(config JobRuntimeConfig) (*JobRuntime, error) {
	if !jobImagePattern.MatchString(config.GoImage) {
		return nil, errors.New("a safe Go job image reference is required")
	}
	if config.PodmanPath == "" {
		config.PodmanPath = "podman"
	}
	if config.MaxLogBytes == 0 {
		config.MaxLogBytes = defaultJobLogLimit
	}
	if config.MaxLogBytes < 1 || config.MaxLogBytes > maxJobLogLimit {
		return nil, fmt.Errorf("job log limit must be between 1 and %d bytes", maxJobLogLimit)
	}
	if config.ModuleCache != "" {
		resolved, err := filepath.EvalSymlinks(config.ModuleCache)
		if err != nil {
			return nil, fmt.Errorf("resolve read-only Go module cache: %w", err)
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			return nil, errors.New("Go module cache must be an existing directory")
		}
		config.ModuleCache = resolved
	}
	if config.CommandRunner == nil {
		config.CommandRunner = osCommandRunner{}
	}
	return &JobRuntime{config: config, results: make(map[domain.WorkloadRunID]storedJobResult)}, nil
}

// RunGoTest executes the immutable checked-out commit in an ephemeral,
// network-disabled Podman container. The repository mount is read-only and no
// host credentials or environment variables are forwarded. Results and
// redacted logs are cached by this local fake runtime for inspection.
func (r *JobRuntime) RunGoTest(ctx context.Context, spec JobSpec) (JobResult, error) {
	if r == nil || ctx == nil || spec.TenantID == "" || spec.RunID == "" || !jobCommitPattern.MatchString(spec.CommitSHA) {
		return JobResult{}, errors.New("Tenant, Workload Run, valid commit SHA, and runtime are required")
	}
	if spec.Timeout <= 0 || spec.Timeout > 24*time.Hour {
		return JobResult{}, errors.New("Job timeout must be between one nanosecond and 24 hours")
	}
	repositoryPath, err := filepath.EvalSymlinks(spec.RepositoryPath)
	if err != nil {
		return JobResult{}, fmt.Errorf("resolve checked-out Repository: %w", err)
	}
	repositoryPath, err = filepath.Abs(repositoryPath)
	if err != nil {
		return JobResult{}, fmt.Errorf("make checked-out Repository path absolute: %w", err)
	}
	if repositoryPath == string(filepath.Separator) || strings.ContainsAny(repositoryPath, ",\n\r") {
		return JobResult{}, errors.New("checked-out Repository path is not safe to mount")
	}
	info, err := os.Stat(repositoryPath)
	if err != nil || !info.IsDir() {
		return JobResult{}, errors.New("checked-out Repository must be a directory")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if previous, exists := r.results[spec.RunID]; exists {
		if previous.tenantID != spec.TenantID || previous.commitSHA != spec.CommitSHA {
			return JobResult{}, ErrJobRunConflict
		}
		return cloneJobResult(previous.result), nil
	}
	startedAt := time.Now().UTC()
	var revision bytes.Buffer
	if err := r.config.CommandRunner.Run(ctx, "git", []string{"-C", repositoryPath, "rev-parse", "--verify", "HEAD"}, &revision); err != nil {
		return JobResult{}, errors.New("read checked-out Repository commit")
	}
	if strings.TrimSpace(revision.String()) != spec.CommitSHA {
		return JobResult{}, ErrCommitMismatch
	}
	jobCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	logs := &boundedJobLog{limit: r.config.MaxLogBytes}
	args := r.podmanArgs(repositoryPath)
	args = append(args, r.config.GoImage, "go", "test", "./...")
	jobErr := r.config.CommandRunner.Run(jobCtx, r.config.PodmanPath, args, logs)
	completedAt := time.Now().UTC()
	result := JobResult{TenantID: spec.TenantID, RunID: spec.RunID, CommitSHA: spec.CommitSHA,
		ExitCode: 0, StartedAt: startedAt, CompletedAt: completedAt}
	if jobErr != nil {
		if errors.Is(jobCtx.Err(), context.DeadlineExceeded) {
			result.TimedOut = true
			result.ExitCode = 124
		} else {
			var exitErr *CommandExitError
			if !errors.As(jobErr, &exitErr) {
				return JobResult{}, fmt.Errorf("execute isolated Go test Job: %w", jobErr)
			}
			result.ExitCode = exitErr.Code
		}
	}
	result.LogTruncated = logs.truncated
	result.RedactedLogs = RedactJobLogs(logs.data, spec.SecretValues)
	if len(result.RedactedLogs) > r.config.MaxLogBytes {
		result.RedactedLogs = result.RedactedLogs[:r.config.MaxLogBytes]
		result.LogTruncated = true
	}
	result.LogsSHA256 = sha256Hex(result.RedactedLogs)
	r.results[spec.RunID] = storedJobResult{tenantID: spec.TenantID, commitSHA: spec.CommitSHA, result: cloneJobResult(result)}
	return result, nil
}

func (r *JobRuntime) podmanArgs(repositoryPath string) []string {
	args := []string{"run", "--rm", "--pull=never", "--network=none", "--read-only", "--cap-drop=ALL",
		"--security-opt=no-new-privileges", "--pids-limit=256", "--memory=2g", "--cpus=2",
		"--user=65532:65532", "--stop-timeout=5", "--tmpfs", "/tmp:rw,nosuid,nodev,size=1g,mode=1777",
		"--mount", "type=bind,src=" + repositoryPath + ",dst=/workspace,ro=true",
		"--workdir=/workspace", "--env=HOME=/tmp", "--env=GOCACHE=/tmp/go-cache", "--env=GOPROXY=off", "--env=GOSUMDB=off"}
	if r.config.ModuleCache != "" {
		args = append(args, "--mount", "type=bind,src="+r.config.ModuleCache+",dst=/go/pkg/mod,ro=true", "--env=GOMODCACHE=/go/pkg/mod")
	} else {
		args = append(args, "--env=GOMODCACHE=/tmp/go-mod-cache")
	}
	return args
}

func (r *JobRuntime) Result(tenantID domain.TenantID, runID domain.WorkloadRunID) (JobResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result, exists := r.results[runID]
	if !exists || result.tenantID != tenantID {
		return JobResult{}, false
	}
	return cloneJobResult(result.result), true
}

func RedactJobLogs(logs []byte, secretValues []string) []byte {
	redacted := append([]byte(nil), logs...)
	for _, secret := range secretValues {
		if secret != "" {
			redacted = bytes.ReplaceAll(redacted, []byte(secret), []byte("[REDACTED]"))
		}
	}
	for _, pattern := range secretPatterns {
		redacted = pattern.ReplaceAll(redacted, []byte("[REDACTED]"))
	}
	return redacted
}

func cloneJobResult(result JobResult) JobResult {
	result.RedactedLogs = append([]byte(nil), result.RedactedLogs...)
	return result
}

type boundedJobLog struct {
	data      []byte
	limit     int
	truncated bool
	mu        sync.Mutex
}

func (w *boundedJobLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	remaining := w.limit - len(w.data)
	if remaining > 0 {
		if remaining >= len(p) {
			w.data = append(w.data, p...)
		} else {
			w.data = append(w.data, p[:remaining]...)
			w.truncated = true
		}
	} else if len(p) > 0 {
		w.truncated = true
	}
	return len(p), nil
}

type osCommandRunner struct{}

func (osCommandRunner) Run(ctx context.Context, name string, args []string, output io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &CommandExitError{Code: exitErr.ExitCode(), Err: err}
		}
		return err
	}
	return nil
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
