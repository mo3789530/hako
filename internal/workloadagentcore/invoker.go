package workloadagentcore

import (
	"context"
	"errors"
	"time"

	"github.com/mo3789530/hako/internal/workloadjournal"
)

var ErrInvocationOutcomeUnknown = errors.New("AgentCore invocation outcome is unknown; reconcile without reinvoking")

// RuntimeInvoker is the provider boundary. Implementations use only the frozen
// Runtime target and session supplied here; repository input never selects it.
type RuntimeInvoker interface {
	Invoke(context.Context, Runtime, string, []byte) ([]byte, error)
}

// InvokeOnce durably claims an invocation before calling the provider. The
// caller must hold its exclusive attempt lease from before this call through
// its result/cleanup checkpoint and must recheck cancellation under that lease.
// A lost response intentionally leaves the journal Running; recovery must stop
// or reconcile the frozen session and may not invoke it again.
func InvokeOnce(ctx context.Context, store workloadjournal.Store, current workloadjournal.Snapshot,
	lease workloadjournal.AttemptLease, input InvocationRequest, invoker RuntimeInvoker, now time.Time) (workloadjournal.Snapshot, InvocationDraft, error) {
	if ctx == nil || ctx.Err() != nil || store == nil || invoker == nil || current.Journal.Validate() != nil ||
		input.Command != journalCommand(current.Journal) || !workloadjournal.AttemptLeaseHeld(current, lease, now) {
		return workloadjournal.Snapshot{}, InvocationDraft{}, ErrNotReady
	}
	prepared, err := PreparedInvocationFromJournal(current.Journal)
	if err != nil {
		return workloadjournal.Snapshot{}, InvocationDraft{}, ErrNotReady
	}
	body, err := EncodeInvocationRequest(input)
	if err != nil {
		return workloadjournal.Snapshot{}, InvocationDraft{}, ErrNotReady
	}
	claimed, err := ClaimInvocation(ctx, store, current, now)
	if err != nil {
		return workloadjournal.Snapshot{}, InvocationDraft{}, err
	}
	response, err := invoker.Invoke(ctx, prepared.Runtime, prepared.SessionID, body)
	if err != nil || len(response) == 0 || len(response) > maxInvocationResponseBytes {
		return claimed, InvocationDraft{}, ErrInvocationOutcomeUnknown
	}
	draft, err := DecodeInvocationResponse(response, input)
	if err != nil {
		// The provider call is already claimed. Even an invalid response cannot
		// make this attempt eligible for another invoke; recovery must clean up.
		return claimed, InvocationDraft{}, ErrInvocationOutcomeUnknown
	}
	return claimed, draft, nil
}

func journalCommand(j workloadjournal.Journal) workloadjournal.Command {
	return workloadjournal.Command{TenantID: j.TenantID, WorkloadRunID: j.WorkloadRunID,
		AttemptID: j.AttemptID, Generation: j.Generation, ResourcePlaneID: j.ResourcePlaneID,
		IdempotencyToken: j.IdempotencyToken}
}
