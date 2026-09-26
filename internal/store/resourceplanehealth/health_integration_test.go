//go:build integration

package resourceplanehealth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestHealthReportDefaultsFreshnessAndAuditAreTransactional(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('usr_health', 'health-subject', '', $1)`, []any{now}},
		{`INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES ('rp-health', 'aws', 'ap-northeast-1', '[]')`, nil},
	} {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed health test: %v", err)
		}
	}
	read := func() (Health, error) {
		return transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (Health, error) {
			return Get(ctx, tx, "rp-health", now)
		})
	}
	initial, err := read()
	if err != nil || initial.Status != Unknown || initial.EffectiveStatus != Healthy || initial.ReportedAt != nil {
		t.Fatalf("unreported health = %+v, %v", initial, err)
	}
	updated, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (Health, error) {
		return Put(ctx, tx, "rp-health", Healthy, "fresh", "usr_health", now)
	})
	if err != nil || updated.Status != Healthy || updated.EffectiveStatus != Healthy || updated.Reason != "fresh" {
		t.Fatalf("fresh health = %+v, %v", updated, err)
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM platform_audit_events WHERE action = 'resource_plane.health.update'`).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("platform audit rows = %d, error=%v", auditCount, err)
	}
	stale, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (Health, error) {
		return Get(ctx, tx, "rp-health", now.Add(FreshnessTTL+time.Second))
	})
	if err != nil || stale.EffectiveStatus != Degraded {
		t.Fatalf("stale healthy report = %+v, %v; want degraded", stale, err)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE platform_audit_events`); err != nil {
		t.Fatalf("remove audit sink: %v", err)
	}
	_, err = transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (Health, error) {
		return Put(ctx, tx, "rp-health", Unhealthy, "must roll back", "usr_health", now.Add(time.Minute))
	})
	if err == nil {
		t.Fatal("health update must fail when audit append fails")
	}
	current, err := read()
	if err != nil || current.Status != Healthy || current.Reason != "fresh" {
		t.Fatalf("health change should roll back with audit failure: %+v, %v", current, err)
	}
	var operatorEvents int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM resource_plane_health_events WHERE resource_plane_id = 'rp-health' AND report_source = 'operator'`).Scan(&operatorEvents); err != nil || operatorEvents != 1 {
		t.Fatalf("operator history should roll back with failed audit: events=%d error=%v", operatorEvents, err)
	}
	automated, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (Health, error) {
		return PutAutomated(ctx, tx, "rp-health", Degraded, "controller queue check failed", now.Add(2*time.Minute))
	})
	if err != nil || automated.Status != Degraded || automated.ReportSource != AutomatedReport || automated.Reason != "controller queue check failed" {
		t.Fatalf("automated health report = %+v, %v", automated, err)
	}
	var automatedEvents int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM resource_plane_health_events WHERE resource_plane_id = 'rp-health' AND report_source = 'automated'`).Scan(&automatedEvents); err != nil || automatedEvents != 1 {
		t.Fatalf("automated health report events = %d, error=%v", automatedEvents, err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (Health, error) {
		return Get(ctx, tx, "rp-missing", now)
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Resource Plane error = %v, want ErrNotFound", err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (Health, error) {
		return PutAutomated(ctx, tx, "rp-missing", Healthy, "probe recovered", now)
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("automated report for missing Resource Plane error = %v, want ErrNotFound", err)
	}
}
