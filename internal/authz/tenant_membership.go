// Package authz implements tenant-level authorization primitives.
package authz

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

// ErrTenantAccessDenied intentionally covers both a missing Tenant and a user
// without membership, preventing callers from probing Tenant identifiers.
var ErrTenantAccessDenied = errors.New("tenant access denied")

// RequireTenantMembership returns the caller's Tenant role or a non-disclosing
// access-denied error. Call it inside the same transaction as the protected
// resource read or mutation whenever possible.
func RequireTenantMembership(ctx context.Context, tx pgx.Tx, userID domain.UserID, tenantID domain.TenantID) (domain.TenantMembership, error) {
	if tx == nil {
		return domain.TenantMembership{}, errors.New("transaction is required")
	}
	if userID == "" || tenantID == "" {
		return domain.TenantMembership{}, ErrTenantAccessDenied
	}
	var membership domain.TenantMembership
	err := tx.QueryRow(ctx, `SELECT tenant_id, user_id, role, joined_at FROM tenant_members WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID).Scan(
		&membership.TenantID, &membership.UserID, &membership.Role, &membership.JoinedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.TenantMembership{}, ErrTenantAccessDenied
	}
	if err != nil {
		return domain.TenantMembership{}, fmt.Errorf("load Tenant membership: %w", err)
	}
	return membership, nil
}

// RequireTenantRole checks that the caller has a membership role explicitly
// allowed by the operation. Passing no roles accepts any Tenant member.
func RequireTenantRole(ctx context.Context, tx pgx.Tx, userID domain.UserID, tenantID domain.TenantID, allowed ...domain.TenantRole) (domain.TenantMembership, error) {
	membership, err := RequireTenantMembership(ctx, tx, userID, tenantID)
	if err != nil {
		return domain.TenantMembership{}, err
	}
	if len(allowed) == 0 {
		return membership, nil
	}
	for _, role := range allowed {
		if membership.Role == role {
			return membership, nil
		}
	}
	return domain.TenantMembership{}, ErrTenantAccessDenied
}
