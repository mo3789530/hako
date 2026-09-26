// Package workspaces contains transactional Workspace lifecycle operations.
package workspaces

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idempotency"
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/scheduler"
	"github.com/mo3789530/hako/internal/store/audit"
	"github.com/mo3789530/hako/internal/store/operations"
	"github.com/mo3789530/hako/internal/store/resourcecapacity"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var errConcurrentCreate = errors.New("another request created this idempotent operation concurrently")

var ErrWorkspaceNameConflict = errors.New("workspace name is already in use in this tenant")

var ErrWorkspaceQuotaExceeded = errors.New("tenant workspace quota exceeded")

const maxCreateRaceRetries = 5

const MaxWorkspacesPerTenant = 10

type CreateInput struct {
	TenantID             domain.TenantID
	OwnerID              domain.UserID
	Name                 string
	RuntimeClass         string
	Image                string
	Region               string
	RequiredCapabilities []string
	// ResourcePlaneID optionally pins placement for internal/bootstrap callers.
	// API requests leave it empty so the scheduler chooses an eligible plane.
	ResourcePlaneID domain.ResourcePlaneID
	IdempotencyKey  string
}

type CreateResult struct {
	Workspace domain.Workspace
	Status    domain.WorkspaceStatus
	Placement domain.Placement
	Operation domain.Operation
}

type createRequest struct {
	OwnerID      domain.UserID `json:"owner_id"`
	Name         string        `json:"name"`
	RuntimeClass string        `json:"runtime_class"`
	Image        string        `json:"image"`
}

type operationCommand struct {
	SchemaVersion     int                    `json:"schema_version"`
	WorkspaceRevision int64                  `json:"workspace_revision,omitempty"`
	OperationID       domain.OperationID     `json:"operation_id"`
	TenantID          domain.TenantID        `json:"tenant_id"`
	WorkspaceID       domain.WorkspaceID     `json:"workspace_id"`
	ResourcePlaneID   domain.ResourcePlaneID `json:"resource_plane_id"`
	Type              domain.OperationType   `json:"type"`
	CreatedAt         time.Time              `json:"created_at"`
}

