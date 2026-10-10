// Package workloadjournal stores Resource Plane attempt checkpoints without
// connecting to Control Plane DSQL.
package workloadjournal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

var (
	ErrNotFound = errors.New("Workload attempt journal not found")
	ErrConflict = errors.New("Workload attempt journal revision conflict")
	idPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type ExecutionState string

const (
	Accepted  ExecutionState = "accepted"
	Launching ExecutionState = "launching"
	Running   ExecutionState = "running"
	Succeeded ExecutionState = "succeeded"
	Failed    ExecutionState = "failed"
	Cancelled ExecutionState = "cancelled"
	TimedOut  ExecutionState = "timed_out"
)

type CleanupState string

const (
	CleanupNotStarted CleanupState = "not_started"
	CleanupPending    CleanupState = "pending"
	CleanupConfirmed  CleanupState = "confirmed"
	CleanupFailed     CleanupState = "failed"
)

type Journal struct {
	SchemaVersion        int             `json:"schema_version"`
	TenantID             string          `json:"tenant_id"`
	WorkloadRunID        string          `json:"workload_run_id"`
	AttemptID            string          `json:"attempt_id"`
	Generation           int64           `json:"generation"`
	ResourcePlaneID      string          `json:"resource_plane_id"`
	IdempotencyToken     string          `json:"idempotency_token"`
	Revision             int64           `json:"revision"`
	ExecutionState       ExecutionState  `json:"execution_state"`
	CancelRequested      bool            `json:"cancel_requested"`
	RuntimeHandle        string          `json:"runtime_handle,omitempty"`
	SpecificationHash    string          `json:"specification_hash,omitempty"`
	SpecificationPayload json.RawMessage `json:"specification_payload,omitempty"`
	ResultPayload        json.RawMessage `json:"result_payload,omitempty"`
	ResultPublished      bool            `json:"result_published"`
	CleanupState         CleanupState    `json:"cleanup_state"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

type Snapshot struct {
	Journal Journal
	ETag    string
}

type Store interface {
	Create(context.Context, Journal) (Snapshot, error)
	Get(context.Context, string, string, string) (Snapshot, error)
	CompareAndSwap(context.Context, Snapshot, Journal) (Snapshot, error)
}

func (j Journal) Validate() error {
	if j.SchemaVersion != 1 || !idPattern.MatchString(j.TenantID) || !idPattern.MatchString(j.WorkloadRunID) ||
		!idPattern.MatchString(j.AttemptID) || !idPattern.MatchString(j.ResourcePlaneID) ||
		!idPattern.MatchString(j.IdempotencyToken) || j.Generation < 1 || j.Revision < 1 || j.CreatedAt.IsZero() ||
		j.UpdatedAt.IsZero() || j.UpdatedAt.Before(j.CreatedAt) {
		return errors.New("Workload attempt journal identity or revision is invalid")
	}
	if !validExecutionState(j.ExecutionState) || !validCleanupState(j.CleanupState) {
		return errors.New("Workload attempt journal state is invalid")
	}
	if j.RuntimeHandle != "" && !idPattern.MatchString(j.RuntimeHandle) {
		return errors.New("Workload attempt journal runtime handle is invalid")
	}
	if j.SpecificationHash != "" && !hashPattern.MatchString(j.SpecificationHash) {
		return errors.New("Workload attempt specification hash is invalid")
	}
	if len(j.SpecificationPayload) > 0 {
		digest := sha256.Sum256(j.SpecificationPayload)
		if len(j.SpecificationPayload) > 16<<10 || !json.Valid(j.SpecificationPayload) || hex.EncodeToString(digest[:]) != j.SpecificationHash {
			return errors.New("Workload frozen specification does not match its hash")
		}
	}
	if terminal(j.ExecutionState) && j.CleanupState == CleanupNotStarted {
		return errors.New("terminal Workload attempt must have a cleanup state")
	}
	if !terminal(j.ExecutionState) && j.CleanupState != CleanupNotStarted {
		return errors.New("nonterminal Workload attempt cannot have a cleanup state")
	}
	if j.ResultPublished && len(j.ResultPayload) == 0 {
		return errors.New("published Workload result must be durable")
	}
	if len(j.ResultPayload) > 0 {
		var result struct {
			SchemaVersion   int            `json:"schema_version"`
			TenantID        string         `json:"tenant_id"`
			WorkloadRunID   string         `json:"workload_run_id"`
			AttemptID       string         `json:"attempt_id"`
			Generation      int64          `json:"generation"`
			ResourcePlaneID string         `json:"resource_plane_id"`
			Status          ExecutionState `json:"status"`
		}
		if len(j.ResultPayload) > 32<<10 || !terminal(j.ExecutionState) || json.Unmarshal(j.ResultPayload, &result) != nil ||
			result.SchemaVersion != 1 || result.TenantID != j.TenantID || result.WorkloadRunID != j.WorkloadRunID ||
			result.AttemptID != j.AttemptID || result.Generation != j.Generation || result.ResourcePlaneID != j.ResourcePlaneID || result.Status != j.ExecutionState {
			return errors.New("Workload journal result does not match its attempt")
		}
	}
	return nil
}

func New(command Command, now time.Time) (Journal, error) {
	j := Journal{SchemaVersion: 1, TenantID: command.TenantID, WorkloadRunID: command.WorkloadRunID,
		AttemptID: command.AttemptID, Generation: command.Generation, ResourcePlaneID: command.ResourcePlaneID,
		IdempotencyToken: command.IdempotencyToken, Revision: 1, ExecutionState: Accepted,
		CleanupState: CleanupNotStarted, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
	if err := j.Validate(); err != nil {
		return Journal{}, err
	}
	return j, nil
}

// Command is intentionally limited to immutable dispatch identity. Runtime
// configuration, source, and secrets must be resolved independently.
type Command struct {
	TenantID         string
	WorkloadRunID    string
	AttemptID        string
	Generation       int64
	ResourcePlaneID  string
	IdempotencyToken string
}

func ValidateUpdate(current Snapshot, next Journal) error {
	if current.ETag == "" || current.Journal.Validate() != nil || next.Validate() != nil {
		return ErrConflict
	}
	old := current.Journal
	if old.TenantID != next.TenantID || old.WorkloadRunID != next.WorkloadRunID || old.AttemptID != next.AttemptID ||
		old.Generation != next.Generation || old.ResourcePlaneID != next.ResourcePlaneID || old.IdempotencyToken != next.IdempotencyToken ||
		old.CreatedAt != next.CreatedAt || next.Revision != old.Revision+1 || next.UpdatedAt.Before(old.UpdatedAt) {
		return ErrConflict
	}
	if !executionTransitionAllowed(old.ExecutionState, next.ExecutionState) ||
		(old.CancelRequested && !next.CancelRequested) || !cleanupTransitionAllowed(old.CleanupState, next.CleanupState) {
		return ErrConflict
	}
	if next.CancelRequested && old.ExecutionState != next.ExecutionState &&
		(next.ExecutionState == Launching || next.ExecutionState == Running) {
		return ErrConflict
	}
	if old.RuntimeHandle != "" && old.RuntimeHandle != next.RuntimeHandle {
		return ErrConflict
	}
	if old.SpecificationHash != "" && old.SpecificationHash != next.SpecificationHash {
		return ErrConflict
	}
	if len(old.SpecificationPayload) > 0 && !bytes.Equal(old.SpecificationPayload, next.SpecificationPayload) {
		return ErrConflict
	}
	if (old.ResultPublished && !next.ResultPublished) || (len(old.ResultPayload) > 0 && !bytes.Equal(old.ResultPayload, next.ResultPayload)) {
		return ErrConflict
	}
	return nil
}

func Encode(j Journal) ([]byte, error) {
	if err := j.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(j)
	if err != nil {
		return nil, fmt.Errorf("encode Workload attempt journal: %w", err)
	}
	return data, nil
}

func Decode(data []byte) (Journal, error) {
	var j Journal
	if len(data) == 0 || len(data) > 64<<10 {
		return Journal{}, errors.New("Workload attempt journal must be 1-64 KiB")
	}
	if err := json.Unmarshal(data, &j); err != nil {
		return Journal{}, fmt.Errorf("decode Workload attempt journal: %w", err)
	}
	if err := j.Validate(); err != nil {
		return Journal{}, err
	}
	return j, nil
}

func executionTransitionAllowed(from, to ExecutionState) bool {
	if from == to {
		return true
	}
	switch from {
	case Accepted:
		return to == Launching || to == Cancelled || to == TimedOut
	case Launching:
		return to == Running || to == Cancelled || to == Failed || to == TimedOut
	case Running:
		return to == Succeeded || to == Failed || to == Cancelled || to == TimedOut
	default:
		return false
	}
}

func cleanupTransitionAllowed(from, to CleanupState) bool {
	if from == to {
		return true
	}
	switch from {
	case CleanupNotStarted:
		return to == CleanupPending
	case CleanupPending:
		return to == CleanupConfirmed || to == CleanupFailed
	case CleanupFailed:
		return to == CleanupPending || to == CleanupConfirmed
	default:
		return false
	}
}

func validExecutionState(state ExecutionState) bool {
	switch state {
	case Accepted, Launching, Running, Succeeded, Failed, Cancelled, TimedOut:
		return true
	default:
		return false
	}
}

func validCleanupState(state CleanupState) bool {
	switch state {
	case CleanupNotStarted, CleanupPending, CleanupConfirmed, CleanupFailed:
		return true
	default:
		return false
	}
}

func terminal(state ExecutionState) bool {
	return state == Succeeded || state == Failed || state == Cancelled || state == TimedOut
}
