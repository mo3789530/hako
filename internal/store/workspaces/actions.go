package workspaces

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/authz"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idempotency"
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/store/audit"
	"github.com/mo3789530/hako/internal/store/operations"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var ErrWorkspaceActionConflict = errors.New("Workspace action conflicts with its current state")
var ErrWorkspaceOperationInProgress = errors.New("Workspace already has an operation in progress")

type ActionInput struct {
	TenantID       domain.TenantID
	WorkspaceID    domain.WorkspaceID
	UserID         domain.UserID
	Action         domain.OperationType
	IdempotencyKey string
}

type ActionResult struct {
	Workspace View
	Operation domain.Operation
}

type actionRequest struct {
	WorkspaceID domain.WorkspaceID `json:"workspace_id"`
	UserID      domain.UserID      `json:"user_id"`
}

type actionCommand struct {
	SchemaVersion   int                    `json:"schema_version"`
	OperationID     domain.OperationID     `json:"operation_id"`
	TenantID        domain.TenantID        `json:"tenant_id"`
	WorkspaceID     domain.WorkspaceID     `json:"workspace_id"`
	ResourcePlaneID domain.ResourcePlaneID `json:"resource_plane_id"`
	Type            domain.OperationType   `json:"type"`
	CreatedAt       time.Time              `json:"created_at"`
}

