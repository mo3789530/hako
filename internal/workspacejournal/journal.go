// Package workspacejournal stores durable Resource Plane Workspace allocation
// checkpoints. Production stores must be conditional and must not use DSQL.
package workspacejournal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

var (
	ErrNotFound  = errors.New("Workspace runtime journal not found")
	ErrConflict  = errors.New("Workspace runtime journal conflict")
	ErrStale     = errors.New("stale Workspace revision")
	ErrDuplicate = errors.New("duplicate Workspace Operation")
	idPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	hashPattern  = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type State string

const (
	Provisioning State = "provisioning"
	Running      State = "running"
	Suspending   State = "suspending"
	Suspended    State = "suspended"
	Resuming     State = "resuming"
	Terminating  State = "terminating"
	Terminated   State = "terminated"
	Deleting     State = "deleting"
	Deleted      State = "deleted"
	Failed       State = "failed"
)

type CleanupState string

const (
	CleanupNotRequired CleanupState = "not_required"
	CleanupPending     CleanupState = "pending"
	CleanupFailed      CleanupState = "failed"
	CleanupConfirmed   CleanupState = "confirmed"
)

// Journal records one current MicroVM allocation and the highest accepted
// Workspace command revision. It contains no endpoint token, AWS credential,
// image secret, or other secret material.
type Journal struct {
	SchemaVersion        int                    `json:"schema_version"`
	TenantID             domain.TenantID        `json:"tenant_id"`
	WorkspaceID          domain.WorkspaceID     `json:"workspace_id"`
	ResourcePlaneID      domain.ResourcePlaneID `json:"resource_plane_id"`
	WorkspaceRevision    int64                  `json:"workspace_revision"`
	Revision             int64                  `json:"revision"`
	OperationID          domain.OperationID     `json:"operation_id"`
	OperationType        domain.OperationType   `json:"operation_type"`
	State                State                  `json:"state"`
	Tombstone            bool                   `json:"tombstone"`
	AllocationGeneration int64                  `json:"allocation_generation"`
	ProviderID           string                 `json:"provider_id,omitempty"`
	LaunchToken          string                 `json:"launch_token,omitempty"`
	LaunchSpecSHA256     string                 `json:"launch_spec_sha256,omitempty"`
	CleanupState         CleanupState           `json:"cleanup_state"`
	CreatedAt            time.Time              `json:"created_at"`
	UpdatedAt            time.Time              `json:"updated_at"`
}

type Snapshot struct {
	Journal Journal
	ETag    string
}

type Store interface {
	Create(context.Context, Journal) (Snapshot, error)
	Get(context.Context, domain.TenantID, domain.WorkspaceID, domain.ResourcePlaneID) (Snapshot, error)
	CompareAndSwap(context.Context, Snapshot, Journal) (Snapshot, error)
}

// Command is the immutable identity needed to fence a Workspace Operation.
// Runtime configuration is resolved separately and represented by its digest.
type Command struct {
	TenantID          domain.TenantID
	WorkspaceID       domain.WorkspaceID
	ResourcePlaneID   domain.ResourcePlaneID
	WorkspaceRevision int64
	OperationID       domain.OperationID
	Type              domain.OperationType
}

// New creates the first journal checkpoint before any provider side effect.
// An initial suspend/resume cannot be accepted without an existing allocation.
func New(command Command, launchToken, launchSpecSHA256 string, now time.Time) (Journal, error) {
	j := Journal{
		SchemaVersion: 1, TenantID: command.TenantID, WorkspaceID: command.WorkspaceID,
		ResourcePlaneID: command.ResourcePlaneID, WorkspaceRevision: command.WorkspaceRevision,
		Revision: 1, OperationID: command.OperationID, OperationType: command.Type,
		CleanupState: CleanupNotRequired, CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
	switch command.Type {
	case domain.OperationEnsureRunning:
		j.State, j.AllocationGeneration = Provisioning, 1
		j.LaunchToken, j.LaunchSpecSHA256 = launchToken, launchSpecSHA256
	case domain.OperationDelete:
		if launchToken != "" || launchSpecSHA256 != "" {
			return Journal{}, errors.New("initial delete Operation cannot include launch parameters")
		}
		// A delete for a never-provisioned Workspace still leaves a durable
		// tombstone so a delayed ensure cannot recreate it.
		j.State, j.Tombstone, j.CleanupState = Deleted, true, CleanupConfirmed
	default:
		return Journal{}, errors.New("initial Workspace journal requires ensure_running or delete")
	}
	if err := j.Validate(); err != nil {
		return Journal{}, err
	}
	return j, nil
}

// ApplyCommand validates a command against the latest checkpoint and returns
// the next record. The caller must persist it with CompareAndSwap before any
// external side effect. A duplicate is safe to acknowledge only after loading
// the already-durable matching operation.
func ApplyCommand(current Snapshot, command Command, launchToken, launchSpecSHA256 string, now time.Time) (Journal, error) {
	old := current.Journal
	if current.ETag == "" || old.Validate() != nil || !commandMatchesJournalIdentity(command, old) {
		return Journal{}, ErrConflict
	}
	if command.WorkspaceRevision < old.WorkspaceRevision {
		return Journal{}, ErrStale
	}
	if command.WorkspaceRevision == old.WorkspaceRevision {
		if command.OperationID == old.OperationID && command.Type == old.OperationType {
			if command.Type == domain.OperationEnsureRunning {
				if launchToken != old.LaunchToken || launchSpecSHA256 != old.LaunchSpecSHA256 {
					return Journal{}, ErrConflict
				}
			} else if launchToken != "" || launchSpecSHA256 != "" {
				return Journal{}, ErrConflict
			}
			return old, ErrDuplicate
		}
		return Journal{}, ErrConflict
	}
	if old.Tombstone || old.State == Deleted || old.CleanupState == CleanupPending || old.CleanupState == CleanupFailed {
		return Journal{}, ErrConflict
	}
	if !stable(old.State) {
		return Journal{}, ErrConflict
	}

	next := old
	next.Revision++
	next.WorkspaceRevision = command.WorkspaceRevision
	next.OperationID, next.OperationType = command.OperationID, command.Type
	next.UpdatedAt = now.UTC()
	switch command.Type {
	case domain.OperationEnsureRunning:
		switch old.State {
		case Running:
			if launchToken != "" || launchSpecSHA256 != "" {
				return Journal{}, errors.New("ensure_running for an existing allocation cannot change launch parameters")
			}
			next.State = Running // Reconcile an already-running allocation.
		case Terminated:
			if old.CleanupState != CleanupConfirmed {
				return Journal{}, ErrConflict
			}
			if !validLaunch(launchToken, launchSpecSHA256) {
				return Journal{}, errors.New("replacement launch token and specification digest are required")
			}
			next.State = Provisioning
			next.AllocationGeneration++
			next.ProviderID, next.LaunchToken, next.LaunchSpecSHA256 = "", launchToken, launchSpecSHA256
			next.CleanupState = CleanupNotRequired
		default:
			return Journal{}, ErrConflict
		}
	case domain.OperationSuspend:
		if launchToken != "" || launchSpecSHA256 != "" {
			return Journal{}, errors.New("suspend Operation cannot change launch parameters")
		}
		if old.State != Running || old.ProviderID == "" {
			return Journal{}, ErrConflict
		}
		next.State = Suspending
	case domain.OperationResume:
		if launchToken != "" || launchSpecSHA256 != "" {
			return Journal{}, errors.New("resume Operation cannot change launch parameters")
		}
		if old.State != Suspended || old.ProviderID == "" {
			return Journal{}, ErrConflict
		}
		next.State = Resuming
	case domain.OperationDelete:
		if launchToken != "" || launchSpecSHA256 != "" {
			return Journal{}, errors.New("delete Operation cannot change launch parameters")
		}
		next.Tombstone = true
		if old.AllocationGeneration == 0 {
			next.State, next.CleanupState = Deleted, CleanupConfirmed
		} else {
			next.State, next.CleanupState = Deleting, CleanupPending
		}
	default:
		return Journal{}, errors.New("unsupported Workspace Operation type")
	}
	if err := ValidateUpdate(current, next); err != nil {
		return Journal{}, err
	}
	return next, nil
}

func (j Journal) Validate() error {
	if j.SchemaVersion != 1 || !idPattern.MatchString(string(j.TenantID)) ||
		!idPattern.MatchString(string(j.WorkspaceID)) || !idPattern.MatchString(string(j.ResourcePlaneID)) ||
		j.WorkspaceRevision < 1 || j.Revision < 1 || !idPattern.MatchString(string(j.OperationID)) ||
		!validOperation(j.OperationType) || !validState(j.State) || !validCleanup(j.CleanupState) ||
		j.AllocationGeneration < 0 || j.CreatedAt.IsZero() || j.UpdatedAt.IsZero() || j.UpdatedAt.Before(j.CreatedAt) {
		return errors.New("Workspace runtime journal identity, revision, or state is invalid")
	}
	if !validOperationState(j.OperationType, j.State) {
		return errors.New("Workspace Operation type and journal state are inconsistent")
	}
	if j.ProviderID != "" && (len(j.ProviderID) > 256 || strings.TrimSpace(j.ProviderID) != j.ProviderID || !printable(j.ProviderID)) {
		return errors.New("Workspace runtime provider ID is invalid")
	}
	if j.AllocationGeneration == 0 && !(j.State == Deleted && j.Tombstone) {
		return errors.New("Workspace journal without an allocation must be a deletion tombstone")
	}
	if j.ProviderID != "" && j.AllocationGeneration == 0 {
		return errors.New("Workspace provider ID requires an allocation generation")
	}
	if j.LaunchToken != "" && !idPattern.MatchString(j.LaunchToken) {
		return errors.New("Workspace runtime launch token is invalid")
	}
	if j.LaunchSpecSHA256 != "" && !hashPattern.MatchString(j.LaunchSpecSHA256) {
		return errors.New("Workspace runtime launch specification digest is invalid")
	}
	if j.State == Provisioning || j.AllocationGeneration > 0 {
		if j.AllocationGeneration < 1 || !validLaunch(j.LaunchToken, j.LaunchSpecSHA256) {
			return errors.New("Workspace allocation requires a generation, launch token, and specification digest")
		}
	}
	if (j.State == Running || j.State == Suspending || j.State == Suspended || j.State == Resuming || j.State == Terminating || j.State == Terminated) && j.ProviderID == "" {
		return errors.New("active or terminated Workspace allocation requires a provider ID")
	}
	if j.State == Deleted && (!j.Tombstone || j.CleanupState != CleanupConfirmed) {
		return errors.New("deleted Workspace journal must have a confirmed deletion tombstone")
	}
	if j.Tombstone && j.OperationType != domain.OperationDelete {
		return errors.New("Workspace tombstone must be associated with delete Operation")
	}
	if j.Tombstone && j.State != Deleting && j.State != Deleted && j.State != Failed {
		return errors.New("Workspace deletion tombstone has an invalid lifecycle state")
	}
	if j.Tombstone && j.State == Failed && j.CleanupState != CleanupPending && j.CleanupState != CleanupFailed {
		return errors.New("failed deletion tombstone must retain pending or failed cleanup")
	}
	if j.State == Deleting && (!j.Tombstone || j.CleanupState != CleanupPending) {
		return errors.New("deleting Workspace journal requires a tombstone and pending cleanup")
	}
	if (j.CleanupState == CleanupPending || j.CleanupState == CleanupFailed) && !j.Tombstone {
		return errors.New("Workspace cleanup state requires a deletion tombstone")
	}
	return nil
}

func ValidateUpdate(current Snapshot, next Journal) error {
	old := current.Journal
	if current.ETag == "" || old.Validate() != nil || next.Validate() != nil {
		return ErrConflict
	}
	if old.TenantID != next.TenantID || old.WorkspaceID != next.WorkspaceID ||
		old.ResourcePlaneID != next.ResourcePlaneID || old.CreatedAt != next.CreatedAt ||
		next.Revision != old.Revision+1 || next.WorkspaceRevision < old.WorkspaceRevision ||
		next.UpdatedAt.Before(old.UpdatedAt) || (old.Tombstone && !next.Tombstone) {
		return ErrConflict
	}
	if next.WorkspaceRevision == old.WorkspaceRevision {
		if next.OperationID != old.OperationID || next.OperationType != old.OperationType {
			return ErrConflict
		}
	} else if old.Tombstone || !stable(old.State) {
		return ErrConflict
	}
	if old.Tombstone && (next.OperationID != old.OperationID || next.OperationType != old.OperationType) {
		return ErrConflict
	}
	if old.ProviderID != "" && old.AllocationGeneration == next.AllocationGeneration && old.ProviderID != next.ProviderID {
		return ErrConflict
	}
	if next.AllocationGeneration < old.AllocationGeneration || next.AllocationGeneration > old.AllocationGeneration+1 {
		return ErrConflict
	}
	if next.AllocationGeneration == old.AllocationGeneration+1 {
		if old.State != Terminated || old.CleanupState != CleanupConfirmed || next.OperationType != domain.OperationEnsureRunning ||
			next.State != Provisioning || next.ProviderID != "" || next.LaunchToken == old.LaunchToken || next.LaunchSpecSHA256 == "" {
			return ErrConflict
		}
	} else if old.LaunchToken != next.LaunchToken || old.LaunchSpecSHA256 != next.LaunchSpecSHA256 {
		return ErrConflict
	}
	if old.CleanupState != next.CleanupState {
		if next.AllocationGeneration == old.AllocationGeneration+1 && old.CleanupState == CleanupConfirmed && next.CleanupState == CleanupNotRequired {
			// A new, independently fenced MicroVM allocation starts with a fresh
			// cleanup lifecycle after the prior allocation is confirmed gone.
		} else if !cleanupTransitionAllowed(old.CleanupState, next.CleanupState) {
			return ErrConflict
		}
	}
	if err := transition(old, next); err != nil {
		return err
	}
	return nil
}

func Encode(j Journal) ([]byte, error) {
	if err := j.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(j)
	if err != nil {
		return nil, fmt.Errorf("encode Workspace runtime journal: %w", err)
	}
	return data, nil
}

func Decode(data []byte) (Journal, error) {
	if len(data) == 0 || len(data) > 32<<10 {
		return Journal{}, errors.New("Workspace runtime journal must be 1-32 KiB")
	}
	var journal Journal
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return Journal{}, fmt.Errorf("decode Workspace runtime journal: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Journal{}, errors.New("Workspace runtime journal must contain one JSON object")
	}
	if err := journal.Validate(); err != nil {
		return Journal{}, err
	}
	return journal, nil
}

func commandMatchesJournalIdentity(command Command, journal Journal) bool {
	return command.TenantID == journal.TenantID && command.WorkspaceID == journal.WorkspaceID && command.ResourcePlaneID == journal.ResourcePlaneID
}

func validLaunch(token, digest string) bool {
	return idPattern.MatchString(token) && hashPattern.MatchString(digest)
}

func printable(value string) bool {
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return value != ""
}

func validOperation(operation domain.OperationType) bool {
	switch operation {
	case domain.OperationEnsureRunning, domain.OperationSuspend, domain.OperationResume, domain.OperationDelete:
		return true
	default:
		return false
	}
}

func validOperationState(operation domain.OperationType, state State) bool {
	switch operation {
	case domain.OperationEnsureRunning:
		return state == Provisioning || state == Running || state == Terminating || state == Terminated || state == Failed
	case domain.OperationSuspend:
		return state == Suspending || state == Suspended || state == Failed
	case domain.OperationResume:
		return state == Resuming || state == Running || state == Failed
	case domain.OperationDelete:
		return state == Deleting || state == Deleted || state == Failed
	default:
		return false
	}
}

func validState(state State) bool {
	switch state {
	case Provisioning, Running, Suspending, Suspended, Resuming, Terminating, Terminated, Deleting, Deleted, Failed:
		return true
	default:
		return false
	}
}

func validCleanup(state CleanupState) bool {
	switch state {
	case CleanupNotRequired, CleanupPending, CleanupFailed, CleanupConfirmed:
		return true
	default:
		return false
	}
}

func stable(state State) bool {
	return state == Running || state == Suspended || state == Terminated || state == Failed || state == Deleted
}

func cleanupTransitionAllowed(from, to CleanupState) bool {
	switch from {
	case CleanupNotRequired:
		return to == CleanupPending
	case CleanupPending:
		return to == CleanupFailed || to == CleanupConfirmed
	case CleanupFailed:
		return to == CleanupPending || to == CleanupConfirmed
	default:
		return false
	}
}

func transition(old, next Journal) error {
	if old.State == next.State {
		return nil
	}
	allowed := next.Tombstone && next.OperationType == domain.OperationDelete && next.State == Deleting
	switch old.State {
	case Provisioning:
		allowed = allowed || next.State == Running || next.State == Failed || next.State == Terminating || next.State == Deleting
	case Running:
		allowed = allowed || next.State == Suspending || next.State == Terminating || next.State == Failed || next.State == Deleting
	case Suspending:
		allowed = allowed || next.State == Suspended || next.State == Failed || next.State == Deleting
	case Suspended:
		allowed = allowed || next.State == Resuming || next.State == Terminating || next.State == Failed || next.State == Deleting
	case Resuming:
		allowed = allowed || next.State == Running || next.State == Suspended || next.State == Failed || next.State == Deleting
	case Terminating:
		allowed = allowed || next.State == Terminated || next.State == Failed || next.State == Deleting
	case Terminated:
		allowed = allowed || next.State == Provisioning || next.State == Deleting || next.State == Deleted
	case Deleting:
		allowed = allowed || next.State == Deleted || next.State == Failed
	case Failed:
		allowed = allowed || next.State == Deleting
	}
	if !allowed {
		return ErrConflict
	}
	return nil
}

// DigestLaunchSpec returns the canonical SHA-256 used to pin immutable launch
// parameters in a journal. Callers should pass a canonical serialized struct.
func DigestLaunchSpec(canonical []byte) string {
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}
