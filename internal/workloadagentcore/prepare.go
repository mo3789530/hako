package workloadagentcore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
		j.SpecificationHash != "" || len(j.SpecificationPayload) != 0 || now.Before(j.UpdatedAt) {
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
	j.SpecificationPayload = payload
	j.SpecificationHash = hex.EncodeToString(digest[:])
	j.Revision++
	j.UpdatedAt = now.UTC()
	if workloadjournal.ValidateUpdate(current, j) != nil {
		return workloadjournal.Snapshot{}, ErrNotReady
	}
	return store.CompareAndSwap(ctx, current, j)
}
