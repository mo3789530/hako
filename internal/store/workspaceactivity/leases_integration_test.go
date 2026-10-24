//go:build integration

package workspaceactivity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestConnectionLeaseRenewReleaseExpiryAndRuntimeFence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants (id, name, created_at) VALUES ('tenant_activity', 'activity', $1)`, []any{now}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('user_activity', 'activity-subject', '', $1)`, []any{now}},
		{`INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES ('rp_activity', 'aws', 'ap-northeast-1', '[]')`, nil},
		{`INSERT INTO workspaces (id, tenant_id, owner_id, name, runtime_class, image, created_at, updated_at)
			VALUES ('ws_activity', 'tenant_activity', 'user_activity', 'activity', 'standard', 'image:test', $1, $1)`, []any{now}},
		{`INSERT INTO placements (workspace_id, resource_plane_id, placed_at) VALUES ('ws_activity', 'rp_activity', $1)`, []any{now}},
		{`INSERT INTO workspace_status (workspace_id, desired_state, observed_state, updated_at)
			VALUES ('ws_activity', 'running', 'running', $1)`, []any{now}},
		{`INSERT INTO resource_bindings (workspace_id, resource_plane_id, kind, external_id, created_at, allocation_generation)
			VALUES ('ws_activity', 'rp_activity', 'lambda_microvm', 'vm_activity', $1, 4)`, []any{now}},
		{`INSERT INTO sessions (id, user_id, tenant_id, workspace_id, resource_plane_id, permissions_json, expires_at, created_at)
			VALUES ('abcdef0123456789abcdef0123456789abcdef0123456789', 'user_activity', 'tenant_activity', 'ws_activity', 'rp_activity', '["shell"]', $1, $2)`, []any{now.Add(10 * time.Minute), now}},
	} {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed Workspace lease database: %v\nquery: %s", err, statement.query)
		}
	}
	lease := Lease{ID: "0123456789abcdef0123456789abcdef0123456789abcdef", SessionID: "abcdef0123456789abcdef0123456789abcdef0123456789", TenantID: "tenant_activity",
		WorkspaceID: "ws_activity", ResourcePlaneID: "rp_activity", MicroVMID: "vm_activity", AllocationGeneration: 4,
		ExpiresAt: now.Add(90 * time.Second), Now: now}
	renew := func(value Lease) error {
		_, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
			return struct{}{}, Renew(ctx, tx, value)
		})
		return err
	}
	if err := renew(lease); err != nil {
		t.Fatalf("create active lease: %v", err)
	}
	active, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return HasActiveLeases(ctx, tx, lease.WorkspaceID, lease.ResourcePlaneID, lease.MicroVMID, lease.AllocationGeneration, now.Add(time.Minute))
	})
	if err != nil || !active {
		t.Fatalf("lease not active before expiry: %v, %v", active, err)
	}
	if err := renew(lease); err != nil {
		t.Fatalf("idempotently renew lease: %v", err)
	}
	if candidates, err := listIdle(ctx, pool, now.Add(time.Minute)); err != nil || len(candidates) != 0 {
		t.Fatalf("active lease produced idle candidate: %+v, %v", candidates, err)
	}

	release := lease
	release.Now = now.Add(30 * time.Second)
	released, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return Release(ctx, tx, release)
	})
	if err != nil || !released {
		t.Fatalf("release lease: %v, %v", released, err)
	}
	releasedAgain, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return Release(ctx, tx, release)
	})
	if err != nil || !releasedAgain {
		t.Fatalf("idempotent release: %v, %v", releasedAgain, err)
	}
	candidates, err := listIdle(ctx, pool, now.Add(31*time.Second))
	if err != nil || len(candidates) != 1 || !candidates[0].IdleSince.Equal(release.Now) {
		t.Fatalf("released lease idle candidate = %+v, %v", candidates, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_workspace_idle_policies (tenant_id, enabled, grace_period_seconds, revision, updated_at)
		VALUES ('tenant_activity', TRUE, 300, 1, $1)`, now); err != nil {
		t.Fatalf("enable Tenant idle policy: %v", err)
	}
	idleNow := release.Now.Add(301 * time.Second)
	idleStats, err := RunIdleSweep(ctx, pool, idleNow, 10)
	if err != nil || idleStats.Checked != 1 || idleStats.Suspended != 1 {
		t.Fatalf("reconcile idle Workspace suspend: stats=%+v err=%v", idleStats, err)
	}
	var desired string
	var operationCount, eventCount, outboxCount, systemAuditCount int
	if err := pool.QueryRow(ctx, `SELECT desired_state FROM workspace_status WHERE workspace_id='ws_activity'`).Scan(&desired); err != nil || desired != "suspended" {
		t.Fatalf("idle suspension desired state=%q err=%v", desired, err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM operations WHERE workspace_id='ws_activity' AND type='suspend' AND status='pending'`).Scan(&operationCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM operation_events e JOIN operations o ON o.id=e.operation_id WHERE o.workspace_id='ws_activity' AND e.event_type='operation.requested'`).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM outbox_events o JOIN operations p ON p.id=o.aggregate_id WHERE p.workspace_id='ws_activity' AND o.event_type='operation.requested'`).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE tenant_id='tenant_activity' AND actor_user_id IS NULL
		AND actor_type='system' AND actor_name=$1 AND action='workspace.suspend'`, IdleSuspendSystemActor).Scan(&systemAuditCount); err != nil {
		t.Fatal(err)
	}
	if operationCount != 1 || eventCount != 1 || outboxCount != 1 || systemAuditCount != 1 {
		t.Fatalf("idle suspend side effects operation=%d event=%d outbox=%d systemAudit=%d", operationCount, eventCount, outboxCount, systemAuditCount)
	}
	duplicate, err := RunIdleSweep(ctx, pool, idleNow.Add(time.Minute), 10)
	if err != nil || duplicate.Suspended != 0 {
		t.Fatalf("idle suspend replay should be a no-op: stats=%+v err=%v", duplicate, err)
	}

	// The lease ID is permanently bound to its original runtime and cannot be
	// reused for a replacement allocation.
	replacement := lease
	replacement.TenantID = domain.TenantID("tenant_other")
	replacement.Now = now.Add(time.Minute)
	replacement.ExpiresAt = replacement.Now.Add(time.Minute)
	if err := renew(replacement); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("foreign tenant lease renew error=%v, want stale lease", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE resource_bindings SET external_id='vm_replacement', allocation_generation=5 WHERE workspace_id='ws_activity'`); err != nil {
		t.Fatal(err)
	}
	replacement = lease
	replacement.MicroVMID = "vm_replacement"
	replacement.AllocationGeneration = 5
	replacement.Now = now.Add(time.Minute)
	replacement.ExpiresAt = replacement.Now.Add(time.Minute)
	if err := renew(replacement); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("reused lease ID on replacement error=%v, want stale lease", err)
	}
	active, err = transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return HasActiveLeases(ctx, tx, lease.WorkspaceID, lease.ResourcePlaneID, lease.MicroVMID, lease.AllocationGeneration, now)
	})
	if err != nil || active {
		t.Fatalf("released lease still active: %v, %v", active, err)
	}
}

