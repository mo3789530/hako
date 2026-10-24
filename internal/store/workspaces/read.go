package workspaces

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/quotas"
)

var ErrWorkspaceAccessDenied = errors.New("workspace access denied")

type View struct {
	Workspace domain.Workspace       `json:"workspace"`
	Status    domain.WorkspaceStatus `json:"status"`
}

type ListResult struct {
	Items  []View `json:"items"`
	Total  int64  `json:"total"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
}

// QuotaUsage reports the Tenant-wide active Workspace count used by admission.
// A pending deletion remains active until the Resource Plane confirms cleanup.
type QuotaUsage struct {
	WorkspacesUsed        int64 `json:"workspaces_used"`
	WorkspacesLimit       int64 `json:"workspaces_limit"`
	WorkloadRunsUsed      int64 `json:"workload_runs_used"`
	WorkloadRunsLimit     int64 `json:"workload_runs_limit"`
	PreviewRunsUsed       int64 `json:"preview_runs_used"`
	PreviewRunsLimit      int64 `json:"preview_runs_limit"`
	PreviewLifetimeLimit  int64 `json:"preview_lifetime_seconds_limit"`
	ArtifactBytesUsed     int64 `json:"artifact_bytes_used"`
	ArtifactBytesReserved int64 `json:"artifact_bytes_reserved"`
	ArtifactBytesLimit    int64 `json:"artifact_bytes_limit"`
}

func ReadQuotaUsage(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID) (QuotaUsage, error) {
	if tx == nil || tenantID == "" {
		return QuotaUsage{}, errors.New("Tenant quota usage requires a transaction and Tenant ID")
	}
	var usage QuotaUsage
	limits, err := quotas.Read(ctx, tx, tenantID)
	if err != nil {
		return QuotaUsage{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM workspaces w LEFT JOIN workspace_status s ON s.workspace_id = w.id
		WHERE w.tenant_id = $1 AND COALESCE(s.observed_state, 'pending') <> 'deleted'`, tenantID).Scan(&usage.WorkspacesUsed); err != nil {
		return QuotaUsage{}, fmt.Errorf("read Tenant Workspace quota usage: %w", err)
	}
	usage.WorkspacesLimit = int64(limits.Workspaces)
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM workload_runs WHERE tenant_id = $1
		AND state NOT IN ('succeeded','failed','cancelled','timed_out')`, tenantID).Scan(&usage.WorkloadRunsUsed); err != nil {
		return QuotaUsage{}, fmt.Errorf("read Tenant concurrent Workload usage: %w", err)
	}
	usage.WorkloadRunsLimit = int64(limits.ConcurrentWorkloadRuns)
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT COUNT(*) FROM workload_runs WHERE tenant_id=$1 AND kind='preview' AND state NOT IN ('succeeded','failed','cancelled','timed_out')) +
		(SELECT COUNT(*) FROM repository_previews WHERE tenant_id=$1 AND state IN ('awaiting_runtime','running','cleanup_pending'))`, tenantID).Scan(&usage.PreviewRunsUsed); err != nil {
		return QuotaUsage{}, fmt.Errorf("read Tenant Preview quota usage: %w", err)
	}
	usage.PreviewRunsLimit = int64(limits.PreviewRuns)
	usage.PreviewLifetimeLimit = limits.PreviewLifetimeSeconds
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(size_bytes), 0) FROM workload_artifacts WHERE tenant_id = $1`, tenantID).Scan(&usage.ArtifactBytesUsed); err != nil {
		return QuotaUsage{}, fmt.Errorf("read Tenant artifact storage usage: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(reserved_bytes), 0) FROM workload_artifact_quota_reservations WHERE tenant_id = $1`, tenantID).Scan(&usage.ArtifactBytesReserved); err != nil {
		return QuotaUsage{}, fmt.Errorf("read Tenant artifact storage reservations: %w", err)
	}
	usage.ArtifactBytesLimit = limits.ArtifactBytes
	return usage, nil
}

