package workloadcontroller

import (
	"context"

	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

// HTTPAgentRunner implements the controller's AgentRunner contract using the
// private, versioned protocol. It deliberately has no logging hook for the
// ephemeral AWS proxy token.
type HTTPAgentRunner struct {
	client  *workloadagent.Client
	sources workloadsource.SourceArchiveReader
	logs    workloadagent.AttemptLogWriter
}

func NewHTTPAgentRunner(client *workloadagent.Client, sources workloadsource.SourceArchiveReader, logs workloadagent.AttemptLogWriter) *HTTPAgentRunner {
	if client == nil {
		client = workloadagent.NewClient(nil)
	}
	return &HTTPAgentRunner{client: client, sources: sources, logs: logs}
}

func (r *HTTPAgentRunner) Start(ctx context.Context, conn AgentConnection, spec Specification) error {
	if r == nil || r.client == nil || r.sources == nil || conn.MicroVMID == "" {
		return ErrMicroVMNotReady
	}
	if conn.SourceTicket == "" || spec.ArtifactPrefix == "" {
		return ErrMicroVMNotReady
	}
	archive, err := r.sources.OpenSourceArchive(ctx, spec.Identity, spec.ArtifactPrefix, spec.SourceDigest)
	if err != nil || archive.Body == nil {
		return ErrExecutionMaterialization
	}
	defer archive.Body.Close()
	if err := r.client.UploadSource(ctx, conn.Endpoint, conn.AuthToken, conn.SourceTicket, spec.Identity, spec.SourceDigest, spec.CommitSHA, archive.Size, archive.Body); err != nil {
		return err
	}
	return r.client.Start(ctx, conn.Endpoint, conn.AuthToken, conn.AgentTicket, workloadagent.StartRequest{
		Identity: workloadagent.IdentityFromCommand(spec.Identity), SourceDigest: spec.SourceDigest, CommitSHA: spec.CommitSHA, Deadline: spec.Deadline.UTC(),
	})
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
	if workloadjobs.ValidateResult(result) != nil {
		return workloadjobs.WorkloadResult{}, ErrExecutionMaterialization
	}
	return result, nil
}
