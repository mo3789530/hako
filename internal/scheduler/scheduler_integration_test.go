//go:build integration

package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestSelectFiltersAndBalancesEligibleResourcePlanes(t *testing.T) {
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
		{`INSERT INTO tenants (id, name, created_at) VALUES ('tenant_sched', 'scheduler', $1)`, []any{now}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('user_sched', 'subject-sched', '', $1)`, []any{now}},
		{`INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES ('rp_a', 'aws', 'ap-northeast-1', '["microvm"]')`, nil},
		{`INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES ('rp_b', 'aws', 'ap-northeast-1', '["microvm","private-network"]')`, nil},
		{`INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES ('rp_c', 'aws', 'ap-south-1', '["microvm","private-network"]')`, nil},
		{`INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES ('rp_disabled', 'aws', 'ap-northeast-1', '["microvm","private-network"]')`, nil},
		{`INSERT INTO resource_plane_status (resource_plane_id, status, updated_at) VALUES ('rp_a', 'active', $1), ('rp_b', 'active', $1), ('rp_c', 'active', $1), ('rp_disabled', 'disabled', $1)`, []any{now}},
		{`INSERT INTO workspaces (id, tenant_id, owner_id, name, runtime_class, image, created_at, updated_at) VALUES ('ws_a', 'tenant_sched', 'user_sched', 'a', 'standard', 'image', $1, $1), ('ws_b', 'tenant_sched', 'user_sched', 'b', 'standard', 'image', $1, $1), ('ws_b_deleted', 'tenant_sched', 'user_sched', 'b-deleted', 'standard', 'image', $1, $1)`, []any{now}},
		{`INSERT INTO workspace_status (workspace_id, desired_state, observed_state, updated_at) VALUES ('ws_a', 'running', 'running', $1), ('ws_b', 'running', 'running', $1), ('ws_b_deleted', 'deleted', 'deleted', $1)`, []any{now}},
		{`INSERT INTO placements (workspace_id, resource_plane_id, placed_at) VALUES ('ws_a', 'rp_a', $1), ('ws_b', 'rp_b', $1), ('ws_b_deleted', 'rp_b', $1)`, []any{now}},
	}
	for _, item := range seed {
		if _, err := pool.Exec(ctx, item.query, item.args...); err != nil {
			t.Fatalf("seed scheduler test: %v", err)
		}
	}

	var selected domain.ResourcePlaneID
	_, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		var err error
		selected, err = Select(ctx, tx, Policy{Region: "ap-northeast-1", RequiredCapabilities: []string{"microvm", "private-network"}})
		return struct{}{}, err
	})
	if err != nil || selected != "rp_b" {
		t.Fatalf("expected least-loaded eligible Resource Plane in required region rp_b, got %q (error=%v)", selected, err)
	}
	_, err = transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		selected, err = Select(ctx, tx, Policy{RequiredCapabilities: []string{"microvm", "private-network"}})
		return struct{}{}, err
	})
	if err != nil || selected != "rp_c" {
		t.Fatalf("deleted Workspaces must not count as active placement load; expected rp_c, got %q (error=%v)", selected, err)
	}
	_, err = transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		_, err := Select(ctx, tx, Policy{Region: "eu-west-1", RequiredCapabilities: []string{"microvm"}})
		return struct{}{}, err
	})
	if !errors.Is(err, ErrNoEligibleResourcePlane) {
		t.Fatalf("expected no eligible Resource Plane, got %v", err)
	}
}

func TestSelectDowngradesStaleHealthAndKeepsStaleUnhealthyExcluded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC()
	stale := now.Add(-HealthFreshnessTTL - time.Minute)
	seed := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES
			('rp_fresh', 'aws', 'ap-northeast-1', '["microvm"]'),
			('rp_stale', 'aws', 'ap-northeast-1', '["microvm"]'),
			('rp_unhealthy', 'aws', 'ap-northeast-1', '["microvm"]')`, nil},
		{`INSERT INTO resource_plane_status (resource_plane_id, status, updated_at) VALUES
			('rp_fresh', 'active', $1), ('rp_stale', 'active', $1), ('rp_unhealthy', 'active', $1)`, []any{now}},
		{`INSERT INTO resource_plane_health (resource_plane_id, status, reason, updated_at) VALUES
			('rp_fresh', 'healthy', 'fresh report', $1),
			('rp_stale', 'healthy', 'old report', $2),
			('rp_unhealthy', 'unhealthy', 'old unhealthy report', $2)`, []any{now, stale}},
	}
	for _, item := range seed {
		if _, err := pool.Exec(ctx, item.query, item.args...); err != nil {
			t.Fatalf("seed health TTL test: %v", err)
		}
	}

	var selected domain.ResourcePlaneID
	_, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		var err error
		selected, err = Select(ctx, tx, Policy{RequiredCapabilities: []string{"microvm"}})
		return struct{}{}, err
	})
	if err != nil || selected != "rp_fresh" {
		t.Fatalf("fresh healthy Plane should outrank stale healthy Plane, got %q (error=%v)", selected, err)
	}

	_, err = transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		selected, err = Select(ctx, tx, Policy{ResourcePlaneID: "rp_stale", RequiredCapabilities: []string{"microvm"}})
		return struct{}{}, err
	})
	if err != nil || selected != "rp_stale" {
		t.Fatalf("stale healthy Plane should remain degraded fallback, got %q (error=%v)", selected, err)
	}

	_, err = transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		_, err := Select(ctx, tx, Policy{ResourcePlaneID: "rp_unhealthy", RequiredCapabilities: []string{"microvm"}})
		return struct{}{}, err
	})
	if !errors.Is(err, ErrNoEligibleResourcePlane) {
		t.Fatalf("stale unhealthy Plane must remain excluded, got %v", err)
	}
}

