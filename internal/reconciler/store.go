package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/store/operations"
	"github.com/mo3789530/hako/internal/store/transaction"
)

type StoreAdapter struct {
	Pool   transaction.Beginner
	Policy transaction.Policy
}

func (s StoreAdapter) FailStaleOperations(ctx context.Context, cutoff, now time.Time, limit int) (int, error) {
	return operations.FailStale(ctx, s.Pool, s.Policy, cutoff, now, limit)
}

type requestedCommand struct {
	SchemaVersion   int                    `json:"schema_version"`
	OperationID     domain.OperationID     `json:"operation_id"`
	TenantID        domain.TenantID        `json:"tenant_id"`
	WorkspaceID     domain.WorkspaceID     `json:"workspace_id"`
	ResourcePlaneID domain.ResourcePlaneID `json:"resource_plane_id"`
	Type            domain.OperationType   `json:"type"`
	CreatedAt       time.Time              `json:"created_at"`
}

type reconcileRequest struct {
	WorkspaceID domain.WorkspaceID           `json:"workspace_id"`
	Desired     domain.DesiredWorkspaceState `json:"desired_state"`
}

func (s StoreAdapter) Candidates(ctx context.Context, limit int, now time.Time, failureDelay time.Duration) ([]Candidate, error) {
	return transaction.Within(ctx, s.Pool, s.Policy, func(ctx context.Context, tx pgx.Tx) ([]Candidate, error) {
		rows, err := tx.Query(ctx, `SELECT w.id, s.desired_state, s.observed_state, s.updated_at
			FROM workspaces w
			JOIN workspace_status s ON s.workspace_id = w.id
			JOIN placements p ON p.workspace_id = w.id
			WHERE s.desired_state <> s.observed_state
			  AND NOT EXISTS (SELECT 1 FROM operations o WHERE o.workspace_id = w.id AND o.status IN ('pending', 'running'))
			  AND NOT EXISTS (SELECT 1 FROM operations o WHERE o.workspace_id = w.id AND o.status IN ('succeeded', 'failed', 'cancelled') AND o.updated_at > $1)
			ORDER BY s.updated_at, w.id LIMIT $2`, now.Add(-failureDelay), limit)
		if err != nil {
			return nil, fmt.Errorf("query mismatched Workspaces: %w", err)
		}
		defer rows.Close()
		candidates := make([]Candidate, 0)
		for rows.Next() {
			var candidate Candidate
			if err := rows.Scan(&candidate.WorkspaceID, &candidate.Desired, &candidate.Observed, &candidate.UpdatedAt); err != nil {
				return nil, fmt.Errorf("scan Workspace reconciliation candidate: %w", err)
			}
			candidates = append(candidates, candidate)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate Workspace reconciliation candidates: %w", err)
		}
		return candidates, nil
	})
}

// EnsureOperation re-reads all state and atomically claims the observed status
// revision before inserting an Operation, event, and Outbox command. Concurrent
// Reconciler instances therefore cannot both act on the same snapshot.
func (s StoreAdapter) EnsureOperation(ctx context.Context, candidate Candidate, action domain.OperationType, now time.Time, failureDelay time.Duration) (bool, error) {
	operationID, err := idgen.New("op_")
	if err != nil {
		return false, err
	}
	eventID, err := idgen.New("evt_")
	if err != nil {
		return false, err
	}
	idempotencyKey, err := idgen.New("reconcile_")
	if err != nil {
		return false, err
	}
	return transaction.Within(ctx, s.Pool, s.Policy, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		var tenantID domain.TenantID
		var workspaceID domain.WorkspaceID
		var resourcePlaneID domain.ResourcePlaneID
		var desired domain.DesiredWorkspaceState
		var observed domain.ObservedWorkspaceState
		var updatedAt time.Time
		var revision int64
		err := tx.QueryRow(ctx, `SELECT w.tenant_id, w.id, p.resource_plane_id, s.desired_state, s.observed_state, s.updated_at, COALESCE(s.reconcile_revision, 0)
			FROM workspaces w JOIN placements p ON p.workspace_id = w.id JOIN workspace_status s ON s.workspace_id = w.id
			WHERE w.id = $1`, candidate.WorkspaceID).Scan(&tenantID, &workspaceID, &resourcePlaneID, &desired, &observed, &updatedAt, &revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("reload Workspace reconciliation state: %w", err)
		}
		currentAction, needed := operationFor(desired, observed)
		if !needed || currentAction != action {
			return false, nil
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM operations WHERE workspace_id = $1 AND status IN ('pending', 'running'))`, workspaceID).Scan(&active); err != nil {
			return false, fmt.Errorf("check active Workspace Operation: %w", err)
		}
		if active {
			return false, nil
		}
		var recentlyTerminal bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM operations WHERE workspace_id = $1 AND status IN ('succeeded', 'failed', 'cancelled') AND updated_at > $2)`, workspaceID, now.Add(-failureDelay)).Scan(&recentlyTerminal); err != nil {
			return false, fmt.Errorf("check recent terminal Workspace Operation: %w", err)
		}
		if recentlyTerminal {
			return false, nil
		}

		// The monotonic revision ensures two Reconciler transactions cannot
		// both claim the same state snapshot even when their clocks are equal.
		tag, err := tx.Exec(ctx, `UPDATE workspace_status SET updated_at = $1, reconcile_revision = COALESCE(reconcile_revision, 0) + 1 WHERE workspace_id = $2 AND desired_state = $3 AND observed_state = $4 AND updated_at = $5 AND COALESCE(reconcile_revision, 0) = $6`, now, workspaceID, desired, observed, updatedAt, revision)
		if err != nil {
			return false, fmt.Errorf("claim Workspace reconciliation revision: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return false, nil
		}

		operation := domain.Operation{
			ID: domain.OperationID(operationID), TenantID: tenantID, WorkspaceID: workspaceID,
			ResourcePlaneID: resourcePlaneID, Type: action, Status: domain.OperationPending,
			IdempotencyKey: idempotencyKey, CreatedAt: now, UpdatedAt: now,
		}
		request := reconcileRequest{WorkspaceID: workspaceID, Desired: desired}
		stored, err := operations.InsertOrGet(ctx, tx, operation, request)
		if err != nil {
			return false, fmt.Errorf("insert reconciled Operation: %w", err)
		}
		if stored.ID != operation.ID {
			return false, errors.New("unexpected Operation returned for Reconciler idempotency key")
		}
		command, err := json.Marshal(requestedCommand{
			SchemaVersion: 1, OperationID: operation.ID, TenantID: tenantID,
			WorkspaceID: workspaceID, ResourcePlaneID: resourcePlaneID, Type: action, CreatedAt: now,
		})
		if err != nil {
			return false, fmt.Errorf("encode reconciled Operation command: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO operation_events (operation_id, sequence, event_type, payload_json, created_at) VALUES ($1, 1, 'operation.reconciled', $2, $3)`, operation.ID, string(command), now); err != nil {
			return false, fmt.Errorf("insert reconciled Operation event: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload_json, created_at, published_at, attempt) VALUES ($1, 'operation', $2, 'operation.requested', $3, $4, NULL, 0)`, eventID, operation.ID, string(command), now); err != nil {
			return false, fmt.Errorf("insert reconciled Operation Outbox event: %w", err)
		}
		return true, nil
	})
}
