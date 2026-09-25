//go:build integration

package reconciler

import (
	"context"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/operations"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestRunOnceCreatesCorrectiveOperationsAndHonorsCooldown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	base := time.Now().UTC().Truncate(time.Microsecond)
	seed := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants (id, name, created_at) VALUES ('tenant_rec', 'reconciler', $1)`, []any{base}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('user_rec', 'subject-rec', '', $1)`, []any{base}},
		{`INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES ('rp_rec', 'aws', 'ap-northeast-1', '["microvm"]')`, nil},
		{`INSERT INTO resource_plane_status (resource_plane_id, status, updated_at) VALUES ('rp_rec', 'active', $1)`, []any{base}},
		{`INSERT INTO workspaces (id, tenant_id, owner_id, name, runtime_class, image, created_at, updated_at) VALUES ('ws_rec', 'tenant_rec', 'user_rec', 'reconcile-me', 'standard', 'image', $1, $1)`, []any{base}},
		{`INSERT INTO workspace_status (workspace_id, desired_state, observed_state, updated_at) VALUES ('ws_rec', 'running', 'pending', $1)`, []any{base}},
		{`INSERT INTO placements (workspace_id, resource_plane_id, placed_at) VALUES ('ws_rec', 'rp_rec', $1)`, []any{base}},
	}
	for _, statement := range seed {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed Reconciler test: %v", err)
		}
	}
	now := base.Add(time.Second)
	worker, err := New(StoreAdapter{Pool: pool, Policy: transaction.DefaultPolicy()}, Config{
		FailureDelay: time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := worker.RunOnce(ctx)
	if err != nil || stats.Created != 1 || stats.Checked != 1 {
		t.Fatalf("initial mismatch should create ensure_running: stats=%+v error=%v", stats, err)
	}
	var firstID domain.OperationID
	if err := pool.QueryRow(ctx, `SELECT id FROM operations WHERE workspace_id = 'ws_rec'`).Scan(&firstID); err != nil {
		t.Fatalf("read corrective Operation: %v", err)
	}
	stats, err = worker.RunOnce(ctx)
	if err != nil || stats.Created != 0 || stats.Checked != 0 {
		t.Fatalf("pending Operation must prevent duplicates: stats=%+v error=%v", stats, err)
	}

	provisioning := domain.ObservedWorkspaceProvisioning
	if _, err := operations.Transition(ctx, pool, transaction.DefaultPolicy(), operations.TransitionInput{
		OperationID: firstID, ExpectedStatus: domain.OperationPending, NextStatus: domain.OperationRunning,
		EventType: "operation.started", ObservedState: &provisioning, OccurredAt: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("start corrective Operation: %v", err)
	}
	failed := domain.ObservedWorkspaceFailed
	if _, err := operations.Transition(ctx, pool, transaction.DefaultPolicy(), operations.TransitionInput{
		OperationID: firstID, ExpectedStatus: domain.OperationRunning, NextStatus: domain.OperationFailed,
		EventType: "operation.failed", ObservedState: &failed, ErrorCode: "runtime_failure", OccurredAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("fail corrective Operation: %v", err)
	}
	now = now.Add(30 * time.Second)
	stats, err = worker.RunOnce(ctx)
	if err != nil || stats.Checked != 0 {
		t.Fatalf("recent terminal Operation must be under cooldown: stats=%+v error=%v", stats, err)
	}
	now = now.Add(2 * time.Minute)
	stats, err = worker.RunOnce(ctx)
	if err != nil || stats.Created != 1 {
		t.Fatalf("persistent mismatch after cooldown should be retried: stats=%+v error=%v", stats, err)
	}
	var operationCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM operations WHERE workspace_id = 'ws_rec'`).Scan(&operationCount); err != nil || operationCount != 2 {
		t.Fatalf("expected one retry Operation, count=%d error=%v", operationCount, err)
	}
	var outboxCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM outbox_events WHERE aggregate_id IN (SELECT id FROM operations WHERE workspace_id = 'ws_rec')`).Scan(&outboxCount); err != nil || outboxCount != 2 {
		t.Fatalf("each corrective Operation should have an Outbox command: count=%d error=%v", outboxCount, err)
	}
}

func TestRunOnceFailsStaleOperationsWithoutChangingObservedState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	base := time.Now().UTC().Truncate(time.Microsecond)
	old := base.Add(-2 * time.Hour)
	seed := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants (id, name, created_at) VALUES ('tenant_timeout', 'timeout', $1)`, []any{old}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('user_timeout', 'subject-timeout', '', $1)`, []any{old}},
		{`INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES ('rp_timeout', 'aws', 'ap-northeast-1', '["microvm"]')`, nil},
		{`INSERT INTO resource_plane_status (resource_plane_id, status, updated_at) VALUES ('rp_timeout', 'active', $1)`, []any{old}},
		{`INSERT INTO workspaces (id, tenant_id, owner_id, name, runtime_class, image, created_at, updated_at) VALUES
			('ws_pending_old', 'tenant_timeout', 'user_timeout', 'pending-old', 'standard', 'image', $1, $1),
			('ws_running_old', 'tenant_timeout', 'user_timeout', 'running-old', 'standard', 'image', $1, $1),
			('ws_pending_fresh', 'tenant_timeout', 'user_timeout', 'pending-fresh', 'standard', 'image', $2, $2)`, []any{old, base.Add(-5 * time.Minute)}},
		{`INSERT INTO workspace_status (workspace_id, desired_state, observed_state, updated_at) VALUES
			('ws_pending_old', 'running', 'pending', $1),
			('ws_running_old', 'running', 'pending', $1),
			('ws_pending_fresh', 'running', 'pending', $2)`, []any{old, base.Add(-5 * time.Minute)}},
		{`INSERT INTO placements (workspace_id, resource_plane_id, placed_at) VALUES
			('ws_pending_old', 'rp_timeout', $1), ('ws_running_old', 'rp_timeout', $1), ('ws_pending_fresh', 'rp_timeout', $1)`, []any{old}},
		{`INSERT INTO operations (id, tenant_id, workspace_id, resource_plane_id, type, status, idempotency_key, request_hash, attempt, created_at, updated_at) VALUES
			('op_pending_old', 'tenant_timeout', 'ws_pending_old', 'rp_timeout', 'ensure_running', 'pending', 'pending-old', 'hash', 0, $1, $1),
			('op_running_old', 'tenant_timeout', 'ws_running_old', 'rp_timeout', 'ensure_running', 'running', 'running-old', 'hash', 1, $1, $1),
			('op_pending_fresh', 'tenant_timeout', 'ws_pending_fresh', 'rp_timeout', 'ensure_running', 'pending', 'pending-fresh', 'hash', 0, $2, $2)`, []any{old, base.Add(-5 * time.Minute)}},
	}
	for _, statement := range seed {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed Operation timeout test: %v", err)
		}
	}

	now := base
	worker, err := New(StoreAdapter{Pool: pool, Policy: transaction.DefaultPolicy()}, Config{
		BatchSize: 10, FailureDelay: time.Minute, OperationTimeout: 15 * time.Minute,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := worker.RunOnce(ctx)
	if err != nil || stats.TimedOut != 2 || stats.Created != 0 {
		t.Fatalf("old pending/running Operations should expire but respect terminal cooldown: stats=%+v error=%v", stats, err)
	}
	var status, errorCode string
	if err := pool.QueryRow(ctx, `SELECT status, error_code FROM operations WHERE id = 'op_pending_old'`).Scan(&status, &errorCode); err != nil || status != "failed" || errorCode != "operation_timeout" {
		t.Fatalf("stale pending Operation status=(%s,%s), error=%v", status, errorCode, err)
	}
	if err := pool.QueryRow(ctx, `SELECT observed_state FROM workspace_status WHERE workspace_id = 'ws_pending_old'`).Scan(&status); err != nil || status != string(domain.ObservedWorkspacePending) {
		t.Fatalf("timeout must not invent an observed Runtime state, observed=%s error=%v", status, err)
	}
	var eventType string
	if err := pool.QueryRow(ctx, `SELECT event_type FROM operation_events WHERE operation_id = 'op_running_old' ORDER BY sequence DESC LIMIT 1`).Scan(&eventType); err != nil || eventType != "operation.timed_out" {
		t.Fatalf("stale Operation should append timeout event, event=%s error=%v", eventType, err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM operations WHERE id = 'op_pending_fresh'`).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("fresh pending Operation should remain active, status=%s error=%v", status, err)
	}

	now = now.Add(2 * time.Minute)
	stats, err = worker.RunOnce(ctx)
	if err != nil || stats.Created != 2 || stats.TimedOut != 0 {
		t.Fatalf("persistent Workspace mismatches should reconcile after timeout cooldown: stats=%+v error=%v", stats, err)
	}
}