func TestSelectHonorsTenantCostCeilingAndMinimumIsolation(t *testing.T) {
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
		{`INSERT INTO tenants (id, name, created_at) VALUES ('tenant_tiers', 'tiers', $1)`, []any{now}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('user_tiers', 'subject-tiers', '', $1)`, []any{now}},
		{`INSERT INTO resource_planes (id, provider, region, capabilities_json, cost_tier, isolation_tier) VALUES
			('rp_low_busy', 'aws', 'ap-northeast-1', '["microvm"]', 'low', 'dedicated'),
			('rp_standard_empty', 'aws', 'ap-northeast-1', '["microvm"]', 'standard', 'dedicated'),
			('rp_high_isolated', 'aws', 'ap-northeast-1', '["microvm"]', 'high', 'isolated'),
			('rp_low_shared', 'aws', 'ap-northeast-1', '["microvm"]', 'low', 'shared')`, nil},
		{`INSERT INTO resource_plane_status (resource_plane_id, status, updated_at) VALUES
			('rp_low_busy', 'active', $1), ('rp_standard_empty', 'active', $1),
			('rp_high_isolated', 'active', $1), ('rp_low_shared', 'active', $1)`, []any{now}},
		{`INSERT INTO workspaces (id, tenant_id, owner_id, name, runtime_class, image, created_at, updated_at) VALUES ('ws_low_busy', 'tenant_tiers', 'user_tiers', 'busy', 'standard', 'image', $1, $1)`, []any{now}},
		{`INSERT INTO workspace_status (workspace_id, desired_state, observed_state, updated_at) VALUES ('ws_low_busy', 'running', 'running', $1)`, []any{now}},
		{`INSERT INTO placements (workspace_id, resource_plane_id, placed_at) VALUES ('ws_low_busy', 'rp_low_busy', $1)`, []any{now}},
		{`INSERT INTO tenant_placement_policies (tenant_id, allowed_regions_json, resource_plane_ids_json, required_capabilities_json, max_cost_tier, minimum_isolation_tier, updated_at)
			VALUES ('tenant_tiers', '[]', '[]', '[]', 'standard', 'dedicated', $1)`, []any{now}},
	}
	for _, item := range seed {
		if _, err := pool.Exec(ctx, item.query, item.args...); err != nil {
			t.Fatalf("seed cost/isolation scheduler test: %v", err)
		}
	}

	var selected domain.ResourcePlaneID
	_, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		var err error
		selected, err = Select(ctx, tx, Policy{TenantID: "tenant_tiers", RequiredCapabilities: []string{"microvm"}})
		return struct{}{}, err
	})
	if err != nil || selected != "rp_low_busy" {
		t.Fatalf("lowest-cost Plane satisfying the isolation floor should win before load balancing; got %q (error=%v)", selected, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_placement_policies SET max_cost_tier = 'low', minimum_isolation_tier = 'isolated' WHERE tenant_id = 'tenant_tiers'`); err != nil {
		t.Fatalf("tighten tenant Cost/Isolation requirements: %v", err)
	}
	_, err = transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		_, err := Select(ctx, tx, Policy{TenantID: "tenant_tiers", RequiredCapabilities: []string{"microvm"}})
		return struct{}{}, err
	})
	if !errors.Is(err, ErrNoEligibleResourcePlane) {
		t.Fatalf("no Plane satisfies low-cost and isolated requirements; got %v", err)
	}
}
