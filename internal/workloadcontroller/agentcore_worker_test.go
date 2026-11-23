package workloadcontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadagentcore"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

type agentCoreWorkerInputResolver struct {
	input AgentCoreInvocationInput
	err   error
	call  int
}

func (r *agentCoreWorkerInputResolver) ResolveAgentCoreInvocation(_ context.Context, _ workloadjournal.Command) (AgentCoreInvocationInput, error) {
	r.call++
	return r.input, r.err
}

type agentCoreWorkerInvoker struct {
	response     []byte
	err          error
	calls        int
	advanceClock *time.Time
	started      chan struct{}
	release      chan struct{}
}

func (i *agentCoreWorkerInvoker) Invoke(ctx context.Context, _ workloadagentcore.Runtime, _ string, _ []byte) ([]byte, error) {
	i.calls++
	if i.started != nil {
		close(i.started)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-i.release:
		}
	}
	if i.advanceClock != nil {
		*i.advanceClock = i.advanceClock.Add(15 * time.Minute)
	}
	return append([]byte(nil), i.response...), i.err
}

func TestAgentCoreWorkloadWorkerRenewsLeaseDuringInvocation(t *testing.T) {
	worker, store, command, invoker, _, _, _ := newAgentCoreWorkerFixture(t, nil)
	worker.config.LeaseHeartbeatInterval = 20 * time.Millisecond
	invoker.started = make(chan struct{})
	invoker.release = make(chan struct{})
	before := mustJournalSnapshot(t, store, command).Journal.Revision
	type outcome struct {
		snapshot workloadjournal.Snapshot
		err      error
	}
	completed := make(chan outcome, 1)
	go func() {
		snapshot, err := worker.Run(context.Background(), command)
		completed <- outcome{snapshot: snapshot, err: err}
	}()
	<-invoker.started
	time.Sleep(80 * time.Millisecond)
	during := mustJournalSnapshot(t, store, command)
	if during.Journal.Revision < before+3 || !during.Journal.AttemptLeaseExpires.After(time.Now().UTC()) {
		close(invoker.release)
		<-completed
		t.Fatalf("lease was not renewed while invocation blocked: before revision=%d during=%+v", before, during.Journal)
	}
	close(invoker.release)
	result := <-completed
	if result.err != nil || !result.snapshot.Journal.ResultPublished {
		t.Fatalf("worker did not finish after releasing invocation: snapshot=%+v err=%v", result.snapshot.Journal, result.err)
	}
}

type agentCoreWorkerStopper struct {
	err               error
	calls             int
	token             string
	failed            bool
	store             *journalDB
	command           workloadjournal.Command
	leaseExpiryAtStop time.Time
}

func (s *agentCoreWorkerStopper) StopRuntimeSession(_ context.Context, _ workloadagentcore.Runtime, session string, token string) error {
	s.calls++
	s.token = token
	if s.store != nil {
		snapshot, err := s.store.Get(context.Background(), s.command.TenantID, s.command.WorkloadRunID, s.command.AttemptID)
		if err == nil && snapshot.Journal.RuntimeHandle == session {
			s.leaseExpiryAtStop = snapshot.Journal.AttemptLeaseExpires
		}
	}
	return s.err
}

type agentCoreWorkerPublisher struct {
	store   *journalDB
	command workloadjournal.Command
	results []workloadjobs.WorkloadResult
	err     error
}

func (p *agentCoreWorkerPublisher) Publish(_ context.Context, result workloadjobs.WorkloadResult) error {
	snapshot, err := p.store.Get(context.Background(), p.command.TenantID, p.command.WorkloadRunID, p.command.AttemptID)
	if err != nil || snapshot.Journal.CleanupState != workloadjournal.CleanupConfirmed || snapshot.Journal.ResultPublished {
		return errors.New("publisher was called before cleanup confirmation")
	}
	p.results = append(p.results, result)
	return p.err
}