// RequestAction changes Desired State and records its Operation, event, and
// outbox command atomically. Membership and Workspace visibility are checked
// in the same transaction as the mutation.
func RequestAction(ctx context.Context, pool transaction.Beginner, input ActionInput, policy transaction.Policy) (ActionResult, error) {
	if input.TenantID == "" || input.WorkspaceID == "" || input.UserID == "" {
		return ActionResult{}, errors.New("tenant, Workspace, and User IDs are required")
	}
	if input.Action != domain.OperationSuspend && input.Action != domain.OperationResume && input.Action != domain.OperationDelete {
		return ActionResult{}, errors.New("unsupported Workspace action")
	}
	if err := idempotency.ValidateKey(input.IdempotencyKey); err != nil {
		return ActionResult{}, err
	}
	request := actionRequest{WorkspaceID: input.WorkspaceID, UserID: input.UserID}
	operationID, err := idgen.New("op_")
	if err != nil {
		return ActionResult{}, err
	}
	eventID, err := idgen.New("evt_")
	if err != nil {
		return ActionResult{}, err
	}
	createdAt := time.Now().UTC()

	return transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (ActionResult, error) {
		membership, err := authz.RequireTenantMembership(ctx, tx, input.UserID, input.TenantID)
		if err != nil {
			return ActionResult{}, err
		}
		view, err := GetVisible(ctx, tx, input.TenantID, input.WorkspaceID, input.UserID, membership.Role)
		if err != nil {
			return ActionResult{}, err
		}
		existing, found, err := operations.GetByIdempotencyKey(ctx, tx, input.TenantID, input.IdempotencyKey)
		if err != nil {
			return ActionResult{}, err
		}
		if found {
			if err := operations.VerifyRequest(existing, input.Action, request); err != nil {
				return ActionResult{}, err
			}
			return ActionResult{Workspace: view, Operation: existing}, nil
		}
		if view.Workspace.ID == "" || view.Status.ObservedState == domain.ObservedWorkspaceDeleted {
			return ActionResult{}, ErrWorkspaceActionConflict
		}
		var target domain.DesiredWorkspaceState
		switch input.Action {
		case domain.OperationSuspend:
			if view.Status.DesiredState != domain.DesiredWorkspaceRunning {
				return ActionResult{}, ErrWorkspaceActionConflict
			}
			target = domain.DesiredWorkspaceSuspended
		case domain.OperationResume:
			if view.Status.DesiredState != domain.DesiredWorkspaceSuspended {
				return ActionResult{}, ErrWorkspaceActionConflict
			}
			target = domain.DesiredWorkspaceRunning
		case domain.OperationDelete:
			if view.Status.DesiredState == domain.DesiredWorkspaceDeleted {
				return ActionResult{}, ErrWorkspaceActionConflict
			}
			target = domain.DesiredWorkspaceDeleted
		}
		var inProgress bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM operations WHERE workspace_id = $1 AND status IN ('pending', 'running'))`, input.WorkspaceID).Scan(&inProgress); err != nil {
			return ActionResult{}, fmt.Errorf("check active Workspace Operation: %w", err)
		}
		if inProgress {
			return ActionResult{}, ErrWorkspaceOperationInProgress
		}
		var resourcePlaneID domain.ResourcePlaneID
		if err := tx.QueryRow(ctx, `SELECT resource_plane_id FROM placements WHERE workspace_id = $1`, input.WorkspaceID).Scan(&resourcePlaneID); err != nil {
			return ActionResult{}, fmt.Errorf("load Workspace Placement: %w", err)
		}
		tag, err := tx.Exec(ctx, `UPDATE workspace_status SET desired_state = $1, updated_at = $2 WHERE workspace_id = $3 AND desired_state = $4`, target, createdAt, input.WorkspaceID, view.Status.DesiredState)
		if err != nil {
			return ActionResult{}, fmt.Errorf("update Workspace Desired State: %w", err)
		}
		if tag.RowsAffected() != 1 {
			// A concurrent retry with the same key may have committed while this
			// transaction waited for the Desired State row. Return its Operation
			// instead of surfacing a spurious state conflict.
			existing, found, lookupErr := operations.GetByIdempotencyKey(ctx, tx, input.TenantID, input.IdempotencyKey)
			if lookupErr != nil {
				return ActionResult{}, lookupErr
			}
			if found {
				if verifyErr := operations.VerifyRequest(existing, input.Action, request); verifyErr != nil {
					return ActionResult{}, verifyErr
				}
				current, getErr := GetVisible(ctx, tx, input.TenantID, input.WorkspaceID, input.UserID, membership.Role)
				if getErr != nil {
					return ActionResult{}, getErr
				}
				return ActionResult{Workspace: current, Operation: existing}, nil
			}
			return ActionResult{}, ErrWorkspaceActionConflict
		}
		operation := domain.Operation{
			ID: domain.OperationID(operationID), TenantID: input.TenantID, WorkspaceID: input.WorkspaceID,
			ResourcePlaneID: resourcePlaneID, Type: input.Action, Status: domain.OperationPending,
			IdempotencyKey: input.IdempotencyKey, CreatedAt: createdAt, UpdatedAt: createdAt,
		}
		stored, err := operations.InsertOrGet(ctx, tx, operation, request)
		if err != nil {
			return ActionResult{}, err
		}
		if stored.ID != operation.ID {
			return ActionResult{}, operations.ErrIdempotencyConflict
		}
		command, err := json.Marshal(actionCommand{
			SchemaVersion: 1, OperationID: operation.ID, TenantID: operation.TenantID,
			WorkspaceID: operation.WorkspaceID, ResourcePlaneID: operation.ResourcePlaneID,
			Type: operation.Type, CreatedAt: operation.CreatedAt,
		})
		if err != nil {
			return ActionResult{}, fmt.Errorf("encode Workspace action command: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO operation_events (operation_id, sequence, event_type, payload_json, created_at) VALUES ($1, 1, $2, $3, $4)`, operation.ID, "operation.requested", string(command), createdAt); err != nil {
			return ActionResult{}, fmt.Errorf("insert Workspace action event: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload_json, created_at, published_at, attempt) VALUES ($1, 'operation', $2, 'operation.requested', $3, $4, NULL, 0)`, eventID, operation.ID, string(command), createdAt); err != nil {
			return ActionResult{}, fmt.Errorf("insert Workspace action outbox event: %w", err)
		}
		auditDetails, err := json.Marshal(map[string]any{"operation_id": operation.ID, "operation_type": operation.Type})
		if err != nil {
			return ActionResult{}, fmt.Errorf("encode Workspace action audit details: %w", err)
		}
		if err := audit.Append(ctx, tx, audit.Event{
			TenantID: input.TenantID, ActorID: input.UserID, Action: "workspace." + string(input.Action),
			TargetType: "workspace", TargetID: string(input.WorkspaceID), Details: auditDetails, OccurredAt: createdAt,
		}); err != nil {
			return ActionResult{}, err
		}
		view.Status.DesiredState = target
		view.Status.UpdatedAt = createdAt
		return ActionResult{Workspace: view, Operation: stored}, nil
	})
}
