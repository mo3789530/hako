package workspaces

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
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
