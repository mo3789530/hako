package workloadcontroller

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"unicode/utf8"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workloadagentcore"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

const agentCoreSourceArchiveLimit = 40 << 20

// BrokeredAgentCoreInvocationResolver rebuilds invocation input from the
// current durable attempt, the trusted Workload specification, the mTLS task
// broker, and the attempt-scoped source archive. It never accepts task/source
// bytes from a queue envelope.
type BrokeredAgentCoreInvocationResolver struct {
	journal workloadjournal.Store
	tasks   AgentTaskProvider
	sources workloadsource.SourceArchiveReader
}

func NewBrokeredAgentCoreInvocationResolver(journal workloadjournal.Store, tasks AgentTaskProvider,
	sources workloadsource.SourceArchiveReader) (*BrokeredAgentCoreInvocationResolver, error) {
	if journal == nil || tasks == nil || sources == nil {
		return nil, ErrAgentCoreWorker
	}
	return &BrokeredAgentCoreInvocationResolver{journal: journal, tasks: tasks, sources: sources}, nil
}

func (r *BrokeredAgentCoreInvocationResolver) ResolveAgentCoreInvocation(ctx context.Context,
	command workloadjournal.Command) (AgentCoreInvocationInput, error) {
	if r == nil || ctx == nil || ctx.Err() != nil || !validAgentCoreCommand(command) {
		return AgentCoreInvocationInput{}, ErrAgentCoreWorker
	}
	snapshot, err := r.journal.Get(ctx, command.TenantID, command.WorkloadRunID, command.AttemptID)
	if err != nil || snapshot.ETag == "" || snapshot.Journal.Validate() != nil || !matchesAgentCoreCommand(snapshot.Journal, command) {
		return AgentCoreInvocationInput{}, ErrAgentCoreWorker
	}
	spec, err := decodeAgentCoreWorkloadSpecification(snapshot.Journal)
	if err != nil || spec.Kind != domain.WorkloadAgent || spec.GitHubRepositoryID < 1 ||
		!digestPattern.MatchString(spec.AgentTaskContextSHA256) || spec.ArtifactPrefix == "" ||
		!commitPattern.MatchString(spec.CommitSHA) || !digestPattern.MatchString(spec.SourceDigest) {
		return AgentCoreInvocationInput{}, ErrAgentCoreWorker
	}

	task, err := r.tasks.ResolveAgentTask(ctx, command)
	if err != nil || !workloadagent.ValidateAgentTask(task, workloadagent.IdentityFromCommand(command), spec.CommitSHA) ||
		task.IntegrationID != spec.GitHubIntegrationID || task.InstallationID != spec.GitHubInstallationID ||
		task.RepositoryID != spec.GitHubRepositoryID || task.DefaultBranch != spec.TriggerBranch ||
		!constantTimeEqual(task.ContextSHA256, spec.AgentTaskContextSHA256) {
		return AgentCoreInvocationInput{}, ErrAgentCoreWorker
	}
	prompt := task.Title + "\n\n" + task.Body
	if len(prompt) == 0 || len(prompt) > 64<<10 || !utf8.ValidString(prompt) {
		return AgentCoreInvocationInput{}, ErrAgentCoreWorker
	}

	archive, err := r.sources.OpenSourceArchive(ctx, command, spec.ArtifactPrefix, spec.SourceDigest)
	if err != nil || archive.Body == nil || archive.Size <= 0 || archive.Size > agentCoreSourceArchiveLimit ||
		!constantTimeEqual(archive.SHA256, spec.SourceDigest) {
		if archive.Body != nil {
			_ = archive.Body.Close()
		}
		return AgentCoreInvocationInput{}, ErrAgentCoreWorker
	}
	defer archive.Body.Close()
	content, err := io.ReadAll(io.LimitReader(archive.Body, agentCoreSourceArchiveLimit+1))
	if err != nil || int64(len(content)) != archive.Size || len(content) > agentCoreSourceArchiveLimit {
		return AgentCoreInvocationInput{}, ErrAgentCoreWorker
	}
	digest := sha256.Sum256(content)
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(digest[:])), []byte(spec.SourceDigest)) != 1 {
		return AgentCoreInvocationInput{}, ErrAgentCoreWorker
	}

	return AgentCoreInvocationInput{Specification: spec, Request: workloadagentcore.InvocationRequest{
		Command: command, RepositoryID: spec.GitHubRepositoryID, CommitSHA: spec.CommitSHA,
		SourceSHA256: spec.SourceDigest, Task: prompt, SourceArchive: content,
	}}, nil
}

func constantTimeEqual(left, right string) bool {
	if len(left) == 0 || len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

var _ AgentCoreInvocationResolver = (*BrokeredAgentCoreInvocationResolver)(nil)
