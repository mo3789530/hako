//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/authz"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/scheduler"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/operations"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/users"
	"github.com/mo3789530/hako/internal/store/workspaces"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestWorkspaceCreateOutboxAndOperationLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)

	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply embedded schema migrations: %v", err)
	}
	seedPostgres(ctx, t, pool)
	policy := transaction.DefaultPolicy()
	input := workspaces.CreateInput{
		TenantID:        "tenant_test",
		OwnerID:         "user_test",
		Name:            "api-dev",
		RuntimeClass:    "standard",
		Image:           "hako/go:latest",
		ResourcePlaneID: "rp_test",
		IdempotencyKey:  "create-api-dev-001",
	}

	created, err := workspaces.Create(ctx, pool, input, policy)
	if err != nil {
		t.Fatalf("create Workspace: %v", err)
	}
	if created.Status.DesiredState != domain.DesiredWorkspaceRunning || created.Status.ObservedState != domain.ObservedWorkspacePending {
		t.Fatalf("unexpected initial Workspace status: %+v", created.Status)
	}
	if created.Operation.Status != domain.OperationPending || created.Operation.Type != domain.OperationEnsureRunning {
		t.Fatalf("unexpected initial Operation: %+v", created.Operation)
	}

	var outboxCount, eventCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1`, created.Operation.ID).Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox messages: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM operation_events WHERE operation_id = $1`, created.Operation.ID).Scan(&eventCount); err != nil {
		t.Fatalf("count initial operation events: %v", err)
	}
	if outboxCount != 1 || eventCount != 1 {
		t.Fatalf("create should atomically persist one outbox row and one event; got outbox=%d events=%d", outboxCount, eventCount)
	}

	replayed, err := workspaces.Create(ctx, pool, input, policy)
	if err != nil {
		t.Fatalf("replay idempotent Workspace create: %v", err)
	}
	if replayed.Workspace.ID != created.Workspace.ID || replayed.Operation.ID != created.Operation.ID {
		t.Fatalf("idempotent replay returned different resources: first=%+v replay=%+v", created, replayed)
	}
	if _, err := workspaces.Create(ctx, pool, workspaces.CreateInput{
		TenantID: input.TenantID, OwnerID: input.OwnerID, Name: input.Name,
		RuntimeClass: input.RuntimeClass, Image: "hako/go:changed", ResourcePlaneID: input.ResourcePlaneID,
		IdempotencyKey: input.IdempotencyKey,
	}, policy); !errors.Is(err, operations.ErrIdempotencyConflict) {
		t.Fatalf("reuse key with different request should conflict; got %v", err)
	}
	if _, err := workspaces.Create(ctx, pool, workspaces.CreateInput{
		TenantID: input.TenantID, OwnerID: input.OwnerID, Name: input.Name,
		RuntimeClass: input.RuntimeClass, Image: input.Image, ResourcePlaneID: input.ResourcePlaneID,
		IdempotencyKey: "create-api-dev-002",
	}, policy); !errors.Is(err, workspaces.ErrWorkspaceNameConflict) {
		t.Fatalf("duplicate tenant Workspace name should conflict; got %v", err)
	}

	provisioning := domain.ObservedWorkspaceProvisioning
	started, err := operations.Transition(ctx, pool, policy, operations.TransitionInput{
		OperationID: created.Operation.ID, ExpectedStatus: domain.OperationPending,
		NextStatus: domain.OperationRunning, EventType: "operation.started",
		Payload: json.RawMessage(`{"controller":"test"}`), ObservedState: &provisioning,
	})
	if err != nil {
		t.Fatalf("transition Operation to running: %v", err)
	}
	if started.Attempt != 1 || started.Status != domain.OperationRunning {
		t.Fatalf("running transition should increment attempt: %+v", started)
	}
	if _, err := operations.Transition(ctx, pool, policy, operations.TransitionInput{
		OperationID: created.Operation.ID, ExpectedStatus: domain.OperationPending,
		NextStatus: domain.OperationRunning, EventType: "operation.started",
	}); !errors.Is(err, operations.ErrTransitionConflict) {
		t.Fatalf("duplicate/stale transition should conflict; got %v", err)
	}

	running := domain.ObservedWorkspaceRunning
	succeeded, err := operations.Transition(ctx, pool, policy, operations.TransitionInput{
		OperationID: created.Operation.ID, ExpectedStatus: domain.OperationRunning,
		NextStatus: domain.OperationSucceeded, EventType: "operation.succeeded",
		ObservedState: &running,
	})
	if err != nil {
		t.Fatalf("transition Operation to succeeded: %v", err)
	}
	if succeeded.Status != domain.OperationSucceeded || succeeded.Attempt != 1 {
		t.Fatalf("unexpected succeeded Operation: %+v", succeeded)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin event history read: %v", err)
	}
	defer tx.Rollback(ctx)
	events, err := operations.ListEvents(ctx, tx, created.Operation.ID)
	if err != nil {
		t.Fatalf("list operation events: %v", err)
	}
	if len(events) != 3 || events[0].Sequence != 1 || events[1].Sequence != 2 || events[2].Sequence != 3 {
		t.Fatalf("expected contiguous event sequence 1..3, got %+v", events)
	}
	if events[0].Type != "operation.created" || events[1].Type != "operation.started" || events[2].Type != "operation.succeeded" {
		t.Fatalf("unexpected operation event history: %+v", events)
	}
	var observed domain.ObservedWorkspaceState
	if err := pool.QueryRow(ctx, `SELECT observed_state FROM workspace_status WHERE workspace_id = $1`, created.Workspace.ID).Scan(&observed); err != nil {
		t.Fatalf("read Workspace observed state: %v", err)
	}
	if observed != domain.ObservedWorkspaceRunning {
		t.Fatalf("expected observed Workspace running, got %q", observed)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1`, created.Operation.ID).Scan(&outboxCount); err != nil {
		t.Fatalf("count final outbox rows: %v", err)
	}
	if outboxCount != 1 {
		t.Fatalf("idempotent replay must not enqueue duplicate work, got %d outbox rows", outboxCount)
	}
}

func TestTenantWorkspaceQuotaIsConcurrentAndReleasedOnDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply embedded schema migrations: %v", err)
	}
	seedPostgres(ctx, t, pool)

	type result struct {
		created workspaces.CreateResult
		err     error
	}
	const requests = workspaces.MaxWorkspacesPerTenant + 6
	results := make(chan result, requests)
	for i := 0; i < requests; i++ {
		go func(i int) {
			created, err := workspaces.Create(ctx, pool, workspaces.CreateInput{
				TenantID: "tenant_test", OwnerID: "user_test", Name: fmt.Sprintf("quota-%02d", i),
				RuntimeClass: "standard", Image: "hako/go:latest", ResourcePlaneID: "rp_test",
				IdempotencyKey: fmt.Sprintf("quota-create-%02d", i),
			}, transaction.DefaultPolicy())
			results <- result{created: created, err: err}
		}(i)
	}

	created := make([]workspaces.CreateResult, 0, workspaces.MaxWorkspacesPerTenant)
	quotaErrors := 0
	for i := 0; i < requests; i++ {
		result := <-results
		if errors.Is(result.err, workspaces.ErrWorkspaceQuotaExceeded) {
			quotaErrors++
			continue
		}
		if result.err != nil {
			t.Fatalf("concurrent Workspace create failed unexpectedly: %v", result.err)
		}
		created = append(created, result.created)
	}
	if len(created) != workspaces.MaxWorkspacesPerTenant || quotaErrors != requests-workspaces.MaxWorkspacesPerTenant {
		t.Fatalf("quota admitted %d Workspaces and rejected %d requests; want %d admitted", len(created), quotaErrors, workspaces.MaxWorkspacesPerTenant)
	}
	var slotCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workspace_quota_slots WHERE tenant_id = $1`, "tenant_test").Scan(&slotCount); err != nil {
		t.Fatalf("count allocated quota slots: %v", err)
	}
	if slotCount != workspaces.MaxWorkspacesPerTenant {
		t.Fatalf("quota slot count is %d, want %d", slotCount, workspaces.MaxWorkspacesPerTenant)
	}

	deleting := domain.ObservedWorkspaceDeleting
	if _, err := operations.Transition(ctx, pool, transaction.DefaultPolicy(), operations.TransitionInput{
		OperationID: created[0].Operation.ID, ExpectedStatus: domain.OperationPending,
		NextStatus: domain.OperationRunning, EventType: "operation.started", ObservedState: &deleting,
	}); err != nil {
		t.Fatalf("start Workspace deletion: %v", err)
	}
	deleted := domain.ObservedWorkspaceDeleted
	if _, err := operations.Transition(ctx, pool, transaction.DefaultPolicy(), operations.TransitionInput{
		OperationID: created[0].Operation.ID, ExpectedStatus: domain.OperationRunning,
		NextStatus: domain.OperationSucceeded, EventType: "operation.succeeded", ObservedState: &deleted,
	}); err != nil {
		t.Fatalf("complete Workspace deletion: %v", err)
	}
	if _, err := workspaces.Create(ctx, pool, workspaces.CreateInput{
		TenantID: "tenant_test", OwnerID: "user_test", Name: "quota-after-delete",
		RuntimeClass: "standard", Image: "hako/go:latest", ResourcePlaneID: "rp_test",
		IdempotencyKey: "quota-after-delete",
	}, transaction.DefaultPolicy()); err != nil {
		t.Fatalf("create Workspace after completed deletion should reuse quota slot: %v", err)
	}
}

