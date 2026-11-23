package workloadagentcore

import (
	"context"
	"time"

	"github.com/mo3789530/hako/internal/workloadjournal"
)

// ClaimInvocation durably claims the only InvokeAgentRuntime call for a
// prepared attempt. Moving Launching to Running before the network request is
// intentional: after a crash, Running means the call may have reached AWS and
// must be reconciled/stopped, never blindly invoked again.
func ClaimInvocation(ctx context.Context, store workloadjournal.Store, current workloadjournal.Snapshot, now time.Time) (workloadjournal.Snapshot, error) {
	if ctx == nil || ctx.Err() != nil || store == nil || current.ETag == "" || current.Journal.Validate() != nil || now.IsZero() {
		return workloadjournal.Snapshot{}, ErrNotReady
	}
	j := current.Journal
	if j.ExecutionState != workloadjournal.Launching || j.CancelRequested || j.RuntimeHandle == "" ||
		len(j.RuntimeSpecPayload) == 0 || now.Before(j.UpdatedAt) {
		return workloadjournal.Snapshot{}, ErrNotReady
	}
	if _, err := PreparedInvocationFromJournal(j); err != nil {
		return workloadjournal.Snapshot{}, ErrNotReady
	}
	j.ExecutionState = workloadjournal.Running
	j.Revision++
	j.UpdatedAt = now.UTC()
	if workloadjournal.ValidateUpdate(current, j) != nil {
		return workloadjournal.Snapshot{}, ErrNotReady
	}
	return store.CompareAndSwap(ctx, current, j)
}
