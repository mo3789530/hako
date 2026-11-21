package workspaceactivity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/store/audit"
	"github.com/mo3789530/hako/internal/store/operations"
)

const IdleSuspendSystemActor = "hako-workspace-idle-reconciler"

type idleSuspendOperationEvent struct {
	SchemaVersion     int                    `json:"schema_version"`
	WorkspaceRevision int64                  `json:"workspace_revision"`
	OperationID       domain.OperationID     `json:"operation_id"`
	TenantID          domain.TenantID        `json:"tenant_id"`
	WorkspaceID       domain.WorkspaceID     `json:"workspace_id"`
	ResourcePlaneID   domain.ResourcePlaneID `json:"resource_plane_id"`
	Type              domain.OperationType   `json:"type"`
	CreatedAt         time.Time              `json:"created_at"`
}

// SuspendIdle revalidates the enabled policy, allocation, desired/observed
// state, lease set, latest disconnect time, and pending operations while
// holding the Workspace status row lock. It commits the normal suspend
// Operation, event, outbox, and explicit system audit together with the state
// transition. A connected Gateway renewer locks the same status row before it
// can create or refresh its lease.
func SuspendIdle(ctx context.Context, tx pgx.Tx, candidate IdleCandidate, now time.Time) (bool, error) {
	if ctx == nil || tx == nil || candidate.TenantID == "" || candidate.WorkspaceID == "" || candidate.ResourcePlaneID == "" ||
		candidate.MicroVMID == "" || candidate.AllocationGeneration < 1 || now.IsZero() {
		return false, ErrInvalidLease
	}
	var tenantID domain.TenantID
	var desired, observed string
	var workspaceRevision int64
	var planeID domain.ResourcePlaneID
	var microVMID string
	var generation int64
	err := tx.QueryRow(ctx, `SELECT w.tenant_id, s.desired_state, s.observed_state,
		COALESCE(s.reconcile_revision, 0), p.resource_plane_id, rb.external_id, rb.allocation_generation
		FROM workspaces w
		JOIN workspace_status s ON s.workspace_id = w.id
		JOIN placements p ON p.workspace_id = w.id
		JOIN resource_bindings rb ON rb.workspace_id = w.id AND rb.resource_plane_id = p.resource_plane_id
		WHERE w.tenant_id = $1 AND w.id = $2 AND p.resource_plane_id = $3
			AND rb.kind = 'lambda_microvm'
		FOR UPDATE OF s`, candidate.TenantID, candidate.WorkspaceID, candidate.ResourcePlaneID).
		Scan(&tenantID, &desired, &observed, &workspaceRevision, &planeID, &microVMID, &generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock idle Workspace binding: %w", err)
	}
	if tenantID != candidate.TenantID || planeID != candidate.ResourcePlaneID || microVMID != candidate.MicroVMID || generation != candidate.AllocationGeneration ||
		desired != string(domain.DesiredWorkspaceRunning) || observed != string(domain.ObservedWorkspaceRunning) {
		return false, nil
	}
	var policy Policy
	var policyUpdated time.Time
	err = tx.QueryRow(ctx, `SELECT tenant_id, enabled, grace_period_seconds, revision, updated_at
		FROM tenant_workspace_idle_policies WHERE tenant_id = $1 FOR UPDATE`, candidate.TenantID).
		Scan(&policy.TenantID, &policy.Enabled, &policy.GracePeriodSeconds, &policy.Revision, &policyUpdated)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !policy.Enabled {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock Tenant Workspace idle policy: %w", err)
	}
	if policy.GracePeriodSeconds < int(MinimumIdleGracePeriod.Seconds()) || policy.GracePeriodSeconds > int(MaximumIdleGracePeriod.Seconds()) || policy.Revision < 1 {
		return false, errors.New("stored Tenant Workspace idle policy is invalid")
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspace_connection_leases
		WHERE tenant_id = $1 AND workspace_id = $2 AND resource_plane_id = $3 AND microvm_id = $4
			AND allocation_generation = $5 AND released_at IS NULL AND expires_at > $6)`,
		candidate.TenantID, candidate.WorkspaceID, candidate.ResourcePlaneID, candidate.MicroVMID, candidate.AllocationGeneration, now.UTC()).Scan(&active); err != nil {
		return false, fmt.Errorf("check active Workspace connection leases: %w", err)
	}
	if active {
		return false, nil
	}
	var lastActivity time.Time
	err = tx.QueryRow(ctx, `SELECT MAX(COALESCE(released_at, expires_at)) FROM workspace_connection_leases
		WHERE tenant_id = $1 AND workspace_id = $2 AND resource_plane_id = $3 AND microvm_id = $4 AND allocation_generation = $5`,
		candidate.TenantID, candidate.WorkspaceID, candidate.ResourcePlaneID, candidate.MicroVMID, candidate.AllocationGeneration).Scan(&lastActivity)
	if err != nil {
		return false, fmt.Errorf("get last Workspace connection activity: %w", err)
	}
	idleDeadline := now.UTC().Add(-time.Duration(policy.GracePeriodSeconds) * time.Second)
	if lastActivity.IsZero() || lastActivity.After(idleDeadline) {
		return false, nil
	}
	var operationPending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM operations WHERE workspace_id = $1 AND status IN ('pending', 'running'))`, candidate.WorkspaceID).Scan(&operationPending); err != nil {
		return false, fmt.Errorf("check pending Workspace operations: %w", err)
	}
	if operationPending {
		return false, nil
	}
	var resourcePlaneID domain.ResourcePlaneID
	if err := tx.QueryRow(ctx, `SELECT resource_plane_id FROM placements WHERE workspace_id = $1`, candidate.WorkspaceID).Scan(&resourcePlaneID); err != nil {
		return false, fmt.Errorf("read idle Workspace Placement: %w", err)
	}
	if resourcePlaneID != candidate.ResourcePlaneID {
		return false, nil
	}
	var nextRevision int64
	if err := tx.QueryRow(ctx, `UPDATE workspace_status SET desired_state = 'suspended', updated_at = $1,
		reconcile_revision = COALESCE(reconcile_revision, 0) + 1
		WHERE workspace_id = $2 AND desired_state = 'running' AND observed_state = 'running'
			AND COALESCE(reconcile_revision, 0) = $3 RETURNING reconcile_revision`, now.UTC(), candidate.WorkspaceID, workspaceRevision).Scan(&nextRevision); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("set idle Workspace desired state: %w", err)
	}
	operationID, err := idgen.New("op_")
	if err != nil {
		return false, err
	}
	eventID, err := idgen.New("evt_")
	if err != nil {
		return false, err
	}
	keyMaterial := fmt.Sprintf("%s\n%s\n%d\n%d\n%d", candidate.WorkspaceID, candidate.ResourcePlaneID, nextRevision,
		candidate.AllocationGeneration, policy.Revision)
	keyHash := sha256.Sum256([]byte(keyMaterial))
	idempotencyKey := "idle-suspend-" + hex.EncodeToString(keyHash[:])
	request := struct {
		WorkspaceID          domain.WorkspaceID     `json:"workspace_id"`
		WorkspaceRevision    int64                  `json:"workspace_revision"`
		ResourcePlaneID      domain.ResourcePlaneID `json:"resource_plane_id"`
		AllocationGeneration int64                  `json:"allocation_generation"`
		PolicyRevision       int64                  `json:"policy_revision"`
	}{candidate.WorkspaceID, nextRevision, candidate.ResourcePlaneID, candidate.AllocationGeneration, policy.Revision}
	operation, err := operations.InsertOrGet(ctx, tx, domain.Operation{
		ID: domain.OperationID(operationID), TenantID: candidate.TenantID, WorkspaceID: candidate.WorkspaceID,
		ResourcePlaneID: candidate.ResourcePlaneID, Type: domain.OperationSuspend, Status: domain.OperationPending,
		IdempotencyKey: idempotencyKey, CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}, request)
	if err != nil {
		return false, err
	}
	payload, err := json.Marshal(idleSuspendOperationEvent{SchemaVersion: 2, WorkspaceRevision: nextRevision,
		OperationID: operation.ID, TenantID: candidate.TenantID, WorkspaceID: candidate.WorkspaceID,
		ResourcePlaneID: candidate.ResourcePlaneID, Type: domain.OperationSuspend, CreatedAt: now.UTC()})
	if err != nil {
		return false, fmt.Errorf("encode idle suspend operation event: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO operation_events (operation_id, sequence, event_type, payload_json, created_at)
		VALUES ($1, 1, 'operation.requested', $2, $3)`, operation.ID, string(payload), now.UTC()); err != nil {
		return false, fmt.Errorf("insert idle suspend operation event: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outbox_events
		(id, aggregate_type, aggregate_id, event_type, payload_json, created_at, published_at, attempt)
		VALUES ($1, 'operation', $2, 'operation.requested', $3, $4, NULL, 0)`, eventID, operation.ID, string(payload), now.UTC()); err != nil {
		return false, fmt.Errorf("insert idle suspend outbox event: %w", err)
	}
	details, err := json.Marshal(map[string]any{"operation_id": operation.ID, "reason": "idle_grace_period_elapsed",
		"workspace_revision": nextRevision, "policy_revision": policy.Revision, "grace_period_seconds": policy.GracePeriodSeconds,
		"allocation_generation": candidate.AllocationGeneration, "idle_since": lastActivity})
	if err != nil {
		return false, fmt.Errorf("encode idle suspend audit details: %w", err)
	}
	if err := audit.AppendSystem(ctx, tx, audit.Event{TenantID: candidate.TenantID, ActorType: "system", ActorName: IdleSuspendSystemActor,
		Action: "workspace.suspend", TargetType: "workspace", TargetID: string(candidate.WorkspaceID), Details: details, OccurredAt: now.UTC()}); err != nil {
		return false, err
	}
	return true, nil
}