// Create inserts a Workspace, desired/observed state, placement, initial
// ensure_running Operation, event history, and outbox command atomically.
// Placement selection and all database writes share the same transaction.
// The function performs no AWS or queue calls.
func Create(ctx context.Context, pool transaction.Beginner, input CreateInput, policy transaction.Policy) (CreateResult, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.RuntimeClass = strings.TrimSpace(input.RuntimeClass)
	input.Image = strings.TrimSpace(input.Image)
	if input.TenantID == "" || input.OwnerID == "" {
		return CreateResult{}, errors.New("tenant and owner ids are required")
	}
	if input.Name == "" || input.RuntimeClass == "" || input.Image == "" {
		return CreateResult{}, errors.New("workspace name, runtime class, and image are required")
	}
	if err := idempotency.ValidateKey(input.IdempotencyKey); err != nil {
		return CreateResult{}, err
	}

	request := createRequest{OwnerID: input.OwnerID, Name: input.Name, RuntimeClass: input.RuntimeClass, Image: input.Image}
	workspaceID, err := idgen.New("ws_")
	if err != nil {
		return CreateResult{}, err
	}
	operationID, err := idgen.New("op_")
	if err != nil {
		return CreateResult{}, err
	}
	eventID, err := idgen.New("evt_")
	if err != nil {
		return CreateResult{}, err
	}

	createdAt := time.Now().UTC()
	workspace := domain.Workspace{
		ID:           domain.WorkspaceID(workspaceID),
		TenantID:     input.TenantID,
		OwnerID:      input.OwnerID,
		Name:         request.Name,
		RuntimeClass: request.RuntimeClass,
		Image:        request.Image,
		CreatedAt:    createdAt,
		UpdatedAt:    createdAt,
	}
	status := domain.WorkspaceStatus{
		WorkspaceID:   workspace.ID,
		DesiredState:  domain.DesiredWorkspaceRunning,
		ObservedState: domain.ObservedWorkspacePending,
		UpdatedAt:     createdAt,
	}
	placement := domain.Placement{
		WorkspaceID: workspace.ID,
		PlacedAt:    createdAt,
	}
	operation := domain.Operation{
		ID:             domain.OperationID(operationID),
		TenantID:       input.TenantID,
		WorkspaceID:    workspace.ID,
		Type:           domain.OperationEnsureRunning,
		Status:         domain.OperationPending,
		IdempotencyKey: input.IdempotencyKey,
		CreatedAt:      createdAt,
		UpdatedAt:      createdAt,
	}
	for raceAttempt := 0; raceAttempt <= maxCreateRaceRetries; raceAttempt++ {
		result, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (CreateResult, error) {
			existing, found, err := operations.GetByIdempotencyKey(ctx, tx, input.TenantID, input.IdempotencyKey)
			if err != nil {
				return CreateResult{}, err
			}
			if found {
				if err := operations.VerifyRequest(existing, domain.OperationEnsureRunning, request); err != nil {
					return CreateResult{}, err
				}
				return loadCreateResult(ctx, tx, existing)
			}
			placementPolicy := scheduler.Policy{
				TenantID:        input.TenantID,
				ResourcePlaneID: input.ResourcePlaneID,
				RuntimeClass:    input.RuntimeClass,
				Region:          input.Region, RequiredCapabilities: input.RequiredCapabilities,
			}
			selectedResourcePlaneID, err := scheduler.Select(ctx, tx, placementPolicy)
			if err != nil {
				return CreateResult{}, err
			}
			selectedPlacement := placement
			selectedPlacement.ResourcePlaneID = selectedResourcePlaneID
			selectedOperation := operation
			selectedOperation.ResourcePlaneID = selectedResourcePlaneID
			command, err := json.Marshal(operationCommand{
				SchemaVersion: 2, WorkspaceRevision: 1, OperationID: selectedOperation.ID, TenantID: selectedOperation.TenantID,
				WorkspaceID: selectedOperation.WorkspaceID, ResourcePlaneID: selectedOperation.ResourcePlaneID,
				Type: selectedOperation.Type, CreatedAt: selectedOperation.CreatedAt,
			})
			if err != nil {
				return CreateResult{}, fmt.Errorf("encode initial operation command: %w", err)
			}
			if err := prepareQuotaSlots(ctx, tx, input.TenantID); err != nil {
				return CreateResult{}, err
			}

			var insertedWorkspaceID domain.WorkspaceID
			err = tx.QueryRow(ctx, `INSERT INTO workspaces (id, tenant_id, owner_id, name, runtime_class, image, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (tenant_id, name) DO NOTHING RETURNING id`,
				workspace.ID, workspace.TenantID, workspace.OwnerID, workspace.Name, workspace.RuntimeClass, workspace.Image, workspace.CreatedAt, workspace.UpdatedAt,
			).Scan(&insertedWorkspaceID)
			if errors.Is(err, pgx.ErrNoRows) {
				existing, found, lookupErr := operations.GetByIdempotencyKey(ctx, tx, input.TenantID, input.IdempotencyKey)
				if lookupErr != nil {
					return CreateResult{}, lookupErr
				}
				if found {
					if err := operations.VerifyRequest(existing, domain.OperationEnsureRunning, request); err != nil {
						return CreateResult{}, err
					}
					return loadCreateResult(ctx, tx, existing)
				}
				var conflictingWorkspaceID domain.WorkspaceID
				lookupErr = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE tenant_id = $1 AND name = $2`, input.TenantID, workspace.Name).Scan(&conflictingWorkspaceID)
				if lookupErr == nil {
					return CreateResult{}, ErrWorkspaceNameConflict
				}
				if !errors.Is(lookupErr, pgx.ErrNoRows) {
					return CreateResult{}, fmt.Errorf("check conflicting Workspace name: %w", lookupErr)
				}
				return CreateResult{}, errConcurrentCreate
			}
			if err != nil {
				return CreateResult{}, fmt.Errorf("insert workspace: %w", err)
			}
			if insertedWorkspaceID != workspace.ID {
				return CreateResult{}, errors.New("database returned an unexpected Workspace ID")
			}
			if err := reserveQuotaSlot(ctx, tx, input.TenantID, workspace.ID); err != nil {
				return CreateResult{}, err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_status (workspace_id, desired_state, observed_state, updated_at, reconcile_revision) VALUES ($1, $2, $3, $4, 1)`,
				status.WorkspaceID, status.DesiredState, status.ObservedState, status.UpdatedAt,
			); err != nil {
				return CreateResult{}, fmt.Errorf("insert workspace status: %w", err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO placements (workspace_id, resource_plane_id, placed_at) VALUES ($1, $2, $3)`,
				selectedPlacement.WorkspaceID, selectedPlacement.ResourcePlaneID, selectedPlacement.PlacedAt,
			); err != nil {
				return CreateResult{}, fmt.Errorf("insert workspace placement: %w", err)
			}
			if err := resourcecapacity.RecordWorkspace(ctx, tx, workspace.ID, selectedPlacement.ResourcePlaneID, workspace.RuntimeClass, createdAt); err != nil {
				return CreateResult{}, err
			}

			storedOperation, err := operations.InsertOrGet(ctx, tx, selectedOperation, request)
			if err != nil {
				return CreateResult{}, err
			}
			if storedOperation.ID != selectedOperation.ID {
				return CreateResult{}, errConcurrentCreate
			}

			if _, err := tx.Exec(ctx, `INSERT INTO operation_events (operation_id, sequence, event_type, payload_json, created_at) VALUES ($1, $2, $3, $4, $5)`,
				selectedOperation.ID, int64(1), "operation.created", string(command), createdAt,
			); err != nil {
				return CreateResult{}, fmt.Errorf("insert initial operation event: %w", err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload_json, created_at, published_at, attempt) VALUES ($1, $2, $3, $4, $5, $6, NULL, $7)`,
				eventID, "operation", selectedOperation.ID, "operation.requested", string(command), createdAt, 0,
			); err != nil {
				return CreateResult{}, fmt.Errorf("insert operation outbox event: %w", err)
			}
			auditDetails, err := json.Marshal(map[string]any{"operation_id": selectedOperation.ID, "operation_type": selectedOperation.Type})
			if err != nil {
				return CreateResult{}, fmt.Errorf("encode Workspace audit details: %w", err)
			}
			if err := audit.Append(ctx, tx, audit.Event{
				TenantID: input.TenantID, ActorID: input.OwnerID, Action: "workspace.create",
				TargetType: "workspace", TargetID: string(workspace.ID), Details: auditDetails, OccurredAt: createdAt,
			}); err != nil {
				return CreateResult{}, err
			}

			return CreateResult{Workspace: workspace, Status: status, Placement: selectedPlacement, Operation: storedOperation}, nil
		})
		if !errors.Is(err, errConcurrentCreate) {
			return result, err
		}
		if raceAttempt == maxCreateRaceRetries {
			return CreateResult{}, fmt.Errorf("resolve concurrent idempotent Workspace create: %w", err)
		}
		if err := waitForWinner(ctx, raceAttempt); err != nil {
			return CreateResult{}, err
		}
	}
	return CreateResult{}, errors.New("unreachable Workspace create retry state")
}

