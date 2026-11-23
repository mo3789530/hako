package workloadcontroller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

type agentCoreResolverTaskProvider struct {
	task  workloadagent.AgentTask
	err   error
	calls int
}

func (p *agentCoreResolverTaskProvider) ResolveAgentTask(context.Context, workloadjournal.Command) (workloadagent.AgentTask, error) {
	p.calls++
	return p.task, p.err
}

type agentCoreResolverArchiveReader struct {
	archive workloadsource.SourceArchive
	command workloadjournal.Command
	prefix  string
	digest  string
	calls   int
	err     error
}

func (r *agentCoreResolverArchiveReader) OpenSourceArchive(_ context.Context, command workloadjournal.Command,
	prefix, digest string) (workloadsource.SourceArchive, error) {
	r.calls++
	r.command, r.prefix, r.digest = command, prefix, digest
	return r.archive, r.err
}

func newAgentCoreResolverFixture(t *testing.T) (*journalDB, workloadjournal.Command, Specification,
	workloadagent.AgentTask, []byte) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	command := workloadjournal.Command{TenantID: "tenant_input", WorkloadRunID: "run_input", AttemptID: "attempt_input",
		Generation: 1, ResourcePlaneID: "rp_input", IdempotencyToken: "idempotency_input"}
	source := []byte("verified source archive bytes")
	sourceSum := sha256.Sum256(source)
	task := workloadagent.AgentTask{TenantID: command.TenantID, WorkloadRunID: command.WorkloadRunID,
		IntegrationID: "integration_input", DeliveryID: "delivery_0123456789abcd", InstallationID: 33,
		RepositoryID: 44, IssueNumber: 71, CommentID: 82, DefaultBranch: "main", CommitSHA: strings.Repeat("b", 40),
		Title: "Add a feature", Body: "Please implement the bounded request."}
	task.ContextSHA256 = workloadagent.AgentTaskContextSHA256(task)
	spec := Specification{Identity: command, Kind: domain.WorkloadAgent, ImageDigest: strings.Repeat("a", 64),
		SourceDigest: hex.EncodeToString(sourceSum[:]), CommitSHA: task.CommitSHA, GitHubIntegrationID: task.IntegrationID,
		GitHubInstallationID: task.InstallationID, GitHubRepositoryID: task.RepositoryID,
		AgentTaskContextSHA256: task.ContextSHA256, TriggerEvent: "issue_comment", TriggerBranch: task.DefaultBranch,
		CorrelationID: "req_input", Deadline: now.Add(time.Hour), RequestedAt: now,
		ArtifactPrefix: "s3://agent-artifacts/tenant_input/run_input/attempt_input/"}
	journal, err := workloadjournal.New(command, now)
	if err != nil {
		t.Fatal(err)
	}
	specHash, err := specificationHash(spec, command)
	if err != nil {
		t.Fatal(err)
	}
	specPayload, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	journal.SpecificationHash, journal.SpecificationPayload = specHash, specPayload
	store := &journalDB{rows: make(map[string]workloadjournal.Snapshot)}
	if _, err := store.Create(context.Background(), journal); err != nil {
		t.Fatal(err)
	}
	return store, command, spec, task, source
}

func TestBrokeredAgentCoreInvocationResolverBindsTaskAndArchive(t *testing.T) {
	store, command, spec, task, source := newAgentCoreResolverFixture(t)
	sum := sha256.Sum256(source)
	reader := &agentCoreResolverArchiveReader{archive: workloadsource.SourceArchive{
		Body: io.NopCloser(bytes.NewReader(source)), Size: int64(len(source)), SHA256: hex.EncodeToString(sum[:]),
	}}
	tasks := &agentCoreResolverTaskProvider{task: task}
	resolver, err := NewBrokeredAgentCoreInvocationResolver(store, tasks, reader)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.ResolveAgentCoreInvocation(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resolved.Specification, spec) || resolved.Request.Command != command || resolved.Request.RepositoryID != task.RepositoryID ||
		resolved.Request.CommitSHA != task.CommitSHA || resolved.Request.SourceSHA256 != spec.SourceDigest ||
		resolved.Request.Task != task.Title+"\n\n"+task.Body || !bytes.Equal(resolved.Request.SourceArchive, source) {
		t.Fatal("resolver changed trusted pins or task/source bytes")
	}
	if tasks.calls != 1 || reader.calls != 1 || reader.command != command || reader.prefix != spec.ArtifactPrefix || reader.digest != spec.SourceDigest {
		t.Fatal("resolver did not use exact attempt-scoped broker and archive coordinates")
	}
}

func TestBrokeredAgentCoreInvocationResolverRejectsUnpinnedInputs(t *testing.T) {
	tests := []struct {
		name       string
		mutateTask func(*workloadagent.AgentTask)
		mutateSrc  func(*workloadsource.SourceArchive)
	}{
		{name: "foreign task", mutateTask: func(task *workloadagent.AgentTask) { task.RepositoryID++ }},
		{name: "changed task context", mutateTask: func(task *workloadagent.AgentTask) { task.Body = "changed" }},
		{name: "archive digest metadata", mutateSrc: func(src *workloadsource.SourceArchive) { src.SHA256 = strings.Repeat("0", 64) }},
		{name: "archive byte digest", mutateSrc: func(src *workloadsource.SourceArchive) { src.Body = io.NopCloser(strings.NewReader("tampered bytes")) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, command, _, task, source := newAgentCoreResolverFixture(t)
			sum := sha256.Sum256(source)
			src := workloadsource.SourceArchive{Body: io.NopCloser(bytes.NewReader(source)), Size: int64(len(source)), SHA256: hex.EncodeToString(sum[:])}
			if test.mutateTask != nil {
				test.mutateTask(&task)
			}
			if test.mutateSrc != nil {
				test.mutateSrc(&src)
			}
			resolver, err := NewBrokeredAgentCoreInvocationResolver(store,
				&agentCoreResolverTaskProvider{task: task}, &agentCoreResolverArchiveReader{archive: src})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := resolver.ResolveAgentCoreInvocation(context.Background(), command); !errors.Is(err, ErrAgentCoreWorker) {
				t.Fatalf("expected safe rejection, got %v", err)
			}
		})
	}
}

func TestBrokeredAgentCoreInvocationResolverRequiresExactJournalCommand(t *testing.T) {
	store, command, _, task, source := newAgentCoreResolverFixture(t)
	sum := sha256.Sum256(source)
	resolver, err := NewBrokeredAgentCoreInvocationResolver(store, &agentCoreResolverTaskProvider{task: task},
		&agentCoreResolverArchiveReader{archive: workloadsource.SourceArchive{Body: io.NopCloser(bytes.NewReader(source)),
			Size: int64(len(source)), SHA256: hex.EncodeToString(sum[:])}})
	if err != nil {
		t.Fatal(err)
	}
	foreign := command
	foreign.AttemptID = "attempt_foreign"
	if _, err := resolver.ResolveAgentCoreInvocation(context.Background(), foreign); !errors.Is(err, ErrAgentCoreWorker) {
		t.Fatalf("expected foreign attempt rejection, got %v", err)
	}
}
