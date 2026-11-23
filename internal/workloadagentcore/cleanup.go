package workloadagentcore

import (
	"context"
	"errors"
	"time"

	"github.com/mo3789530/hako/internal/workloadjournal"
)

var ErrCleanupUnconfirmed = errors.New("AgentCore session cleanup is unconfirmed")

// RuntimeSessionStopper is the provider boundary for idempotent session stop.
// A successful return must mean the frozen session is terminated and the
// caller's exclusive attempt lease has already fenced/drained Invoke calls.
type RuntimeSessionStopper interface {
	StopRuntimeSession(context.Context, Runtime, string, string) error
}

// StopAndConfirmCleanup may be called only while the caller holds the same
// exclusive attempt lease used by InvokeOnce and cancellation. It retries a
// terminal attempt's stop with one deterministic client token and records
// CleanupConfirmed only after the provider positively confirms termination.
// Ambiguous provider responses, including an unproven not-found response, stay
// cleanup-pending/failed and cannot authorize result publication.
func StopAndConfirmCleanup(ctx context.Context, store workloadjournal.Store, current workloadjournal.Snapshot,
	lease workloadjournal.AttemptLease, stopper RuntimeSessionStopper, now time.Time) (workloadjournal.Snapshot, error) {
	if ctx == nil || ctx.Err() != nil || store == nil || stopper == nil || current.ETag == "" ||
		current.Journal.Validate() != nil || !workloadjournal.AttemptLeaseHeld(current, lease, now) ||
		now.IsZero() || now.Before(current.Journal.UpdatedAt) {
		return workloadjournal.Snapshot{}, ErrNotReady
	}
	if terminalExecution(current.Journal.ExecutionState) && current.Journal.CleanupState == workloadjournal.CleanupConfirmed {
		return current, nil
	}
	prepared, err := PreparedCleanupFromJournal(current.Journal)
	if err != nil {
		return workloadjournal.Snapshot{}, err
	}
	command := journalCommand(current.Journal)
	token, err := StopClientToken(command)
	if err != nil {
		return workloadjournal.Snapshot{}, ErrNotReady
	}
	if err := stopper.StopRuntimeSession(ctx, prepared.Runtime, prepared.SessionID, token); err != nil {
		if current.Journal.CleanupState == workloadjournal.CleanupPending {
			next := current.Journal
			next.CleanupState = workloadjournal.CleanupFailed
			next.Revision++
			next.UpdatedAt = now.UTC()
			if workloadjournal.ValidateUpdate(current, next) == nil {
				if failed, updateErr := store.CompareAndSwap(ctx, current, next); updateErr == nil {
					current = failed
				}
			}
		}
		return current, ErrCleanupUnconfirmed
	}
	next := current.Journal
	next.CleanupState = workloadjournal.CleanupConfirmed
	next.Revision++
	next.UpdatedAt = now.UTC()
	if workloadjournal.ValidateUpdate(current, next) != nil {
		return current, ErrCleanupUnconfirmed
	}
	confirmed, err := store.CompareAndSwap(ctx, current, next)
	if err == nil {
		return confirmed, nil
	}
	latest, loadErr := store.Get(ctx, current.Journal.TenantID, current.Journal.WorkloadRunID, current.Journal.AttemptID)
	if loadErr == nil && latest.Journal.RuntimeHandle == current.Journal.RuntimeHandle &&
		latest.Journal.CleanupState == workloadjournal.CleanupConfirmed {
		return latest, nil
	}
	return current, ErrCleanupUnconfirmed
}