// reserveQuotaSlot atomically claims one of a Tenant's fixed quota slots.
// The unique (tenant_id, slot) key serializes competing creates without relying
// on SELECT FOR UPDATE, which is not available in Aurora DSQL.
func reserveQuotaSlot(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, workspaceID domain.WorkspaceID) error {
	var existing int16
	err := tx.QueryRow(ctx, `SELECT slot FROM workspace_quota_slots WHERE workspace_id = $1`, workspaceID).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check existing Tenant Workspace quota slot: %w", err)
	}
	for slot := int16(1); slot <= MaxWorkspacesPerTenant; slot++ {
		var claimed int16
		err := tx.QueryRow(ctx, `INSERT INTO workspace_quota_slots (tenant_id, slot, workspace_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING RETURNING slot`, tenantID, slot, workspaceID).Scan(&claimed)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("reserve Tenant Workspace quota slot: %w", err)
		}
		// A concurrent backfill may already have reserved this Workspace using
		// another request's transaction. Treat that as success, not exhaustion.
		err = tx.QueryRow(ctx, `SELECT slot FROM workspace_quota_slots WHERE workspace_id = $1`, workspaceID).Scan(&existing)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check concurrently reserved quota slot: %w", err)
		}
	}
	return ErrWorkspaceQuotaExceeded
}

