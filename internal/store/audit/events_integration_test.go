//go:build integration

package audit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestAppendValidatesAndPersistsActorAuditEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, created_at) VALUES ('tenant_audit', 'Audit', $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('usr_audit', 'audit-sub', '', $1)`, now); err != nil {
		t.Fatal(err)
	}

	if err := Append(ctx, nil, Event{}); err == nil {
		t.Fatal("nil transaction and incomplete event should be rejected")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	invalid := Event{TenantID: "tenant_audit", ActorID: "usr_audit", Action: "workspace.create", TargetType: "workspace", TargetID: "ws_audit", Details: json.RawMessage(`not-json`)}
	if err := Append(ctx, tx, invalid); err == nil {
		t.Fatal("invalid details should be rejected")
	}
	failedTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := Append(ctx, failedTx, Event{
		TenantID: "tenant_audit", ActorID: "usr_missing", Action: "workspace.create",
		TargetType: "workspace", TargetID: "ws_missing_actor", OccurredAt: now,
	}); err == nil {
		_ = failedTx.Rollback(ctx)
		t.Fatal("database foreign-key failure should be returned")
	}
	_ = failedTx.Rollback(ctx)
	event := Event{
		TenantID: domain.TenantID("tenant_audit"), ActorID: domain.UserID("usr_audit"),
		Action: "workspace.create", TargetType: "workspace", TargetID: "ws_audit",
		Details: json.RawMessage(`{"operation_id":"op_audit"}`), OccurredAt: now,
	}
	if err := Append(ctx, tx, event); err != nil {
		t.Fatalf("append valid audit event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var got Event
	var details string
	if err := pool.QueryRow(ctx, `SELECT tenant_id, actor_user_id, action, target_type, target_id, details_json, occurred_at FROM audit_events WHERE tenant_id = $1 AND target_id = $2`, event.TenantID, event.TargetID).Scan(
		&got.TenantID, &got.ActorID, &got.Action, &got.TargetType, &got.TargetID, &details, &got.OccurredAt,
	); err != nil {
		t.Fatalf("read persisted audit event: %v", err)
	}
	if got.TenantID != event.TenantID || got.ActorID != event.ActorID || got.Action != event.Action || got.TargetType != event.TargetType || got.TargetID != event.TargetID || details != string(event.Details) || !got.OccurredAt.Equal(now.Truncate(time.Microsecond)) {
		t.Fatalf("persisted event mismatch: got=%+v details=%s want=%+v details=%s", got, details, event, event.Details)
	}
}
