package workloadagentcore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/workloadjournal"
)

type fakeRuntimeStopper struct {
	target  Runtime
	session string
	token   string
	err     error
	calls   int
}

func (f *fakeRuntimeStopper) StopRuntimeSession(_ context.Context, target Runtime, session, token string) error {
	f.calls++
	f.target, f.session, f.token = target, session, token
	return f.err
}

func terminalInvocation(t *testing.T) (preparedInput, time.Time) {
	t.Helper()
	prepared, _, now := preparedAgentCoreInput(t)
	running, err := ClaimInvocation(context.Background(), prepared.store, prepared.snapshot, now)
	if err != nil {
		t.Fatal(err)
	}
	terminal := running.Journal
	terminal.ExecutionState = workloadjournal.Succeeded
	terminal.CleanupState = workloadjournal.CleanupPending
	terminal.Revision++
	terminal.UpdatedAt = now.Add(time.Second)
	prepared.snapshot, err = prepared.store.CompareAndSwap(context.Background(), running, terminal)
	if err != nil {
		t.Fatal(err)
	}
	return prepared, now.Add(2 * time.Second)
}

func TestStopAndConfirmCleanupUsesFrozenSessionAndDeterministicRetryToken(t *testing.T) {
	prepared, now := terminalInvocation(t)
	stopper := &fakeRuntimeStopper{err: errors.New("provider response is ambiguous")}
	failed, err := StopAndConfirmCleanup(context.Background(), prepared.store, prepared.snapshot, prepared.lease, stopper, now)
	if !errors.Is(err, ErrCleanupUnconfirmed) || failed.Journal.CleanupState != workloadjournal.CleanupFailed ||
		stopper.calls != 1 || stopper.target != prepared.runtime || stopper.session != prepared.snapshot.Journal.RuntimeHandle || stopper.token == "" {
		t.Fatalf("failed cleanup = journal:%+v stopper:%+v err:%v", failed.Journal, stopper, err)
	}
	token := stopper.token
	stopper.err = nil
	confirmed, err := StopAndConfirmCleanup(context.Background(), prepared.store, failed, prepared.lease, stopper, now.Add(time.Second))
	if err != nil || confirmed.Journal.CleanupState != workloadjournal.CleanupConfirmed || stopper.calls != 2 || stopper.token != token {
		t.Fatalf("confirmed cleanup = journal:%+v stopper:%+v err:%v", confirmed.Journal, stopper, err)
	}
	if _, err := StopAndConfirmCleanup(context.Background(), prepared.store, confirmed, prepared.lease, stopper, now.Add(2*time.Second)); err != nil || stopper.calls != 2 {
		t.Fatalf("confirmed cleanup was called again: calls=%d err=%v", stopper.calls, err)
	}
}

func TestStopAndConfirmCleanupRejectsNonterminalOrForeignJournal(t *testing.T) {
	prepared, _, now := preparedAgentCoreInput(t)
	stopper := &fakeRuntimeStopper{}
	if _, err := StopAndConfirmCleanup(context.Background(), prepared.store, prepared.snapshot, prepared.lease, stopper, now); !errors.Is(err, ErrNotReady) {
		t.Fatalf("stopped a nonterminal attempt: %v", err)
	}
	terminal, later := terminalInvocation(t)
	terminal.snapshot.Journal.RuntimeSpecPayload = append([]byte(nil), terminal.snapshot.Journal.RuntimeSpecPayload...)
	terminal.snapshot.Journal.RuntimeHandle = "hako-foreign"
	if _, err := StopAndConfirmCleanup(context.Background(), terminal.store, terminal.snapshot, terminal.lease, stopper, later); !errors.Is(err, ErrNotReady) {
		t.Fatalf("stopped a foreign frozen session: %v", err)
	}
	if stopper.calls != 0 {
		t.Fatalf("invalid journal caused provider side effects: %d", stopper.calls)
	}
}
