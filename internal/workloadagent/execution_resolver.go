package workloadagent

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/factoryconfig"
	webhookevent "github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/workloadsource"
)

// FilesystemExecutionResolver builds an execution plan only from a verified,
// execution-bound Agent ticket and one image-configured source directory. No
// request, queue, repository, or ticket field can select a filesystem path.
type FilesystemExecutionResolver struct {
	repositoryPath   string
	allowJobServices bool
	now              func() time.Time
}

func NewFilesystemExecutionResolver(repositoryPath string, now func() time.Time) (*FilesystemExecutionResolver, error) {
	return NewFilesystemExecutionResolverWithJobServices(repositoryPath, now, false)
}

// NewFilesystemExecutionResolverWithJobServices admits catalog Service
// declarations only for an explicitly composed Job image. The default
// constructor keeps them rejected.
func NewFilesystemExecutionResolverWithJobServices(repositoryPath string, now func() time.Time, allowJobServices bool) (*FilesystemExecutionResolver, error) {
	repositoryPath = strings.TrimSpace(repositoryPath)
	if repositoryPath == "" || !filepath.IsAbs(repositoryPath) || strings.ContainsAny(repositoryPath, "\r\n,\\") {
		return nil, ErrExecutionPlan
	}
	absolute, err := filepath.Abs(repositoryPath)
	if err != nil || filepath.Clean(absolute) != absolute || absolute == string(filepath.Separator) {
		return nil, ErrExecutionPlan
	}
	if now == nil {
		now = time.Now
	}
	return &FilesystemExecutionResolver{repositoryPath: absolute, allowJobServices: allowJobServices, now: now}, nil
}

func (r *FilesystemExecutionResolver) ResolveExecution(ctx context.Context, claims TicketClaims) (ExecutionPlan, error) {
	if r == nil || ctx == nil || ctx.Err() != nil || claims.Operation != OperationStart || claims.Execution == nil ||
		!validIdentity(claims.Identity.Command()) || !validExecutionBinding(*claims.Execution) {
		return ExecutionPlan{}, ErrExecutionPlan
	}
	now := r.now().UTC()
	deadline := claims.Execution.Deadline.UTC()
	if !deadline.After(now) || deadline.Sub(now) > 8*time.Hour {
		return ExecutionPlan{}, ErrExecutionPlan
	}
	plan := ExecutionPlan{Identity: claims.Identity.Command(), RepositoryPath: r.repositoryPath,
		Kind:               claims.Execution.Kind,
		AgentIntegrationID: claims.Execution.AgentIntegrationID, AgentInstallationID: claims.Execution.AgentInstallationID,
		AgentRepositoryID: claims.Execution.AgentRepositoryID, AgentTaskContextSHA256: claims.Execution.AgentTaskContextSHA256,
		SourceDigest: claims.Execution.SourceDigest, CommitSHA: claims.Execution.CommitSHA,
		TriggerEvent: claims.Execution.TriggerEvent, TriggerBranch: claims.Execution.TriggerBranch,
		CorrelationID: claims.Execution.CorrelationID, JobMemoryLimitMB: claims.Execution.JobMemoryLimitMB,
		JobCPULimitMillis: claims.Execution.JobCPULimitMillis, Deadline: deadline}
	if plan.TriggerEvent != "" {
		if verifyInstalledExecutionSource(ctx, plan, workloadsource.DefaultMaxArchiveBytes) != nil {
			return ExecutionPlan{}, ErrExecutionPlan
		}
		if plan.Kind != domain.WorkloadAgent {
			pipelines, err := resolveFactoryPipelines(ctx, plan, r.allowJobServices)
			if err != nil {
				return ExecutionPlan{}, err
			}
			plan.FactoryPipelines = pipelines
		}
	}
	return plan, nil
}

func resolveFactoryPipelines(ctx context.Context, execution ExecutionPlan, allowJobServices bool) ([]factoryconfig.ExecutionPlan, error) {
	if ctx == nil || ctx.Err() != nil || execution.TriggerEvent == "" || execution.TriggerBranch == "" {
		return nil, ErrExecutionPlan
	}
	manifest, found, err := readFactoryManifest(execution.RepositoryPath)
	if err != nil {
		return nil, ErrFactoryManifest
	}
	if !found {
		return nil, nil // legacy repository: retain the fixed Go-test executor.
	}
	config, err := factoryconfig.Parse(manifest)
	if err != nil {
		return nil, ErrFactoryManifest
	}
	names := factoryconfig.Select(config, webhookEventForTrigger(execution.TriggerEvent, execution.TriggerBranch))
	if len(names) == 0 {
		return nil, ErrFactoryManifest
	}
	plans := make([]factoryconfig.ExecutionPlan, 0, len(names))
	for _, name := range names {
		plan, err := factoryconfig.Plan(config, name)
		if err != nil || factoryconfig.ValidateExecutionPlan(plan) != nil || plan.RuntimeClass != "standard" ||
			plan.MemoryMB > execution.JobMemoryLimitMB || plan.CPUMillis > execution.JobCPULimitMillis ||
			plan.Network != "registry-only" || len(plan.Services) != 0 && !allowJobServices {
			return nil, ErrFactoryManifest
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func webhookEventForTrigger(kind, branch string) webhookevent.RepositoryEvent {
	event := webhookevent.RepositoryEvent{SchemaVersion: webhookevent.RepositoryEventSchemaVersion}
	switch kind {
	case "push":
		event.GitHubEvent, event.Type, event.Ref = "push", "repository.push", "refs/heads/"+branch
	case "pull_request":
		event.GitHubEvent, event.Action, event.Type = "pull_request", "opened", "repository.pull_request.opened"
		event.PullRequest = &webhookevent.PullRequestRef{State: "open", BaseRef: branch}
	}
	return event
}

var _ ExecutionResolver = (*FilesystemExecutionResolver)(nil)
