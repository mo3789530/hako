package workloadcontroller

import (
	"context"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

// AgentTaskProvider resolves the untrusted, bounded Issue context for one
// exact current attempt. Implementations must use the Resource Plane mTLS
// broker; task text must never be logged or persisted in the execution journal.
type AgentTaskProvider interface {
	ResolveAgentTask(context.Context, workloadjournal.Command) (workloadagent.AgentTask, error)
}

// AgentModelCredentialProvider resolves one current attempt's Tenant-owned
// model key through the Resource Plane mTLS Broker. Callers must return an
// owned buffer that the Workload runner can clear after delivery.
type AgentModelCredentialProvider interface {
	ResolveAgentModelCredential(context.Context, workloadjournal.Command) ([]byte, error)
}

// HTTPAgentRunner implements the controller's AgentRunner contract using the
// private, versioned protocol. It deliberately has no logging hook for the
// ephemeral AWS proxy token.
type HTTPAgentRunner struct {
	client           *workloadagent.Client
	sources          workloadsource.SourceArchiveReader
	logs             workloadagent.AttemptLogWriter
	changes          workloadagent.AttemptChangeBundleWriter
	tasks            AgentTaskProvider
	modelCredentials AgentModelCredentialProvider
}

func NewHTTPAgentRunner(client *workloadagent.Client, sources workloadsource.SourceArchiveReader, logs workloadagent.AttemptLogWriter, tasks ...AgentTaskProvider) *HTTPAgentRunner {
	if client == nil {
		client = workloadagent.NewClient(nil)
	}
	var taskProvider AgentTaskProvider
	if len(tasks) == 1 {
		taskProvider = tasks[0]
	}
	return &HTTPAgentRunner{client: client, sources: sources, logs: logs, tasks: taskProvider}
}

func NewHTTPAgentRunnerWithModelCredentials(client *workloadagent.Client, sources workloadsource.SourceArchiveReader,
	logs workloadagent.AttemptLogWriter, tasks AgentTaskProvider, credentials AgentModelCredentialProvider) *HTTPAgentRunner {
	runner := NewHTTPAgentRunner(client, sources, logs, tasks)
	runner.modelCredentials = credentials
	return runner
}

func NewHTTPAgentRunnerWithArtifacts(client *workloadagent.Client, sources workloadsource.SourceArchiveReader,
	logs workloadagent.AttemptLogWriter, changes workloadagent.AttemptChangeBundleWriter, tasks AgentTaskProvider,
	credentials AgentModelCredentialProvider) *HTTPAgentRunner {
	runner := NewHTTPAgentRunnerWithModelCredentials(client, sources, logs, tasks, credentials)
	runner.changes = changes
	return runner
}

func (r *HTTPAgentRunner) Start(ctx context.Context, conn AgentConnection, spec Specification) error {
	if r == nil || r.client == nil || r.sources == nil || conn.MicroVMID == "" {
		return ErrMicroVMNotReady
	}
	if conn.SourceTicket == "" || spec.ArtifactPrefix == "" {
		return ErrMicroVMNotReady
	}
	start := workloadagent.StartRequest{
		Identity: workloadagent.IdentityFromCommand(spec.Identity), Kind: spec.Kind, SourceDigest: spec.SourceDigest, CommitSHA: spec.CommitSHA, Deadline: spec.Deadline.UTC(),
	}
	if spec.Kind == domain.WorkloadAgent {
		if r.tasks == nil || r.modelCredentials == nil {
			return ErrExecutionMaterialization
		}
		task, err := r.tasks.ResolveAgentTask(ctx, spec.Identity)
		if err != nil || !workloadagent.ValidateAgentTask(task, start.Identity, spec.CommitSHA) ||
			task.DefaultBranch != spec.TriggerBranch || task.IntegrationID != spec.GitHubIntegrationID ||
			task.InstallationID != spec.GitHubInstallationID || task.RepositoryID != spec.GitHubRepositoryID ||
			task.ContextSHA256 != spec.AgentTaskContextSHA256 {
			return ErrExecutionMaterialization
		}
		start.AgentTask = &task
	} else if spec.Kind != "" && spec.Kind != domain.WorkloadJob {
		return ErrExecutionMaterialization
	}
	archive, err := r.sources.OpenSourceArchive(ctx, spec.Identity, spec.ArtifactPrefix, spec.SourceDigest)
	if err != nil || archive.Body == nil {
		return ErrExecutionMaterialization
	}
	defer archive.Body.Close()
	if err := r.client.UploadSource(ctx, conn.Endpoint, conn.AuthToken, conn.SourceTicket, spec.Identity, spec.SourceDigest, spec.CommitSHA, archive.Size, archive.Body); err != nil {
		return err
	}
	if spec.Kind == domain.WorkloadAgent {
		credential, err := r.modelCredentials.ResolveAgentModelCredential(ctx, spec.Identity)
		if err != nil || !validAgentModelCredential(credential) {
			clear(credential)
			return ErrExecutionMaterialization
		}
		defer clear(credential)
		start.AgentModelCredential = credential
	}
	return r.client.Start(ctx, conn.Endpoint, conn.AuthToken, conn.AgentTicket, start)
}

func validAgentModelCredential(credential []byte) bool {
	if len(credential) == 0 || len(credential) > 8<<10 {
		return false
	}
	for _, b := range credential {
		if b == 0 || b == '\r' || b == '\n' {
			return false
		}
	}
	return true
}

func (r *HTTPAgentRunner) GetStatus(ctx context.Context, conn AgentConnection, id workloadjournal.Command) (workloadjournal.ExecutionState, error) {
	if r == nil || r.client == nil || conn.MicroVMID == "" {
		return "", ErrMicroVMNotReady
	}
	return r.client.Status(ctx, conn.Endpoint, conn.AuthToken, conn.AgentTicket, id)
}

func (r *HTTPAgentRunner) CollectResult(ctx context.Context, conn AgentConnection, id workloadjournal.Command) (workloadjobs.WorkloadResult, error) {
	if r == nil || r.client == nil || conn.MicroVMID == "" {
		return workloadjobs.WorkloadResult{}, ErrMicroVMNotReady
	}
	if r.logs == nil || conn.LogTicket == "" {
		return workloadjobs.WorkloadResult{}, ErrMicroVMNotReady
	}
	result, err := r.client.Result(ctx, conn.Endpoint, conn.AuthToken, conn.AgentTicket, id)
	if err != nil {
		return workloadjobs.WorkloadResult{}, err
	}
	logs, digest, err := r.client.Logs(ctx, conn.Endpoint, conn.AuthToken, conn.LogTicket, id)
	if err != nil {
		return workloadjobs.WorkloadResult{}, err
	}
	artifact, err := r.logs.PutAttemptLogs(ctx, id, logs, digest)
	if err != nil || !artifact.Redacted || artifact.SHA256 != digest || artifact.SizeBytes != int64(len(logs)) {
		return workloadjobs.WorkloadResult{}, ErrExecutionMaterialization
	}
	result.LogArtifact = &artifact
	if conn.ChangesTicket != "" && result.Status == domain.WorkloadSucceeded {
		bundle, bundleDigest, err := r.client.Changes(ctx, conn.Endpoint, conn.AuthToken, conn.ChangesTicket, id)
		if err != nil {
			return workloadjobs.WorkloadResult{}, err
		}
		if len(bundle) > 0 {
			if r.changes == nil {
				clear(bundle)
				return workloadjobs.WorkloadResult{}, ErrExecutionMaterialization
			}
			changeArtifact, err := r.changes.PutAgentChangeBundle(ctx, id, bundle, bundleDigest)
			clear(bundle)
			if err != nil || changeArtifact.SizeBytes < 1 || changeArtifact.SizeBytes > 8<<20 || changeArtifact.SHA256 != bundleDigest {
				return workloadjobs.WorkloadResult{}, ErrExecutionMaterialization
			}
			result.ChangeBundle = &changeArtifact
		}
	}
	if workloadjobs.ValidateResult(result) != nil {
		return workloadjobs.WorkloadResult{}, ErrExecutionMaterialization
	}
	return result, nil
}
