package workloadagentcore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/workloadjournal"
)

type fakeRuntimeInvoker struct {
	target  Runtime
	session string
	body    []byte
	result  []byte
	err     error
	calls   int
}

func (f *fakeRuntimeInvoker) Invoke(_ context.Context, target Runtime, session string, body []byte) ([]byte, error) {
	f.calls++
	f.target, f.session, f.body = target, session, append([]byte(nil), body...)
	return append([]byte(nil), f.result...), f.err
}

func validRuntimeResponse(t *testing.T, input InvocationRequest) []byte {
	t.Helper()
	response, err := json.Marshal(runtimeInvocationResponse{SchemaVersion: 1, Attempt: attemptPins(input),
		DraftStatus: "no_proposal", Iterations: 1, TreeDigest: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestInvokeOnceUsesFrozenTargetAndClaimsBeforeProvider(t *testing.T) {
	prepared, input, now := preparedAgentCoreInput(t)
	provider := &fakeRuntimeInvoker{result: validRuntimeResponse(t, input)}
	claimed, draft, err := InvokeOnce(context.Background(), prepared.store, prepared.snapshot, prepared.lease, input, provider, now)
	if err != nil || draft.Status != "no_proposal" || draft.Attempt != attemptPins(input) || provider.calls != 1 ||
		provider.target != prepared.runtime || provider.session != prepared.snapshot.Journal.RuntimeHandle ||
		claimed.Journal.ExecutionState != workloadjournal.Running {
		t.Fatalf("invoke = journal:%+v draft:%+v err:%v provider:%+v", claimed.Journal, draft, err, provider)
	}
	if len(provider.body) == 0 {
		t.Fatal("provider received no serialized request")
	}
}

func TestInvokeOnceLostResponseCannotBeReplayed(t *testing.T) {
	prepared, input, now := preparedAgentCoreInput(t)
	provider := &fakeRuntimeInvoker{err: errors.New("sensitive provider detail")}
	claimed, draft, err := InvokeOnce(context.Background(), prepared.store, prepared.snapshot, prepared.lease, input, provider, now)
	if !errors.Is(err, ErrInvocationOutcomeUnknown) || draft.Status != "" || claimed.Journal.ExecutionState != workloadjournal.Running {
		t.Fatalf("ambiguous invocation = journal:%+v draft:%+v err:%v", claimed.Journal, draft, err)
	}
	_, _, retryErr := InvokeOnce(context.Background(), prepared.store, claimed, prepared.lease, input, provider, now.Add(time.Second))
	if !errors.Is(retryErr, ErrNotReady) || provider.calls != 1 {
		t.Fatalf("ambiguous invocation was replayed: calls=%d err=%v", provider.calls, retryErr)
	}
}

func TestInvokeOnceRejectsForeignRequestBeforeClaim(t *testing.T) {
	prepared, input, now := preparedAgentCoreInput(t)
	input.Command.AttemptID = "foreign-attempt"
	provider := &fakeRuntimeInvoker{result: validRuntimeResponse(t, input)}
	if _, _, err := InvokeOnce(context.Background(), prepared.store, prepared.snapshot, prepared.lease, input, provider, now); !errors.Is(err, ErrNotReady) {
		t.Fatalf("foreign attempt request accepted: %v", err)
	}
	if provider.calls != 0 || prepared.store.snapshot.Journal.ExecutionState != workloadjournal.Launching {
		t.Fatalf("foreign request had side effects: calls=%d journal=%+v", provider.calls, prepared.store.snapshot.Journal)
	}
}

func TestInvokeOnceInvalidResponseCannotBeReplayed(t *testing.T) {
	prepared, input, now := preparedAgentCoreInput(t)
	provider := &fakeRuntimeInvoker{result: []byte(`{"schema_version":1,"draft_status":"verified"}`)}
	claimed, _, err := InvokeOnce(context.Background(), prepared.store, prepared.snapshot, prepared.lease, input, provider, now)
	if !errors.Is(err, ErrInvocationOutcomeUnknown) || provider.calls != 1 || claimed.Journal.ExecutionState != workloadjournal.Running {
		t.Fatalf("invalid provider response accepted: journal=%+v calls=%d err=%v", claimed.Journal, provider.calls, err)
	}
	if _, _, err := InvokeOnce(context.Background(), prepared.store, claimed, prepared.lease, input, provider, now.Add(time.Second)); !errors.Is(err, ErrNotReady) || provider.calls != 1 {
		t.Fatalf("invalid response was replayed: calls=%d err=%v", provider.calls, err)
	}
}

type preparedInput struct {
	store    *testStore
	snapshot workloadjournal.Snapshot
	runtime  Runtime
	lease    workloadjournal.AttemptLease
}

func preparedAgentCoreInput(t *testing.T) (preparedInput, InvocationRequest, time.Time) {
	t.Helper()
	now := time.Now().UTC()
	command := workloadjournal.Command{TenantID: "tenant_invoker", WorkloadRunID: "run_invoker", AttemptID: "attempt_invoker",
		Generation: 2, ResourcePlaneID: "rp_invoker", IdempotencyToken: "token_invoker"}
	journal, err := workloadjournal.New(command, now)
	if err != nil {
		t.Fatal(err)
	}
	store := &testStore{snapshot: workloadjournal.Snapshot{Journal: journal, ETag: "initial"}}
	runtime := Runtime{ARN: "arn:aws:bedrock-agentcore:ap-northeast-1:123456789012:runtime/hako", Qualifier: "DEFAULT"}
	preparedSnapshot, err := Prepare(context.Background(), store, store.snapshot, runtime, now)
	if err != nil {
		t.Fatal(err)
	}
	store.snapshot = preparedSnapshot
	lease, preparedSnapshot, err := workloadjournal.AcquireAttemptLease(context.Background(), store, command,
		"owner-invoker-test", now.Add(time.Second), 20*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	store.snapshot = preparedSnapshot
	archive := []byte("attempt-scoped source")
	digest := sha256.Sum256(archive)
	input := InvocationRequest{Command: command, RepositoryID: 97, CommitSHA: "0123456789abcdef0123456789abcdef01234567",
		SourceSHA256: hex.EncodeToString(digest[:]), Task: "inspect source", SourceArchive: archive}
	return preparedInput{store: store, snapshot: preparedSnapshot, runtime: runtime, lease: lease}, input, now.Add(2 * time.Second)
}
