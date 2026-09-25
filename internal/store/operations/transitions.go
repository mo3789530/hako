package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/resourcecapacity"
	"github.com/mo3789530/hako/internal/store/transaction"
)

type TransitionInput struct {
	OperationID    domain.OperationID
	ExpectedStatus domain.OperationStatus
	NextStatus     domain.OperationStatus
	EventType      string
	Payload        json.RawMessage
	ErrorCode      string
	ObservedState  *domain.ObservedWorkspaceState
	OccurredAt     time.Time
}

type ResultInput struct {
	OperationID     domain.OperationID
	TenantID        domain.TenantID
	WorkspaceID     domain.WorkspaceID
	ResourcePlaneID domain.ResourcePlaneID
	Type            domain.OperationType
	Status          domain.OperationStatus
	ObservedState   domain.ObservedWorkspaceState
	ErrorCode       string
	Payload         json.RawMessage
	CompletedAt     time.Time
}

// ApplyResult atomically validates a Resource Plane result, transitions the
// Operation to a terminal state, appends audit events, and updates Workspace
// Observed State. A duplicate result for an already-terminal Operation is a
// successful no-op (applied=false).
func ApplyResult(ctx context.Context, pool transaction.Beginner, policy transaction.Policy, input ResultInput) (bool, error) {
	input.ErrorCode = strings.TrimSpace(input.ErrorCode)
	if input.OperationID == "" || input.TenantID == "" || input.WorkspaceID == "" || input.ResourcePlaneID == "" || input.Type == "" {
		return false, errors.New("result operation, tenant, workspace, resource plane, and type are required")
	}
	if input.Status != domain.OperationSucceeded && input.Status != domain.OperationFailed {
		return false, errors.New("Resource Plane result status must be succeeded or failed")
	}
	if input.Status == domain.OperationFailed && input.ObservedState != domain.ObservedWorkspaceFailed {
		return false, errors.New("failed result must report failed observed state")
	}
	if input.Status == domain.OperationSucceeded && expectedObservedState(input.Type) != input.ObservedState {
		return false, errors.New("successful result observed state does not match Operation type")
	}
	if len(input.Payload) > 0 && !json.Valid(input.Payload) {
		return false, errors.New("result payload must be valid JSON")
	}
	if input.CompletedAt.IsZero() {
		return false, errors.New("result completion time is required")
	}

	return transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		operation, err := Get(ctx, tx, input.OperationID)
		if err != nil {
			return false, err
		}
		if operation.TenantID != input.TenantID || operation.WorkspaceID != input.WorkspaceID || operation.ResourcePlaneID != input.ResourcePlaneID || operation.Type != input.Type {
			return false, errors.New("Resource Plane result identity does not match Operation")
		}
		if operation.Status == domain.OperationSucceeded || operation.Status == domain.OperationFailed || operation.Status == domain.OperationCancelled {
			return false, nil
		}
		if operation.Status != domain.OperationPending && operation.Status != domain.OperationRunning {
			return false, fmt.Errorf("cannot apply result to Operation status %q", operation.Status)
		}
		if operation.Status == domain.OperationPending {
			tag, err := tx.Exec(ctx, `UPDATE operations SET status = 'running', attempt = attempt + 1, updated_at = $1 WHERE id = $2 AND status = 'pending'`, input.CompletedAt, input.OperationID)
			if err != nil {
				return false, fmt.Errorf("start Operation from result: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return false, ErrTransitionConflict
			}
			if err := appendResultEvent(ctx, tx, input.OperationID, "operation.started", nil, input.CompletedAt); err != nil {
				return false, err
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE operations SET status = $1, error_code = NULLIF($2, ''), updated_at = $3 WHERE id = $4 AND status = 'running'`, input.Status, input.ErrorCode, input.CompletedAt, input.OperationID)
		if err != nil {
			return false, fmt.Errorf("complete Operation from result: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return false, ErrTransitionConflict
		}
		if err := appendResultEvent(ctx, tx, input.OperationID, "operation."+string(input.Status), input.Payload, input.CompletedAt); err != nil {
			return false, err
		}
		tag, err = tx.Exec(ctx, `UPDATE workspace_status SET observed_state = $1, updated_at = $2 WHERE workspace_id = $3`, input.ObservedState, input.CompletedAt, input.WorkspaceID)
		if err != nil {
			return false, fmt.Errorf("update Workspace observed state from result: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return false, errors.New("Workspace status row is missing")
		}
		if input.ObservedState == domain.ObservedWorkspaceDeleted {
			if _, err := tx.Exec(ctx, `DELETE FROM workspace_quota_slots WHERE workspace_id = $1`, input.WorkspaceID); err != nil {
				return false, fmt.Errorf("release deleted Workspace quota slot: %w", err)
			}
			if err := resourcecapacity.ReleaseWorkspace(ctx, tx, input.WorkspaceID, input.CompletedAt); err != nil {
				return false, err
			}
		}
		return true, nil
	})
}

func expectedObservedState(operationType domain.OperationType) domain.ObservedWorkspaceState {
	switch operationType {
	case domain.OperationEnsureRunning, domain.OperationResume:
		return domain.ObservedWorkspaceRunning
	case domain.OperationSuspend:
		return domain.ObservedWorkspaceSuspended
	case domain.OperationDelete:
		return domain.ObservedWorkspaceDeleted
	default:
		return ""
	}
}

func appendResultEvent(ctx context.Context, tx pgx.Tx, operationID domain.OperationID, eventType string, payload json.RawMessage, occurredAt time.Time) error {
	var sequence int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM operation_events WHERE operation_id = $1`, operationID).Scan(&sequence); err != nil {
		return fmt.Errorf("allocate result event sequence: %w", err)
	}
	var payloadValue any
	if len(payload) > 0 {
		payloadValue = string(payload)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO operation_events (operation_id, sequence, event_type, payload_json, created_at) VALUES ($1, $2, $3, $4, $5)`, operationID, sequence, eventType, payloadValue, occurredAt); err != nil {
		return fmt.Errorf("append result event %s: %w", eventType, err)
	}
	return nil
}

// Transition atomically changes an Operation, appends its next ordered event,
// and optionally updates the Workspace's observed state. ExpectedStatus is a
// compare-and-swap guard so duplicate or stale results cannot silently apply.
func Transition(ctx context.Context, pool transaction.Beginner, policy transaction.Policy, input TransitionInput) (domain.Operation, error) {
	input.EventType = strings.TrimSpace(input.EventType)
	input.ErrorCode = strings.TrimSpace(input.ErrorCode)
	if input.OperationID == "" || input.ExpectedStatus == "" || input.NextStatus == "" || input.EventType == "" {
		return domain.Operation{}, errors.New("operation id, expected status, next status, and event type are required")
	}
	if !validTransition(input.ExpectedStatus, input.NextStatus) {
		return domain.Operation{}, fmt.Errorf("invalid operation status transition %q -> %q", input.ExpectedStatus, input.NextStatus)
	}
	if len(input.Payload) > 0 && !json.Valid(input.Payload) {
		return domain.Operation{}, errors.New("operation event payload must be valid JSON")
	}
	if input.ObservedState != nil && !validObservedState(*input.ObservedState) {
		return domain.Operation{}, fmt.Errorf("invalid Workspace observed state %q", *input.ObservedState)
	}
	if input.OccurredAt.IsZero() {
		input.OccurredAt = time.Now().UTC()
	}

	return transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.Operation, error) {
		operation, err := Get(ctx, tx, input.OperationID)
		if err != nil {
			return domain.Operation{}, err
		}
		if operation.Status != input.ExpectedStatus {
			return domain.Operation{}, ErrTransitionConflict
		}

		var updated domain.Operation
		err = tx.QueryRow(ctx, `UPDATE operations SET status = $1, error_code = NULLIF($2, ''), attempt = attempt + $3, updated_at = $4 WHERE id = $5 AND status = $6 RETURNING id, tenant_id, workspace_id, resource_plane_id, type, status, idempotency_key, request_hash, attempt, COALESCE(error_code, ''), created_at, updated_at`,
			input.NextStatus, input.ErrorCode, attemptIncrement(input.NextStatus), input.OccurredAt, input.OperationID, input.ExpectedStatus,
		).Scan(&updated.ID, &updated.TenantID, &updated.WorkspaceID, &updated.ResourcePlaneID, &updated.Type, &updated.Status,
			&updated.IdempotencyKey, &updated.RequestHash, &updated.Attempt, &updated.ErrorCode, &updated.CreatedAt, &updated.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Operation{}, ErrTransitionConflict
		}
		if err != nil {
			return domain.Operation{}, fmt.Errorf("update operation status: %w", err)
		}

		var sequence int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM operation_events WHERE operation_id = $1`, input.OperationID).Scan(&sequence); err != nil {
			return domain.Operation{}, fmt.Errorf("allocate operation event sequence: %w", err)
		}
		var payload any
		if len(input.Payload) > 0 {
			payload = string(input.Payload)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO operation_events (operation_id, sequence, event_type, payload_json, created_at) VALUES ($1, $2, $3, $4, $5)`,
			input.OperationID, sequence, input.EventType, payload, input.OccurredAt,
		); err != nil {
			return domain.Operation{}, fmt.Errorf("append operation event: %w", err)
		}

		if input.ObservedState != nil {
			tag, err := tx.Exec(ctx, `UPDATE workspace_status SET observed_state = $1, updated_at = $2 WHERE workspace_id = $3`, *input.ObservedState, input.OccurredAt, updated.WorkspaceID)
			if err != nil {
				return domain.Operation{}, fmt.Errorf("update Workspace observed state: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return domain.Operation{}, errors.New("Workspace status row is missing")
			}
			if *input.ObservedState == domain.ObservedWorkspaceDeleted {
				if _, err := tx.Exec(ctx, `DELETE FROM workspace_quota_slots WHERE workspace_id = $1`, updated.WorkspaceID); err != nil {
					return domain.Operation{}, fmt.Errorf("release deleted Workspace quota slot: %w", err)
				}
				if err := resourcecapacity.ReleaseWorkspace(ctx, tx, updated.WorkspaceID, input.OccurredAt); err != nil {
					return domain.Operation{}, err
				}
			}
		}
		return updated, nil
	})
}

// ListEvents returns an operation's immutable event history in sequence order.
func ListEvents(ctx context.Context, tx pgx.Tx, id domain.OperationID) ([]domain.OperationEvent, error) {
	if tx == nil {
		return nil, errors.New("transaction is required")
	}
	if id == "" {
		return nil, errors.New("operation id is required")
	}
	rows, err := tx.Query(ctx, `SELECT operation_id, sequence, event_type, COALESCE(payload_json, ''), created_at FROM operation_events WHERE operation_id = $1 ORDER BY sequence`, id)
	if err != nil {
		return nil, fmt.Errorf("list operation events: %w", err)
	}
	defer rows.Close()

	events := make([]domain.OperationEvent, 0)
	for rows.Next() {
		var event domain.OperationEvent
		var payload string
		if err := rows.Scan(&event.OperationID, &event.Sequence, &event.Type, &payload, &event.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan operation event: %w", err)
		}
		if payload != "" {
			event.Payload = json.RawMessage(payload)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate operation events: %w", err)
	}
	return events, nil
}

func validTransition(from, to domain.OperationStatus) bool {
	switch from {
	case domain.OperationPending:
		return to == domain.OperationRunning || to == domain.OperationFailed || to == domain.OperationCancelled
	case domain.OperationRunning:
		return to == domain.OperationPending || to == domain.OperationSucceeded || to == domain.OperationFailed || to == domain.OperationCancelled
	default:
		return false
	}
}

func attemptIncrement(status domain.OperationStatus) int {
	if status == domain.OperationRunning {
		return 1
	}
	return 0
}

func validObservedState(state domain.ObservedWorkspaceState) bool {
	switch state {
	case domain.ObservedWorkspacePending, domain.ObservedWorkspaceProvisioning, domain.ObservedWorkspaceRunning,
		domain.ObservedWorkspaceSuspending, domain.ObservedWorkspaceSuspended, domain.ObservedWorkspaceDeleting,
		domain.ObservedWorkspaceDeleted, domain.ObservedWorkspaceFailed:
		return true
	default:
		return false
	}
}