func newAgentCoreWorkerFixture(t *testing.T, stopErr error) (*AgentCoreWorkloadWorker, *journalDB,
	workloadjournal.Command, *agentCoreWorkerInvoker, *agentCoreWorkerStopper, *agentCoreWorkerPublisher, *time.Time) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	clock := now
	command := workloadjournal.Command{TenantID: "tenant_worker", WorkloadRunID: "run_worker", AttemptID: "attempt_worker",
		Generation: 1, ResourcePlaneID: "rp_worker", IdempotencyToken: "token_worker"}
	source := []byte("immutable source bytes")
	sourceSum := sha256.Sum256(source)
	commit := strings.Repeat("b", 40)
	spec := Specification{Identity: command, Kind: domain.WorkloadAgent, ImageDigest: strings.Repeat("d", 64),
		SourceDigest: hex.EncodeToString(sourceSum[:]), CommitSHA: commit, GitHubIntegrationID: "integration_worker",
		GitHubInstallationID: 71, GitHubRepositoryID: 97, AgentTaskContextSHA256: strings.Repeat("c", 64),
		TriggerEvent: "issue_comment", TriggerBranch: "main", CorrelationID: "req_12345678-1234-1234-1234-123456789abc",
		Deadline: now.Add(time.Hour), ArtifactPrefix: "s3://artifacts/prefix/tenant_worker/run_worker/attempt_worker/"}
	specHash, err := specificationHash(spec, command)
	if err != nil {
		t.Fatal(err)
	}
	specPayload, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := workloadjournal.New(command, now)
	if err != nil {
		t.Fatal(err)
	}
	journal.SpecificationHash, journal.SpecificationPayload = specHash, specPayload
	db := &journalDB{rows: make(map[string]workloadjournal.Snapshot)}
	if _, err := db.Create(context.Background(), journal); err != nil {
		t.Fatal(err)
	}
	runtime := workloadagentcore.Runtime{ARN: "arn:aws:bedrock-agentcore:ap-northeast-1:123456789012:runtime/test", Qualifier: "DEFAULT"}
	prepared, err := workloadagentcore.Prepare(context.Background(), db, mustJournalSnapshot(t, db, command), runtime, now)
	if err != nil {
		t.Fatal(err)
	}
	input := workloadagentcore.InvocationRequest{Command: command, RepositoryID: spec.GitHubRepositoryID,
		CommitSHA: spec.CommitSHA, SourceSHA256: spec.SourceDigest, Task: "implement the request", SourceArchive: source}
	requestBody, err := json.Marshal(map[string]any{"schema_version": 1, "attempt": map[string]any{
		"tenant_id": command.TenantID, "run_id": command.WorkloadRunID, "attempt_id": command.AttemptID,
		"resource_plane_id": command.ResourcePlaneID, "generation": command.Generation, "repository_id": input.RepositoryID,
		"commit_sha": input.CommitSHA, "source_sha256": input.SourceSHA256}, "draft_status": "no_proposal", "iterations": 1,
		"tree_digest": strings.Repeat("a", 64), "proposal": nil})
	if err != nil {
		t.Fatal(err)
	}
	invoker := &agentCoreWorkerInvoker{response: requestBody}
	stopper := &agentCoreWorkerStopper{err: stopErr}
	stopper.store, stopper.command = db, command
	publisher := &agentCoreWorkerPublisher{store: db, command: command}
	inputs := &agentCoreWorkerInputResolver{input: AgentCoreInvocationInput{Specification: spec, Request: input}}
	worker, err := NewAgentCoreWorkloadWorker(AgentCoreWorkloadWorkerConfig{Journal: db, Inputs: inputs, Invoker: invoker,
		Stopper: stopper, Publisher: publisher, LeaseTTL: 20 * time.Minute, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	_ = prepared
	return worker, db, command, invoker, stopper, publisher, &clock
}

func mustJournalSnapshot(t *testing.T, store *journalDB, command workloadjournal.Command) workloadjournal.Snapshot {
	t.Helper()
	snapshot, err := store.Get(context.Background(), command.TenantID, command.WorkloadRunID, command.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestAgentCoreWorkloadWorkerRunsCleanupAndPublicationInOrder(t *testing.T) {
	worker, store, command, invoker, stopper, publisher, clock := newAgentCoreWorkerFixture(t, nil)
	invoker.advanceClock = clock
	completed, err := worker.Run(context.Background(), command)
	if err != nil || completed.Journal.ExecutionState != workloadjournal.Succeeded ||
		completed.Journal.CleanupState != workloadjournal.CleanupConfirmed || !completed.Journal.ResultPublished ||
		invoker.calls != 1 || stopper.calls != 1 || len(publisher.results) != 1 || publisher.results[0].ChangeBundle != nil ||
		!stopper.leaseExpiryAtStop.Equal(clock.Add(20*time.Minute)) {
		t.Fatalf("worker result=%+v invoke=%d stop=%d published=%+v err=%v", completed.Journal, invoker.calls, stopper.calls, publisher.results, err)
	}
	replayed, err := worker.Run(context.Background(), command)
	if err != nil || replayed.Journal.ResultPublished != true || invoker.calls != 1 || stopper.calls != 1 || len(publisher.results) != 1 {
		t.Fatalf("terminal replay changed lifecycle: result=%+v invoke=%d stop=%d publications=%d err=%v", replayed.Journal, invoker.calls, stopper.calls, len(publisher.results), err)
	}
	if _, err := store.Get(context.Background(), command.TenantID, command.WorkloadRunID, command.AttemptID); err != nil {
		t.Fatal(err)
	}
}

func TestAgentCoreWorkloadWorkerRetriesCleanupWithoutReinvoking(t *testing.T) {
	worker, store, command, invoker, stopper, publisher, clock := newAgentCoreWorkerFixture(t, errors.New("ambiguous stop"))
	first, err := worker.Run(context.Background(), command)
	if err == nil || first.Journal.ExecutionState != workloadjournal.Succeeded || first.Journal.CleanupState != workloadjournal.CleanupFailed ||
		first.Journal.ResultPublished || invoker.calls != 1 || stopper.calls != 1 || len(publisher.results) != 0 {
		t.Fatalf("failed cleanup published or reinvoked: result=%+v invoke=%d stop=%d publish=%d err=%v", first.Journal, invoker.calls, stopper.calls, len(publisher.results), err)
	}
	firstToken := stopper.token
	stopper.err = nil
	*clock = clock.Add(time.Second)
	completed, err := worker.Run(context.Background(), command)
	if err != nil || completed.Journal.CleanupState != workloadjournal.CleanupConfirmed || !completed.Journal.ResultPublished ||
		invoker.calls != 1 || stopper.calls != 2 || stopper.token != firstToken || len(publisher.results) != 1 {
		t.Fatalf("cleanup retry = journal:%+v invoke=%d stop=%d published=%d err=%v", completed.Journal, invoker.calls, stopper.calls, len(publisher.results), err)
	}
	if _, err := store.Get(context.Background(), command.TenantID, command.WorkloadRunID, command.AttemptID); err != nil {
		t.Fatal(err)
	}
}

func TestAgentCoreWorkloadWorkerLostInvocationResponseFailsClosed(t *testing.T) {
	worker, _, command, invoker, stopper, publisher, _ := newAgentCoreWorkerFixture(t, nil)
	invoker.err = errors.New("lost provider response")
	completed, err := worker.Run(context.Background(), command)
	if err != nil || completed.Journal.ExecutionState != workloadjournal.Failed ||
		completed.Journal.CleanupState != workloadjournal.CleanupConfirmed || !completed.Journal.ResultPublished ||
		invoker.calls != 1 || stopper.calls != 1 || len(publisher.results) != 1 ||
		publisher.results[0].ErrorCode != "agentcore_invocation_unknown" {
		t.Fatalf("lost response outcome = journal:%+v invoke=%d stop=%d results=%+v err=%v", completed.Journal, invoker.calls, stopper.calls, publisher.results, err)
	}
}
