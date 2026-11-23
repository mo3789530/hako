package workloadagentcore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"

	"github.com/mo3789530/hako/internal/workloadjournal"
)

var ErrNotReady = errors.New("AgentCore attempt cannot be prepared")

var runtimeARN = regexp.MustCompile(`^arn:(aws|aws-us-gov|aws-cn):bedrock-agentcore:[a-z0-9-]+:[0-9]{12}:runtime/[A-Za-z0-9_-]{1,128}$`)
var qualifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Runtime identifies a trusted, preconfigured AgentCore Runtime endpoint.
type Runtime struct {
	ARN       string `json:"runtime_arn"`
	Qualifier string `json:"qualifier"`
}

type frozenSpecification struct {
	Version   int     `json:"version"`
	Backend   string  `json:"backend"`
	SessionID string  `json:"session_id"`
	Runtime   Runtime `json:"runtime"`
}

// PreparedInvocation is the exact trusted endpoint and session recovered from
// the immutable journal specification. Callers must use this value after a
// restart instead of re-resolving Runtime configuration from a queue message.
type PreparedInvocation struct {
	SessionID string
	Runtime   Runtime
}

// PreparedInvocationFromJournal strictly decodes the frozen invocation
// contract and verifies its session binding before any provider action.
func PreparedInvocationFromJournal(j workloadjournal.Journal) (PreparedInvocation, error) {
	if j.Validate() != nil || (j.ExecutionState != workloadjournal.Launching && j.ExecutionState != workloadjournal.Running) ||
		j.RuntimeHandle == "" || len(j.RuntimeSpecPayload) == 0 {
		return PreparedInvocation{}, ErrNotReady
	}
	return preparedInvocationFromFrozenJournal(j)
}

// PreparedCleanupFromJournal restores the same frozen endpoint after the
// execution is terminal and cleanup is pending. A terminal attempt can never
// be invoked again; this value is only for idempotent stop/recovery.
func PreparedCleanupFromJournal(j workloadjournal.Journal) (PreparedInvocation, error) {
	if j.Validate() != nil || !terminalExecution(j.ExecutionState) ||
		(j.CleanupState != workloadjournal.CleanupPending && j.CleanupState != workloadjournal.CleanupFailed) ||
		j.RuntimeHandle == "" || len(j.RuntimeSpecPayload) == 0 {
		return PreparedInvocation{}, ErrNotReady
	}
	return preparedInvocationFromFrozenJournal(j)
}

func preparedInvocationFromFrozenJournal(j workloadjournal.Journal) (PreparedInvocation, error) {
	decoder := json.NewDecoder(bytes.NewReader(j.RuntimeSpecPayload))
	decoder.DisallowUnknownFields()
	var frozen frozenSpecification
	if decoder.Decode(&frozen) != nil || decoder.Decode(new(any)) != io.EOF || frozen.Version != 1 || frozen.Backend != "agentcore" ||
		!runtimeARN.MatchString(frozen.Runtime.ARN) || !qualifier.MatchString(frozen.Runtime.Qualifier) ||
		!identityPart.MatchString(frozen.SessionID) || frozen.SessionID != j.RuntimeHandle {
		return PreparedInvocation{}, ErrNotReady
	}
	expected, err := SessionID(workloadjournal.Command{TenantID: j.TenantID, WorkloadRunID: j.WorkloadRunID,
		AttemptID: j.AttemptID, Generation: j.Generation, ResourcePlaneID: j.ResourcePlaneID, IdempotencyToken: j.IdempotencyToken})
	if err != nil || expected != frozen.SessionID {
		return PreparedInvocation{}, ErrNotReady
	}
	return PreparedInvocation{SessionID: frozen.SessionID, Runtime: frozen.Runtime}, nil
}

func terminalExecution(state workloadjournal.ExecutionState) bool {
	switch state {
	case workloadjournal.Succeeded, workloadjournal.Failed, workloadjournal.Cancelled, workloadjournal.TimedOut:
		return true
	default:
		return false
	}
}

// Prepare freezes the session and runtime before any invocation. A successful
// return is only a checkpoint, not a lease or permission to invoke AWS. Callers
// must separately fence concurrent invocations and cancellation.
func Prepare(ctx context.Context, store workloadjournal.Store, current workloadjournal.Snapshot, runtime Runtime, now time.Time) (workloadjournal.Snapshot, error) {
	if ctx == nil || ctx.Err() != nil || store == nil || current.ETag == "" || current.Journal.Validate() != nil ||
		!runtimeARN.MatchString(runtime.ARN) || !qualifier.MatchString(runtime.Qualifier) || now.IsZero() {
		return workloadjournal.Snapshot{}, ErrNotReady
	}
	j := current.Journal
	if j.ExecutionState != workloadjournal.Accepted || j.CancelRequested || j.RuntimeHandle != "" ||
		j.RuntimeSpecHash != "" || len(j.RuntimeSpecPayload) != 0 || now.Before(j.UpdatedAt) {
		return workloadjournal.Snapshot{}, ErrNotReady
	}
	session, err := SessionID(workloadjournal.Command{TenantID: j.TenantID, WorkloadRunID: j.WorkloadRunID,
		AttemptID: j.AttemptID, Generation: j.Generation, ResourcePlaneID: j.ResourcePlaneID, IdempotencyToken: j.IdempotencyToken})
	if err != nil {
		return workloadjournal.Snapshot{}, ErrNotReady
	}
	payload, err := json.Marshal(frozenSpecification{Version: 1, Backend: "agentcore", SessionID: session, Runtime: runtime})
	if err != nil {
		return workloadjournal.Snapshot{}, ErrNotReady
	}
	digest := sha256.Sum256(payload)
	j.ExecutionState = workloadjournal.Launching
	j.RuntimeHandle = session
	j.RuntimeSpecPayload = payload
	j.RuntimeSpecHash = hex.EncodeToString(digest[:])
	j.Revision++
	j.UpdatedAt = now.UTC()
	if workloadjournal.ValidateUpdate(current, j) != nil {
		return workloadjournal.Snapshot{}, ErrNotReady
	}
	return store.CompareAndSwap(ctx, current, j)
}