func TestResourcePlaneCapacityIsReservedAndReleasedAtomically(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply embedded schema migrations: %v", err)
	}
	seedPostgres(ctx, t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO resource_plane_capacities (resource_plane_id, max_workspaces, reserved_workspaces, updated_at) VALUES ($1, $2, $3, $4)`, "rp_test", 1, 0, time.Now().UTC()); err != nil {
		t.Fatalf("configure Resource Plane capacity: %v", err)
	}
	create := func(name, key string) (workspaces.CreateResult, error) {
		return workspaces.Create(ctx, pool, workspaces.CreateInput{
			TenantID: "tenant_test", OwnerID: "user_test", Name: name,
			RuntimeClass: "standard", Image: "hako/go:latest", IdempotencyKey: key,
		}, transaction.DefaultPolicy())
	}
	first, err := create("capacity-one", "capacity-one")
	if err != nil {
		t.Fatalf("create Workspace within configured capacity: %v", err)
	}
	if _, err := create("capacity-overflow", "capacity-overflow"); !errors.Is(err, scheduler.ErrNoEligibleResourcePlane) {
		t.Fatalf("capacity overflow should report no eligible Resource Plane; got %v", err)
	}
	var reserved int
	if err := pool.QueryRow(ctx, `SELECT reserved_workspaces FROM resource_plane_capacities WHERE resource_plane_id = $1`, "rp_test").Scan(&reserved); err != nil || reserved != 1 {
		t.Fatalf("capacity reservation count after overflow: reserved=%d error=%v", reserved, err)
	}
	deleting := domain.ObservedWorkspaceDeleting
	if _, err := operations.Transition(ctx, pool, transaction.DefaultPolicy(), operations.TransitionInput{
		OperationID: first.Operation.ID, ExpectedStatus: domain.OperationPending,
		NextStatus: domain.OperationRunning, EventType: "operation.started", ObservedState: &deleting,
	}); err != nil {
		t.Fatalf("start Workspace deletion: %v", err)
	}
	deleted := domain.ObservedWorkspaceDeleted
	if _, err := operations.Transition(ctx, pool, transaction.DefaultPolicy(), operations.TransitionInput{
		OperationID: first.Operation.ID, ExpectedStatus: domain.OperationRunning,
		NextStatus: domain.OperationSucceeded, EventType: "operation.succeeded", ObservedState: &deleted,
	}); err != nil {
		t.Fatalf("finish Workspace deletion and release capacity: %v", err)
	}
	second, err := create("capacity-after-delete", "capacity-after-delete")
	if err != nil {
		t.Fatalf("create Workspace after capacity release: %v", err)
	}
	if second.Placement.ResourcePlaneID != "rp_test" {
		t.Fatalf("expected released capacity on rp_test, got %s", second.Placement.ResourcePlaneID)
	}
	if _, err := pool.Exec(ctx, `UPDATE resource_plane_capacities SET max_workspaces = 2 WHERE resource_plane_id = $1`, "rp_test"); err != nil {
		t.Fatalf("raise Resource Plane capacity for concurrent admission test: %v", err)
	}
	type createResult struct {
		result workspaces.CreateResult
		err    error
	}
	const concurrentRequests = 8
	results := make(chan createResult, concurrentRequests)
	for i := 0; i < concurrentRequests; i++ {
		go func(i int) {
			result, err := create(fmt.Sprintf("capacity-concurrent-%02d", i), fmt.Sprintf("capacity-concurrent-%02d", i))
			results <- createResult{result: result, err: err}
		}(i)
	}
	accepted, rejected := 0, 0
	for i := 0; i < concurrentRequests; i++ {
		result := <-results
		switch {
		case result.err == nil:
			accepted++
		case errors.Is(result.err, scheduler.ErrNoEligibleResourcePlane):
			rejected++
		default:
			t.Fatalf("unexpected concurrent capacity result: %v", result.err)
		}
	}
	if accepted != 1 || rejected != concurrentRequests-1 {
		t.Fatalf("concurrent capacity admitted %d and rejected %d; want exactly one admission", accepted, rejected)
	}
	if err := pool.QueryRow(ctx, `SELECT reserved_workspaces FROM resource_plane_capacities WHERE resource_plane_id = $1`, "rp_test").Scan(&reserved); err != nil || reserved != 2 {
		t.Fatalf("concurrent reservations exceeded or missed configured capacity: reserved=%d error=%v", reserved, err)
	}
}

func TestSchedulerPrefersHealthyAndExcludesUnhealthyResourcePlanes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply embedded schema migrations: %v", err)
	}
	seedPostgres(ctx, t, pool)
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES ($1, $2, $3, $4)`, "rp_degraded", "aws", "us-west-2", `["microvm"]`); err != nil {
		t.Fatalf("insert degraded Resource Plane: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO resource_plane_status (resource_plane_id, status, updated_at) VALUES ($1, $2, $3)`, "rp_degraded", "active", now); err != nil {
		t.Fatalf("activate degraded Resource Plane: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO resource_plane_health (resource_plane_id, status, reason, updated_at) VALUES ($1, $2, $3, $4)`, "rp_degraded", "degraded", "maintenance window", now); err != nil {
		t.Fatalf("mark Resource Plane degraded: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := workspaces.Create(ctx, pool, workspaces.CreateInput{
			TenantID: "tenant_test", OwnerID: "user_test", Name: fmt.Sprintf("healthy-load-%d", i),
			RuntimeClass: "standard", Image: "hako/go:latest", ResourcePlaneID: "rp_test",
			IdempotencyKey: fmt.Sprintf("healthy-load-%d", i),
		}, transaction.DefaultPolicy()); err != nil {
			t.Fatalf("place initial Workspace on unreported (default healthy) plane: %v", err)
		}
	}
	selected, err := workspaces.Create(ctx, pool, workspaces.CreateInput{
		TenantID: "tenant_test", OwnerID: "user_test", Name: "health-prefers-healthy",
		RuntimeClass: "standard", Image: "hako/go:latest", IdempotencyKey: "health-prefers-healthy",
	}, transaction.DefaultPolicy())
	if err != nil || selected.Placement.ResourcePlaneID != "rp_test" {
		t.Fatalf("Scheduler should prefer a healthy candidate despite its greater load: plane=%s error=%v", selected.Placement.ResourcePlaneID, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO resource_plane_health (resource_plane_id, status, reason, updated_at) VALUES ($1, $2, $3, $4) ON CONFLICT (resource_plane_id) DO UPDATE SET status = $2, reason = $3, updated_at = $4`, "rp_test", "unhealthy", "health check failure", time.Now().UTC()); err != nil {
		t.Fatalf("mark healthy Resource Plane unhealthy: %v", err)
	}
	selected, err = workspaces.Create(ctx, pool, workspaces.CreateInput{
		TenantID: "tenant_test", OwnerID: "user_test", Name: "health-fallback-degraded",
		RuntimeClass: "standard", Image: "hako/go:latest", IdempotencyKey: "health-fallback-degraded",
	}, transaction.DefaultPolicy())
	if err != nil || selected.Placement.ResourcePlaneID != "rp_degraded" {
		t.Fatalf("Scheduler should use degraded Plane when healthy candidates are unavailable: plane=%s error=%v", selected.Placement.ResourcePlaneID, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE resource_plane_health SET status = 'unhealthy', reason = 'failed checks', updated_at = $2 WHERE resource_plane_id = $1`, "rp_degraded", time.Now().UTC()); err != nil {
		t.Fatalf("mark remaining Resource Plane unhealthy: %v", err)
	}
	if _, err := workspaces.Create(ctx, pool, workspaces.CreateInput{
		TenantID: "tenant_test", OwnerID: "user_test", Name: "health-no-eligible-plane",
		RuntimeClass: "standard", Image: "hako/go:latest", IdempotencyKey: "health-no-eligible-plane",
	}, transaction.DefaultPolicy()); !errors.Is(err, scheduler.ErrNoEligibleResourcePlane) {
		t.Fatalf("Scheduler should exclude all unhealthy Resource Planes: %v", err)
	}
}

func TestCognitoSubjectMappingAndTenantMembership(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply embedded schema migrations: %v", err)
	}
	seedPostgres(ctx, t, pool)
	policy := transaction.DefaultPolicy()

	// The seed identity already has a Hako User. Repeated resolution of its
	// verified Cognito sub must return the same stable internal ID.
	existing, err := users.ResolveCognitoSubject(ctx, pool, policy, "subject-test")
	if err != nil {
		t.Fatalf("resolve existing Cognito identity: %v", err)
	}
	if existing.ID != "user_test" {
		t.Fatalf("expected mapped Hako User user_test, got %q", existing.ID)
	}

	first, err := users.ResolveCognitoSubject(ctx, pool, policy, "new-cognito-subject")
	if err != nil {
		t.Fatalf("create Hako User for new Cognito identity: %v", err)
	}
	second, err := users.ResolveCognitoSubject(ctx, pool, policy, "new-cognito-subject")
	if err != nil {
		t.Fatalf("resolve repeated Cognito identity: %v", err)
	}
	if first.ID != second.ID || first.CognitoSubject != second.CognitoSubject || first.Email != "" {
		t.Fatalf("Cognito subject should map idempotently without inferring email: first=%+v second=%+v", first, second)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin Tenant membership check: %v", err)
	}
	defer tx.Rollback(ctx)
	membership, err := authz.RequireTenantRole(ctx, tx, "user_test", "tenant_test", domain.TenantRoleOwner)
	if err != nil {
		t.Fatalf("allow Tenant Owner membership: %v", err)
	}
	if membership.Role != domain.TenantRoleOwner {
		t.Fatalf("unexpected Tenant role: %+v", membership)
	}
	if _, err := authz.RequireTenantMembership(ctx, tx, first.ID, "tenant_test"); !errors.Is(err, authz.ErrTenantAccessDenied) {
		t.Fatalf("non-member must not access Tenant: %v", err)
	}
	if _, err := authz.RequireTenantMembership(ctx, tx, "user_test", "tenant_missing"); !errors.Is(err, authz.ErrTenantAccessDenied) {
		t.Fatalf("missing or cross-Tenant access must be denied without disclosure: %v", err)
	}
}

func seedPostgres(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	now := time.Now().UTC()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants (id, name, created_at) VALUES ($1, $2, $3)`, []any{"tenant_test", "Test tenant", now}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ($1, $2, $3, $4)`, []any{"user_test", "subject-test", "test@example.invalid", now}},
		{`INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ($1, $2, $3, $4)`, []any{"tenant_test", "user_test", "owner", now}},
		{`INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES ($1, $2, $3, $4)`, []any{"rp_test", "aws", "ap-northeast-1", `["microvm"]`}},
		{`INSERT INTO resource_plane_status (resource_plane_id, status, updated_at) VALUES ($1, $2, $3)`, []any{"rp_test", "active", now}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed integration fixture: %v", err)
		}
	}
}
