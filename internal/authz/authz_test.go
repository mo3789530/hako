package authz

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

type guardTx struct {
	pgx.Tx
	row       pgx.Row
	query     string
	arguments []any
}

func (tx *guardTx) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	tx.query = query
	tx.arguments = append([]any(nil), args...)
	return tx.row
}

type membershipRow struct {
	membership domain.TenantMembership
	err        error
}

func (row membershipRow) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	if len(dest) != 4 {
		return fmt.Errorf("unexpected scan destination count: %d", len(dest))
	}
	*dest[0].(*domain.TenantID) = row.membership.TenantID
	*dest[1].(*domain.UserID) = row.membership.UserID
	*dest[2].(*domain.TenantRole) = row.membership.Role
	*dest[3].(*time.Time) = row.membership.JoinedAt
	return nil
}

func TestMembershipRejectsMissingTransactionOrIdentity(t *testing.T) {
	if _, err := RequireTenantMembership(t.Context(), nil, "usr_1", "tenant_1"); err == nil {
		t.Fatal("nil transaction should fail")
	}
	for _, identity := range []struct {
		user   domain.UserID
		tenant domain.TenantID
	}{{"", "tenant_1"}, {"usr_1", ""}} {
		if _, err := RequireTenantMembership(t.Context(), &guardTx{}, identity.user, identity.tenant); !errors.Is(err, ErrTenantAccessDenied) {
			t.Errorf("empty identity should be non-disclosing access denied, got %v", err)
		}
	}
}

func TestRequireTenantMembershipReturnsRoleAndScopesQuery(t *testing.T) {
	joinedAt := time.Date(2026, time.October, 24, 12, 0, 0, 0, time.UTC)
	membership := domain.TenantMembership{TenantID: "tenant_authz", UserID: "usr_member", Role: domain.TenantRoleMember, JoinedAt: joinedAt}
	tx := &guardTx{row: membershipRow{membership: membership}}
	got, err := RequireTenantMembership(t.Context(), tx, membership.UserID, membership.TenantID)
	if err != nil || got != membership {
		t.Fatalf("RequireTenantMembership() = %+v, %v; want %+v", got, err, membership)
	}
	if tx.query != `SELECT tenant_id, user_id, role, joined_at FROM tenant_members WHERE tenant_id = $1 AND user_id = $2` ||
		len(tx.arguments) != 2 || tx.arguments[0] != membership.TenantID || tx.arguments[1] != membership.UserID {
		t.Fatalf("membership lookup was not scoped by Tenant and user: query=%q args=%v", tx.query, tx.arguments)
	}
}

func TestRequireTenantMembershipHidesMissingTenantAndMembership(t *testing.T) {
	for _, identity := range []struct {
		user   domain.UserID
		tenant domain.TenantID
	}{{"usr_missing", "tenant_authz"}, {"usr_member", "tenant_missing"}} {
		errFromDB := pgx.ErrNoRows
		tx := &guardTx{row: membershipRow{err: errFromDB}}
		if _, err := RequireTenantMembership(t.Context(), tx, identity.user, identity.tenant); !errors.Is(err, ErrTenantAccessDenied) {
			t.Errorf("missing identity (%q, %q) leaked or changed: got %v", identity.user, identity.tenant, err)
		}
	}
}

func TestRequireTenantMembershipWrapsUnexpectedDatabaseFailure(t *testing.T) {
	databaseErr := errors.New("connection reset")
	tx := &guardTx{row: membershipRow{err: databaseErr}}
	if _, err := RequireTenantMembership(t.Context(), tx, "usr_member", "tenant_authz"); !errors.Is(err, databaseErr) {
		t.Fatalf("RequireTenantMembership() error = %v, want wrapped database error", err)
	}
}

func TestRequireTenantRoleAllowsAnyMemberAndOnlyExplicitRoles(t *testing.T) {
	member := domain.TenantMembership{TenantID: "tenant_authz", UserID: "usr_member", Role: domain.TenantRoleMember}
	t.Run("empty allow list accepts membership", func(t *testing.T) {
		got, err := RequireTenantRole(t.Context(), &guardTx{row: membershipRow{membership: member}}, member.UserID, member.TenantID)
		if err != nil || got.Role != domain.TenantRoleMember {
			t.Fatalf("RequireTenantRole(any) = %+v, %v", got, err)
		}
	})
	t.Run("matching role is accepted", func(t *testing.T) {
		got, err := RequireTenantRole(t.Context(), &guardTx{row: membershipRow{membership: member}}, member.UserID, member.TenantID,
			domain.TenantRoleOwner, domain.TenantRoleMember)
		if err != nil || got.Role != domain.TenantRoleMember {
			t.Fatalf("RequireTenantRole(member) = %+v, %v", got, err)
		}
	})
	t.Run("nonmatching role is denied", func(t *testing.T) {
		got, err := RequireTenantRole(t.Context(), &guardTx{row: membershipRow{membership: member}}, member.UserID, member.TenantID,
			domain.TenantRoleOwner, domain.TenantRoleAdmin)
		if !errors.Is(err, ErrTenantAccessDenied) || got != (domain.TenantMembership{}) {
			t.Fatalf("RequireTenantRole(owner/admin) = %+v, %v; want non-disclosing denial", got, err)
		}
	})
}