func TestConnectionLeaseLimitIsSharedAcrossConcurrentGatewayReplicas(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants (id, name, created_at) VALUES ('tenant_lease_limit', 'lease limit', $1)`, []any{now}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('user_lease_limit', 'lease-limit-subject', '', $1)`, []any{now}},
		{`INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES ('rp_lease_limit', 'aws', 'ap-northeast-1', '[]')`, nil},
		{`INSERT INTO workspaces (id, tenant_id, owner_id, name, runtime_class, image, created_at, updated_at)
			VALUES ('ws_lease_limit', 'tenant_lease_limit', 'user_lease_limit', 'lease-limit', 'standard', 'image:test', $1, $1)`, []any{now}},
		{`INSERT INTO placements (workspace_id, resource_plane_id, placed_at) VALUES ('ws_lease_limit', 'rp_lease_limit', $1)`, []any{now}},
		{`INSERT INTO workspace_status (workspace_id, desired_state, observed_state, updated_at)
			VALUES ('ws_lease_limit', 'running', 'running', $1)`, []any{now}},
		{`INSERT INTO resource_bindings (workspace_id, resource_plane_id, kind, external_id, created_at, allocation_generation)
			VALUES ('ws_lease_limit', 'rp_lease_limit', 'lambda_microvm', 'vm_lease_limit', $1, 1)`, []any{now}},
	} {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed Workspace lease limit database: %v\nquery: %s", err, statement.query)
		}
	}
	for i := 0; i < MaxActiveLeasesPerWorkspace*2+1; i++ {
		sessionID := fmt.Sprintf("%048x", i+1)
		if _, err := pool.Exec(ctx, `INSERT INTO sessions (id, user_id, tenant_id, workspace_id, resource_plane_id, permissions_json, expires_at, created_at)
			VALUES ($1, 'user_lease_limit', 'tenant_lease_limit', 'ws_lease_limit', 'rp_lease_limit', '["shell"]', $2, $3)`, sessionID, now.Add(10*time.Minute), now); err != nil {
			t.Fatalf("seed connection Session %s: %v", sessionID, err)
		}
	}

	const contenders = MaxActiveLeasesPerWorkspace * 2
	results := make(chan error, contenders)
	var start sync.WaitGroup
	start.Add(1)
	var workers sync.WaitGroup
	for i := 0; i < contenders; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			start.Wait()
			lease := Lease{ID: fmt.Sprintf("%048x", i+101), SessionID: fmt.Sprintf("%048x", i+1), TenantID: "tenant_lease_limit", WorkspaceID: "ws_lease_limit",
				ResourcePlaneID: "rp_lease_limit", MicroVMID: "vm_lease_limit", AllocationGeneration: 1,
				ExpiresAt: now.Add(90 * time.Second), Now: now}
			_, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
				return struct{}{}, Renew(ctx, tx, lease)
			})
			results <- err
		}(i)
	}
	start.Done()
	workers.Wait()
	close(results)
	accepted, limited := 0, 0
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrConnectionLimit):
			limited++
		default:
			t.Fatalf("concurrent lease admission failed unexpectedly: %v", err)
		}
	}
	if accepted != MaxActiveLeasesPerWorkspace || limited != contenders-MaxActiveLeasesPerWorkspace {
		t.Fatalf("concurrent admission accepted=%d limited=%d, want %d/%d", accepted, limited,
			MaxActiveLeasesPerWorkspace, contenders-MaxActiveLeasesPerWorkspace)
	}

	var renewal Lease
	if err := pool.QueryRow(ctx, `SELECT lease_id FROM workspace_connection_leases WHERE workspace_id='ws_lease_limit' ORDER BY lease_id LIMIT 1`).Scan(&renewal.ID); err != nil {
		t.Fatal(err)
	}
	renewal.TenantID, renewal.WorkspaceID, renewal.ResourcePlaneID = "tenant_lease_limit", "ws_lease_limit", "rp_lease_limit"
	if err := pool.QueryRow(ctx, `SELECT id FROM sessions WHERE active_connection_lease_id=$1`, renewal.ID).Scan(&renewal.SessionID); err != nil {
		t.Fatal(err)
	}
	renewal.MicroVMID, renewal.AllocationGeneration, renewal.Now = "vm_lease_limit", 1, now.Add(time.Second)
	renewal.ExpiresAt = renewal.Now.Add(90 * time.Second)
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		return struct{}{}, Renew(ctx, tx, renewal)
	}); err != nil {
		t.Fatalf("renew existing lease while at cap: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE workspace_connection_leases SET released_at=$2 WHERE lease_id=$1`, renewal.ID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	newLease := renewal
	newLease.ID = fmt.Sprintf("%048x", contenders+1)
	newLease.SessionID = fmt.Sprintf("%048x", contenders+1)
	newLease.Now = now.Add(3 * time.Second)
	newLease.ExpiresAt = newLease.Now.Add(90 * time.Second)
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		return struct{}{}, Renew(ctx, tx, newLease)
	}); err != nil {
		t.Fatalf("admit replacement lease after release: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET active_connection_lease_id=NULL, active_connection_expires_at=NULL WHERE workspace_id='ws_lease_limit'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_connection_leases SET released_at=COALESCE(released_at, $1) WHERE workspace_id='ws_lease_limit'`, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	sharedSession := fmt.Sprintf("%048x", contenders+1)
	replayResults := make(chan error, 2)
	var replayStart sync.WaitGroup
	replayStart.Add(1)
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			replayStart.Wait()
			lease := Lease{ID: fmt.Sprintf("%048x", 900+i), SessionID: sharedSession,
				TenantID: "tenant_lease_limit", WorkspaceID: "ws_lease_limit", ResourcePlaneID: "rp_lease_limit",
				MicroVMID: "vm_lease_limit", AllocationGeneration: 1, ExpiresAt: now.Add(6 * time.Second), Now: now.Add(5 * time.Second)}
			_, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
				return struct{}{}, Renew(ctx, tx, lease)
			})
			replayResults <- err
		}(i)
	}
	replayStart.Done()
	workers.Wait()
	close(replayResults)
	replayAccepted, replayDenied := 0, 0
	for err := range replayResults {
		switch {
		case err == nil:
			replayAccepted++
		case errors.Is(err, ErrSessionConnected):
			replayDenied++
		default:
			t.Fatalf("same-Session concurrent admission failed unexpectedly: %v", err)
		}
	}
	if replayAccepted != 1 || replayDenied != 1 {
		t.Fatalf("same-Session concurrent admission accepted=%d denied=%d, want 1/1", replayAccepted, replayDenied)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET active_connection_expires_at=$2 WHERE id=$1`, sharedSession, now.Add(5*time.Second)); err != nil {
		t.Fatalf("expire failed Gateway Session claim: %v", err)
	}
	expiredClaimLease := Lease{ID: fmt.Sprintf("%048x", 910), SessionID: sharedSession, TenantID: "tenant_lease_limit",
		WorkspaceID: "ws_lease_limit", ResourcePlaneID: "rp_lease_limit", MicroVMID: "vm_lease_limit", AllocationGeneration: 1,
		ExpiresAt: now.Add(2 * time.Minute), Now: now.Add(6 * time.Second)}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		return struct{}{}, Renew(ctx, tx, expiredClaimLease)
	}); err != nil {
		t.Fatalf("recover expired Gateway Session claim: %v", err)
	}
}

func listIdle(ctx context.Context, pool *pgxpool.Pool, before time.Time) ([]IdleCandidate, error) {
	return transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]IdleCandidate, error) {
		return ListIdleCandidates(ctx, tx, before, 10)
	})
}
