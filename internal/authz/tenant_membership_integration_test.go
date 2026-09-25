//go:build integration

package authz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestTenantMembershipAndRoleAuthorization(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC()
	seed := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants (id, name, created_at) VALUES ('tenant_authz', 'Authz', $1)`, []any{now}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('usr_member', 'member-sub', '', $1), ('usr_owner', 'owner-sub', '', $1)`, []any{now}},
		{`INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ('tenant_authz', 'usr_member', 'member', $1), ('tenant_authz', 'usr_owner', 'owner', $1)`, []any{now}},
	}
	for _, statement := range seed {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed authorization test: %v", err)
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	member, err := RequireTenantMembership(ctx, tx, "usr_member", "tenant_authz")
	if err != nil || member.Role != domain.TenantRoleMember {
		t.Fatalf("load Tenant Member = %+v, %v", member, err)
	}
	if _, err := RequireTenantRole(ctx, tx, "usr_owner", "tenant_authz", domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
		t.Fatalf("Tenant Owner should satisfy owner/admin policy: %v", err)
	}
	if _, err := RequireTenantRole(ctx, tx, "usr_member", "tenant_authz", domain.TenantRoleOwner, domain.TenantRoleAdmin); !errors.Is(err, ErrTenantAccessDenied) {
		t.Fatalf("Tenant Member role denial = %v, want ErrTenantAccessDenied", err)
	}
	for _, identity := range []struct {
		user   domain.UserID
		tenant domain.TenantID
	}{{"usr_missing", "tenant_authz"}, {"usr_member", "tenant_missing"}, {"", "tenant_authz"}, {"usr_member", ""}} {
		if _, err := RequireTenantMembership(ctx, tx, identity.user, identity.tenant); !errors.Is(err, ErrTenantAccessDenied) {
			t.Errorf("missing identity (%q, %q) returned %v, want ErrTenantAccessDenied", identity.user, identity.tenant, err)
		}
	}
	if _, err := RequireTenantMembership(ctx, nil, "usr_member", "tenant_authz"); err == nil {
		t.Fatal("nil transaction should fail")
	}
}
