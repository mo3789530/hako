package workspaces

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

type guardTx struct{ pgx.Tx }

func TestReadVisibilityRejectsInvalidInputs(t *testing.T) {
	ctx := t.Context()
	if _, err := ListVisible(ctx, nil, "tenant_1", "usr_1", domain.TenantRoleMember, 10, 0); err == nil {
		t.Fatal("nil transaction should fail")
	}
	for _, role := range []domain.TenantRole{"", "unknown"} {
		if _, err := ListVisible(ctx, guardTx{}, "tenant_1", "usr_1", role, 10, 0); !errors.Is(err, ErrWorkspaceAccessDenied) {
			t.Errorf("invalid role %q should be denied, got %v", role, err)
		}
	}
	if _, err := ListVisible(ctx, nil, "tenant_1", "usr_1", domain.TenantRoleMember, 10, 0); err == nil {
		t.Fatal("nil transaction should be checked before query")
	}
	for _, bounds := range [][2]int{{0, 0}, {101, 0}, {10, -1}, {10, 1_000_001}} {
		if _, err := ListVisible(ctx, guardTx{}, "tenant_1", "usr_1", domain.TenantRoleMember, bounds[0], bounds[1]); err == nil {
			t.Errorf("invalid pagination %v should fail", bounds)
		}
	}
	if !validTenantRole(domain.TenantRoleMember) || !validTenantRole(domain.TenantRoleOwner) || !validTenantRole(domain.TenantRoleAdmin) || validTenantRole("unknown") {
		t.Fatal("tenant role validation mismatch")
	}
	for _, role := range []domain.TenantRole{domain.TenantRoleMember, domain.TenantRoleOwner, domain.TenantRoleAdmin} {
		if _, err := GetVisible(ctx, nil, "tenant_1", "ws_1", "usr_1", role); err == nil {
			t.Fatalf("nil transaction for role %q should fail", role)
		}
	}
	if _, err := GetVisible(ctx, guardTx{}, "tenant_1", "ws_1", "usr_1", "unknown"); !errors.Is(err, ErrWorkspaceAccessDenied) {
		t.Fatalf("invalid role should be denied, got %v", err)
	}
}