// ListVisible returns the workspaces the supplied Tenant membership may see.
// Members see their own; Owners and Admins see all within the Tenant.
func ListVisible(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, userID domain.UserID, role domain.TenantRole, limit, offset int) (ListResult, error) {
	if tx == nil {
		return ListResult{}, errors.New("transaction is required")
	}
	if tenantID == "" || userID == "" || !validTenantRole(role) {
		return ListResult{}, ErrWorkspaceAccessDenied
	}
	if limit < 1 || limit > 100 || offset < 0 || offset > 1000000 {
		return ListResult{}, errors.New("Workspace page size or offset is out of range")
	}
	var result ListResult
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM workspaces w WHERE w.tenant_id = $1 AND ($3 <> 'member' OR w.owner_id = $2)`, tenantID, userID, role).Scan(&result.Total); err != nil {
		return ListResult{}, fmt.Errorf("count visible Workspaces: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT w.id, w.tenant_id, w.owner_id, w.name, w.runtime_class, w.image, w.created_at, w.updated_at,
		s.workspace_id, s.desired_state, s.observed_state, s.updated_at
		FROM workspaces w JOIN workspace_status s ON s.workspace_id = w.id
		WHERE w.tenant_id = $1 AND ($3 <> 'member' OR w.owner_id = $2)
		ORDER BY w.created_at DESC, w.id DESC LIMIT $4 OFFSET $5`, tenantID, userID, role, limit, offset)
	if err != nil {
		return ListResult{}, fmt.Errorf("list visible Workspaces: %w", err)
	}
	defer rows.Close()
	result.Limit, result.Offset = limit, offset
	result.Items = make([]View, 0)
	for rows.Next() {
		var view View
		if err := rows.Scan(
			&view.Workspace.ID, &view.Workspace.TenantID, &view.Workspace.OwnerID, &view.Workspace.Name,
			&view.Workspace.RuntimeClass, &view.Workspace.Image, &view.Workspace.CreatedAt, &view.Workspace.UpdatedAt,
			&view.Status.WorkspaceID, &view.Status.DesiredState, &view.Status.ObservedState, &view.Status.UpdatedAt,
		); err != nil {
			return ListResult{}, fmt.Errorf("scan visible Workspace: %w", err)
		}
		result.Items = append(result.Items, view)
	}
	if err := rows.Err(); err != nil {
		return ListResult{}, fmt.Errorf("iterate visible Workspaces: %w", err)
	}
	return result, nil
}

// GetVisible returns a Workspace only when its Tenant and owner visibility
// rules allow access. Missing and unauthorized resources share one sentinel.
func GetVisible(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, workspaceID domain.WorkspaceID, userID domain.UserID, role domain.TenantRole) (View, error) {
	if tx == nil {
		return View{}, errors.New("transaction is required")
	}
	if tenantID == "" || workspaceID == "" || userID == "" || !validTenantRole(role) {
		return View{}, ErrWorkspaceAccessDenied
	}
	var view View
	err := tx.QueryRow(ctx, `SELECT w.id, w.tenant_id, w.owner_id, w.name, w.runtime_class, w.image, w.created_at, w.updated_at,
		s.workspace_id, s.desired_state, s.observed_state, s.updated_at
		FROM workspaces w JOIN workspace_status s ON s.workspace_id = w.id
		WHERE w.tenant_id = $1 AND w.id = $2 AND ($4 <> 'member' OR w.owner_id = $3)`, tenantID, workspaceID, userID, role).Scan(
		&view.Workspace.ID, &view.Workspace.TenantID, &view.Workspace.OwnerID, &view.Workspace.Name,
		&view.Workspace.RuntimeClass, &view.Workspace.Image, &view.Workspace.CreatedAt, &view.Workspace.UpdatedAt,
		&view.Status.WorkspaceID, &view.Status.DesiredState, &view.Status.ObservedState, &view.Status.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return View{}, ErrWorkspaceAccessDenied
	}
	if err != nil {
		return View{}, fmt.Errorf("get visible Workspace: %w", err)
	}
	return view, nil
}

func validTenantRole(role domain.TenantRole) bool {
	switch role {
	case domain.TenantRoleMember, domain.TenantRoleOwner, domain.TenantRoleAdmin:
		return true
	default:
		return false
	}
}
