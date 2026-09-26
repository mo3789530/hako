//go:build integration

package githubregistry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/authz"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestCompleteSetupStateActivatesOneTenantBindingExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC()
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants (id, name, created_at) VALUES ($1, $2, $3)`, []any{"tenant_setup_one", "Setup One", now}},
		{`INSERT INTO tenants (id, name, created_at) VALUES ($1, $2, $3)`, []any{"tenant_setup_two", "Setup Two", now}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ($1, $2, '', $3)`, []any{"usr_setup_owner", "setup-owner", now}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ($1, $2, '', $3)`, []any{"usr_setup_member", "setup-member", now}},
		{`INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ($1, $2, $3, $4)`, []any{"tenant_setup_one", "usr_setup_owner", "owner", now}},
		{`INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ($1, $2, $3, $4)`, []any{"tenant_setup_two", "usr_setup_owner", "owner", now}},
		{`INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ($1, $2, $3, $4)`, []any{"tenant_setup_one", "usr_setup_member", "member", now}},
	} {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	policy := transaction.DefaultPolicy()
	state := "opaque-one-time-state-for-github-app-install"
	stateHash := HashSetupState(state)
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		if _, err := authz.RequireTenantRole(ctx, tx, "usr_setup_owner", "tenant_setup_one", domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
			return struct{}{}, err
		}
		if err := CreateSetupState(ctx, tx, "tenant_setup_one", "usr_setup_owner", stateHash, strings.Repeat("v", 43), now, now.Add(10*time.Minute)); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, SetSetupInstallation(ctx, tx, stateHash, 7741, now.Add(time.Second))
	}); err != nil {
		t.Fatalf("create setup state: %v", err)
	}
	var installation Installation
	installation, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (Installation, error) {
		return CompleteSetupState(ctx, tx, stateHash, 7741, "acme", now.Add(time.Minute))
	})
	if err != nil {
		t.Fatalf("complete setup: %v", err)
	}
	if installation.TenantID != "tenant_setup_one" || installation.Status != "active" || installation.AccountLogin != "acme" {
		t.Fatalf("activated Installation = %+v", installation)
	}
	var verifier string
	if err := pool.QueryRow(ctx, `SELECT code_verifier FROM github_installation_setup_states WHERE state_sha256 = $1`, stateHash).Scan(&verifier); err != nil || verifier != "" {
		t.Fatalf("consumed setup state verifier = %q, err=%v; want erased", verifier, err)
	}
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (Installation, error) {
		return CompleteSetupState(ctx, tx, stateHash, 7741, "acme", now.Add(2*time.Minute))
	}); !errors.Is(err, ErrInvalidSetupState) {
		t.Fatalf("replay state error = %v, want invalid/consumed state", err)
	}

	otherStateHash := HashSetupState("second-independent-state-for-installation")
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		if err := CreateSetupState(ctx, tx, "tenant_setup_two", "usr_setup_owner", otherStateHash, strings.Repeat("v", 43), now, now.Add(10*time.Minute)); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, SetSetupInstallation(ctx, tx, otherStateHash, 7741, now.Add(time.Second))
	}); err != nil {
		t.Fatalf("create second setup state: %v", err)
	}
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (Installation, error) {
		return CompleteSetupState(ctx, tx, otherStateHash, 7741, "acme", now.Add(time.Minute))
	}); !errors.Is(err, ErrInstallationBound) {
		t.Fatalf("cross-Tenant rebinding error = %v, want conflict", err)
	}
	var boundTenant string
	if err := pool.QueryRow(ctx, `SELECT tenant_id FROM github_app_installation_bindings WHERE installation_id = 7741`).Scan(&boundTenant); err != nil || boundTenant != "tenant_setup_one" {
		t.Fatalf("global binding tenant = %q, err=%v", boundTenant, err)
	}
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (Installation, error) {
		return CompleteSetupState(ctx, tx, HashSetupState("missing-state"), 999, "other", now)
	}); !errors.Is(err, ErrInvalidSetupState) {
		t.Fatalf("missing setup state error = %v", err)
	}
}

func TestCompleteSetupStateRechecksTenantOwnerOrAdmin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, created_at) VALUES ('tenant_setup_rbac', 'Setup RBAC', $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('usr_setup_member_only', 'setup-member-only', '', $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ('tenant_setup_rbac', 'usr_setup_member_only', 'member', $1)`, now); err != nil {
		t.Fatal(err)
	}
	stateHash := HashSetupState("member-state-should-not-activate-installation")
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		if err := CreateSetupState(ctx, tx, "tenant_setup_rbac", "usr_setup_member_only", stateHash, strings.Repeat("v", 43), now, now.Add(10*time.Minute)); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, SetSetupInstallation(ctx, tx, stateHash, 7742, now.Add(time.Second))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (Installation, error) {
		return CompleteSetupState(ctx, tx, stateHash, 7742, "acme", now.Add(time.Minute))
	}); !errors.Is(err, authz.ErrTenantAccessDenied) {
		t.Fatalf("member activation error = %v, want Tenant owner/admin denial", err)
	}
}
