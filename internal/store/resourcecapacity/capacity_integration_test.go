//go:build integration

package resourcecapacity

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

func TestComputeCapacityReserveRecordAndRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants (id, name, created_at) VALUES ('ten_capacity', 'capacity', $1)`, []any{now}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('usr_capacity', 'capacity-subject', '', $1)`, []any{now}},
		{`INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES
			('rp_unbounded', 'aws', 'ap-northeast-1', '[]'),
			('rp_compute', 'aws', 'ap-northeast-1', '[]'),
			('rp_underflow', 'aws', 'ap-northeast-1', '[]')`, nil},
		{`INSERT INTO workspaces (id, tenant_id, owner_id, name, runtime_class, image, created_at, updated_at) VALUES
			('ws_capacity', 'ten_capacity', 'usr_capacity', 'capacity', 'standard', 'image:test', $1, $1),
			('ws_underflow', 'ten_capacity', 'usr_capacity', 'underflow', 'standard', 'image:test', $1, $1)`, []any{now}},
		{`INSERT INTO resource_plane_capacities
			(resource_plane_id, max_workspaces, reserved_workspaces, updated_at, max_cpu_millicores, max_memory_mib, reserved_cpu_millicores, reserved_memory_mib)
			VALUES ('rp_compute', 4, 0, $1, 1000, 1024, 0, 0), ('rp_underflow', 4, 0, $1, 1000, 1024, 0, 0)`, []any{now}},
		{`INSERT INTO runtime_class_resources (runtime_class, cpu_millicores, memory_mib, updated_at) VALUES ('standard', 750, 800, $1)`, []any{now}},
	} {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed capacity store: %v\nquery: %s", err, statement.query)
		}
	}

	reserved, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return Reserve(ctx, tx, "rp_unbounded", "", now)
	})
	if err != nil || !reserved {
		t.Fatalf("unconfigured Plane reservation = %v, %v; want unbounded", reserved, err)
	}

	reserved, err = transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
		reserved, err := Reserve(ctx, tx, "rp_compute", " STANDARD ", now)
		if err != nil || !reserved {
			return reserved, err
		}
		if err := RecordWorkspace(ctx, tx, "ws_capacity", "rp_compute", " STANDARD ", now); err != nil {
			return false, err
		}
		return true, nil
	})
	if err != nil || !reserved {
		t.Fatalf("first compute reservation = %v, %v; want success", reserved, err)
	}
	reserved, err = transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return Reserve(ctx, tx, "rp_compute", "standard", now.Add(time.Minute))
	})
	if err != nil || reserved {
		t.Fatalf("second compute reservation = %v, %v; want capacity rejection", reserved, err)
	}
	var reservedWorkspaces, reservedCPU, reservedMemory int64
	if err := pool.QueryRow(ctx, `SELECT reserved_workspaces, reserved_cpu_millicores, reserved_memory_mib
		FROM resource_plane_capacities WHERE resource_plane_id = 'rp_compute'`).Scan(&reservedWorkspaces, &reservedCPU, &reservedMemory); err != nil {
		t.Fatalf("read reserved counters: %v", err)
	}
	if reservedWorkspaces != 1 || reservedCPU != 750 || reservedMemory != 800 {
		t.Fatalf("reserved counters = workspaces %d, CPU %d, memory %d; want 1, 750, 800", reservedWorkspaces, reservedCPU, reservedMemory)
	}

	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		return struct{}{}, ReleaseWorkspace(ctx, tx, "ws_capacity", now.Add(2*time.Minute))
	}); err != nil {
		t.Fatalf("release Workspace capacity: %v", err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		return struct{}{}, ReleaseWorkspace(ctx, tx, "ws_capacity", now.Add(3*time.Minute))
	}); err != nil {
		t.Fatalf("duplicate release should be a no-op: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT reserved_workspaces, reserved_cpu_millicores, reserved_memory_mib
		FROM resource_plane_capacities WHERE resource_plane_id = 'rp_compute'`).Scan(&reservedWorkspaces, &reservedCPU, &reservedMemory); err != nil {
		t.Fatalf("read released counters: %v", err)
	}
	if reservedWorkspaces != 0 || reservedCPU != 0 || reservedMemory != 0 {
		t.Fatalf("released counters = workspaces %d, CPU %d, memory %d; want all zero", reservedWorkspaces, reservedCPU, reservedMemory)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO resource_plane_reservations
		(workspace_id, resource_plane_id, reserved_at, runtime_class, cpu_millicores, memory_mib)
		VALUES ('ws_underflow', 'rp_underflow', $1, 'standard', 100, 200)`, now); err != nil {
		t.Fatalf("seed corrupted reservation: %v", err)
	}
	_, err = transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		return struct{}{}, ReleaseWorkspace(ctx, tx, "ws_underflow", now)
	})
	if !errors.Is(err, ErrReservationCounterUnderflow) {
		t.Fatalf("counter underflow error = %v, want ErrReservationCounterUnderflow", err)
	}
	var reservationCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM resource_plane_reservations WHERE workspace_id = 'ws_underflow'`).Scan(&reservationCount); err != nil || reservationCount != 1 {
		t.Fatalf("failed release must roll back reservation deletion: rows=%d error=%v", reservationCount, err)
	}
}

func TestReserveRequiresRuntimeClassProfileForComputeLimits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES ('rp_missing_profile', 'aws', 'ap-northeast-1', '[]')`); err != nil {
		t.Fatalf("seed Resource Plane: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO resource_plane_capacities (resource_plane_id, max_workspaces, reserved_workspaces, updated_at, max_cpu_millicores) VALUES ('rp_missing_profile', 2, 0, now(), 1000)`); err != nil {
		t.Fatalf("seed compute capacity: %v", err)
	}
	_, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return Reserve(ctx, tx, "rp_missing_profile", "unconfigured", time.Now().UTC())
	})
	if err == nil {
		t.Fatal("capacity with CPU/RAM limit must require a Runtime Class profile")
	}
}