// prepareQuotaSlots backfills claims for active Workspaces created before the
// quota migration. It commits as part of a successful create transaction, and
// the active Workspace count prevents legacy tenants from exceeding the limit.
func prepareQuotaSlots(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID) error {
	var activeCount int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM workspaces w LEFT JOIN workspace_status s ON s.workspace_id = w.id WHERE w.tenant_id = $1 AND COALESCE(s.observed_state, 'pending') <> 'deleted'`, tenantID).Scan(&activeCount); err != nil {
		return fmt.Errorf("count active Tenant Workspaces: %w", err)
	}
	if activeCount >= MaxWorkspacesPerTenant {
		return ErrWorkspaceQuotaExceeded
	}
	staleRows, err := tx.Query(ctx, `SELECT q.workspace_id FROM workspace_quota_slots q JOIN workspace_status s ON s.workspace_id = q.workspace_id WHERE q.tenant_id = $1 AND s.observed_state = 'deleted'`, tenantID)
	if err != nil {
		return fmt.Errorf("find stale Tenant Workspace quota slots: %w", err)
	}
	var staleIDs []domain.WorkspaceID
	for staleRows.Next() {
		var id domain.WorkspaceID
		if err := staleRows.Scan(&id); err != nil {
			staleRows.Close()
			return fmt.Errorf("scan stale Workspace quota slot: %w", err)
		}
		staleIDs = append(staleIDs, id)
	}
	if err := staleRows.Err(); err != nil {
		staleRows.Close()
		return fmt.Errorf("iterate stale Workspace quota slots: %w", err)
	}
	staleRows.Close()
	for _, id := range staleIDs {
		if _, err := tx.Exec(ctx, `DELETE FROM workspace_quota_slots WHERE workspace_id = $1`, id); err != nil {
			return fmt.Errorf("release stale Workspace quota slot: %w", err)
		}
	}
	rows, err := tx.Query(ctx, `SELECT w.id FROM workspaces w LEFT JOIN workspace_status s ON s.workspace_id = w.id LEFT JOIN workspace_quota_slots q ON q.workspace_id = w.id WHERE w.tenant_id = $1 AND COALESCE(s.observed_state, 'pending') <> 'deleted' AND q.workspace_id IS NULL ORDER BY w.created_at, w.id`, tenantID)
	if err != nil {
		return fmt.Errorf("find legacy Tenant Workspaces without quota slots: %w", err)
	}
	var legacyIDs []domain.WorkspaceID
	for rows.Next() {
		var id domain.WorkspaceID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy Workspace quota candidate: %w", err)
		}
		legacyIDs = append(legacyIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate legacy Workspace quota candidates: %w", err)
	}
	rows.Close()
	for _, id := range legacyIDs {
		if err := reserveQuotaSlot(ctx, tx, tenantID, id); err != nil {
			return err
		}
	}
	return nil
}

func loadCreateResult(ctx context.Context, tx pgx.Tx, operation domain.Operation) (CreateResult, error) {
	var result CreateResult
	result.Operation = operation
	err := tx.QueryRow(ctx, `SELECT id, tenant_id, owner_id, name, runtime_class, image, created_at, updated_at FROM workspaces WHERE id = $1`, operation.WorkspaceID).Scan(
		&result.Workspace.ID, &result.Workspace.TenantID, &result.Workspace.OwnerID, &result.Workspace.Name,
		&result.Workspace.RuntimeClass, &result.Workspace.Image, &result.Workspace.CreatedAt, &result.Workspace.UpdatedAt,
	)
	if err != nil {
		return CreateResult{}, fmt.Errorf("load idempotent Workspace: %w", err)
	}
	err = tx.QueryRow(ctx, `SELECT workspace_id, desired_state, observed_state, updated_at FROM workspace_status WHERE workspace_id = $1`, operation.WorkspaceID).Scan(
		&result.Status.WorkspaceID, &result.Status.DesiredState, &result.Status.ObservedState, &result.Status.UpdatedAt,
	)
	if err != nil {
		return CreateResult{}, fmt.Errorf("load idempotent Workspace status: %w", err)
	}
	err = tx.QueryRow(ctx, `SELECT workspace_id, resource_plane_id, placed_at FROM placements WHERE workspace_id = $1`, operation.WorkspaceID).Scan(
		&result.Placement.WorkspaceID, &result.Placement.ResourcePlaneID, &result.Placement.PlacedAt,
	)
	if err != nil {
		return CreateResult{}, fmt.Errorf("load idempotent Workspace placement: %w", err)
	}
	return result, nil
}

func waitForWinner(ctx context.Context, attempt int) error {
	delay := time.Duration(1<<attempt) * 20 * time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
