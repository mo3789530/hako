package authz

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

var (
	ErrRepositoryAccessDenied = errors.New("repository access denied")
	ErrRepositoryActionDenied = errors.New("repository action denied")
)

type RepositoryKey struct {
	TenantID       domain.TenantID
	InstallationID int64
	RepositoryID   int64
}

type RepositoryRole string

const (
	RepositoryViewer   RepositoryRole = "viewer"
	RepositoryOperator RepositoryRole = "operator"
)

func (r RepositoryRole) Valid() bool {
	return r == RepositoryViewer || r == RepositoryOperator
}

type RepositoryAction string

const (
	RepositoryRead   RepositoryAction = "read"
	RepositoryRun    RepositoryAction = "run"
	RepositoryManage RepositoryAction = "manage"
)

// ActiveRepositoryBinding returns the binding timestamp used to invalidate grants
// if an installation is rebound. Use the same transaction for subsequent access.
func ActiveRepositoryBinding(ctx context.Context, tx pgx.Tx, key RepositoryKey) (time.Time, error) {
	var boundAt time.Time
	err := tx.QueryRow(ctx, `SELECT b.bound_at FROM tenant_github_repositories r
		JOIN github_app_installation_bindings b USING (tenant_id, installation_id)
		JOIN tenant_github_installations i USING (tenant_id, installation_id)
		WHERE r.tenant_id=$1 AND r.installation_id=$2 AND r.github_repository_id=$3
		AND i.status='active' AND i.suspended_at IS NULL`, key.TenantID, key.InstallationID, key.RepositoryID).Scan(&boundAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrRepositoryAccessDenied
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("load active repository binding: %w", err)
	}
	return boundAt, nil
}

// RequireRepositoryAction checks the complete repository identity and current
// Tenant membership in the transaction used by the caller.
func RequireRepositoryAction(ctx context.Context, tx pgx.Tx, userID domain.UserID, key RepositoryKey, action RepositoryAction) error {
	if action != RepositoryRead && action != RepositoryRun && action != RepositoryManage {
		return ErrRepositoryActionDenied
	}
	member, err := RequireTenantMembership(ctx, tx, userID, key.TenantID)
	if err != nil {
		return err
	}
	boundAt, err := ActiveRepositoryBinding(ctx, tx, key)
	if err != nil {
		return err
	}
	if member.Role == domain.TenantRoleOwner || member.Role == domain.TenantRoleAdmin {
		return nil
	}
	var role RepositoryRole
	err = tx.QueryRow(ctx, `SELECT role FROM tenant_repository_members
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND user_id=$4
		AND membership_joined_at=$5 AND installation_bound_at=$6`,
		key.TenantID, key.InstallationID, key.RepositoryID, userID, member.JoinedAt, boundAt).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRepositoryAccessDenied
	}
	if err != nil {
		return fmt.Errorf("load repository grant: %w", err)
	}
	if !role.Valid() {
		return ErrRepositoryAccessDenied
	}
	if action == RepositoryRead || (action == RepositoryRun && role == RepositoryOperator) {
		return nil
	}
	return ErrRepositoryActionDenied
}
