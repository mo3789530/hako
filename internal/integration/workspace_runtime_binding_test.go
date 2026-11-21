//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/operationresults"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/operations"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workspaces"
	"github.com/mo3789530/hako/internal/testutil"
)

type workspaceRuntimeResultQueue struct {
	results []resourcecontroller.Result
	acked   map[string]bool
}

func (q *workspaceRuntimeResultQueue) Report(_ context.Context, result resourcecontroller.Result) error {
	q.results = append(q.results, result)
	return nil
}

func (q *workspaceRuntimeResultQueue) Receive(_ context.Context, max int, _ time.Duration) ([]operationresults.Delivery, error) {
	var deliveries []operationresults.Delivery
	for i, result := range q.results {
		handle := string(rune('a' + i))
		if q.acked[handle] {
			continue
		}
		body, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		deliveries = append(deliveries, operationresults.Delivery{Body: body, ReceiptHandle: handle})
		if len(deliveries) == max {
			break
		}
	}
	return deliveries, nil
}

func (q *workspaceRuntimeResultQueue) Delete(_ context.Context, receipt string) error {
	if q.acked == nil {
		q.acked = map[string]bool{}
	}
	q.acked[receipt] = true
	return nil
}

type workspaceBindingRuntime struct{}

func (workspaceBindingRuntime) Execute(_ context.Context, command resourcecontroller.Command) (domain.ObservedWorkspaceState, error) {
	if command.Type == domain.OperationDelete {
		return domain.ObservedWorkspaceDeleted, nil
	}
	return domain.ObservedWorkspaceRunning, nil
}

func (workspaceBindingRuntime) ResourceBinding(_ context.Context, command resourcecontroller.Command) (string, string, int64, error) {
	if command.Type == domain.OperationDelete {
		return "", "", 0, nil
	}
	return "lambda_microvm", "microvm-e2e-1", 1, nil
}

func TestWorkspaceRuntimeBindingResultAndDeleteAreAtomic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply schema migrations: %v", err)
	}
	seedPostgres(ctx, t, pool)
	created, err := workspaces.Create(ctx, pool, workspaces.CreateInput{
		TenantID: "tenant_test", OwnerID: "user_test", Name: "runtime-binding",
		RuntimeClass: "standard", Image: "hako/workspace:local", ResourcePlaneID: "rp_test", IdempotencyKey: "runtime-binding-create",
	}, transaction.DefaultPolicy())
	if err != nil {
		t.Fatalf("create Workspace: %v", err)
	}
	completedAt := time.Now().UTC()
	applied, err := operations.ApplyResult(ctx, pool, transaction.DefaultPolicy(), operations.ResultInput{
		OperationID: created.Operation.ID, TenantID: created.Workspace.TenantID, WorkspaceID: created.Workspace.ID,
		ResourcePlaneID: created.Placement.ResourcePlaneID, Type: domain.OperationEnsureRunning,
		Status: domain.OperationSucceeded, ObservedState: domain.ObservedWorkspaceRunning,
		ResourceKind: "lambda_microvm", ExternalResourceID: "microvm-binding-e2e", AllocationGeneration: 1, CompletedAt: completedAt,
	})
	if err != nil || !applied {
		t.Fatalf("apply runtime result: applied=%v error=%v", applied, err)
	}
	var externalID string
	var generation int64
	if err := pool.QueryRow(ctx, `SELECT external_id, allocation_generation FROM resource_bindings WHERE workspace_id = $1 AND resource_plane_id = $2 AND kind = 'lambda_microvm'`, created.Workspace.ID, created.Placement.ResourcePlaneID).Scan(&externalID, &generation); err != nil || externalID != "microvm-binding-e2e" || generation != 1 {
		t.Fatalf("Workspace provider binding = %q generation=%d, %v", externalID, generation, err)
	}
	serviceTime := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO services (id, workspace_id, name, catalog_version, config_json, desired_state, observed_state, desired_revision, observed_revision, created_at, updated_at)
		VALUES ('svc_generation_test', $1, 'postgres', 'postgres/1.0.0', '{}', 'running', 'healthy', 1, 1, $2, $2)`, created.Workspace.ID, serviceTime); err != nil {
		t.Fatalf("seed Service runtime generation: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_service_runtime_generations
		(service_id, workspace_id, resource_plane_id, allocation_generation, microvm_id, desired_revision, observed_state, updated_at)
		VALUES ('svc_generation_test', $1, $2, 1, 'microvm-binding-e2e', 1, 'healthy', $3)`,
		created.Workspace.ID, created.Placement.ResourcePlaneID, serviceTime); err != nil {
		t.Fatalf("seed Service generation ledger: %v", err)
	}

	suspended, err := workspaces.RequestAction(ctx, pool, workspaces.ActionInput{
		TenantID: created.Workspace.TenantID, WorkspaceID: created.Workspace.ID, UserID: "user_test",
		Action: domain.OperationSuspend, IdempotencyKey: "runtime-binding-suspend",
	}, transaction.DefaultPolicy())
	if err != nil {
		t.Fatalf("request Workspace suspension: %v", err)
	}
	if applied, err := operations.ApplyResult(ctx, pool, transaction.DefaultPolicy(), operations.ResultInput{
		OperationID: suspended.Operation.ID, TenantID: suspended.Operation.TenantID, WorkspaceID: suspended.Operation.WorkspaceID,
		ResourcePlaneID: suspended.Operation.ResourcePlaneID, Type: domain.OperationSuspend,
		Status: domain.OperationSucceeded, ObservedState: domain.ObservedWorkspaceSuspended, CompletedAt: time.Now().UTC(),
	}); err != nil || !applied {
		t.Fatalf("apply Workspace suspend result: applied=%v error=%v", applied, err)
	}
	resumed, err := workspaces.RequestAction(ctx, pool, workspaces.ActionInput{
		TenantID: created.Workspace.TenantID, WorkspaceID: created.Workspace.ID, UserID: "user_test",
		Action: domain.OperationResume, IdempotencyKey: "runtime-binding-resume",
	}, transaction.DefaultPolicy())
	if err != nil {
		t.Fatalf("request Workspace resume: %v", err)
	}
	if applied, err := operations.ApplyResult(ctx, pool, transaction.DefaultPolicy(), operations.ResultInput{
		OperationID: resumed.Operation.ID, TenantID: resumed.Operation.TenantID, WorkspaceID: resumed.Operation.WorkspaceID,
		ResourcePlaneID: resumed.Operation.ResourcePlaneID, Type: domain.OperationResume,
		Status: domain.OperationSucceeded, ObservedState: domain.ObservedWorkspaceRunning,
		ResourceKind: "lambda_microvm", ExternalResourceID: "microvm-binding-e2e-2", AllocationGeneration: 2,
		CompletedAt: time.Now().UTC(),
	}); err != nil || !applied {
		t.Fatalf("apply Workspace resume result: applied=%v error=%v", applied, err)
	}
	var retired bool
	if err := pool.QueryRow(ctx, `SELECT retired FROM workspace_service_runtime_generations WHERE service_id = 'svc_generation_test' AND allocation_generation = 1`).Scan(&retired); err != nil || !retired {
		t.Fatalf("prior MicroVM Service generation was not retired after replacement: retired=%v error=%v", retired, err)
	}
	if err := pool.QueryRow(ctx, `SELECT external_id, allocation_generation FROM resource_bindings WHERE workspace_id = $1 AND resource_plane_id = $2 AND kind = 'lambda_microvm'`, created.Workspace.ID, created.Placement.ResourcePlaneID).Scan(&externalID, &generation); err != nil || externalID != "microvm-binding-e2e-2" || generation != 2 {
		t.Fatalf("replacement Workspace provider binding = %q generation=%d, %v", externalID, generation, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_service_runtime_generations
		(service_id, workspace_id, resource_plane_id, allocation_generation, microvm_id, desired_revision, observed_state, updated_at)
		VALUES ('svc_generation_test', $1, $2, 2, 'microvm-binding-e2e-2', 1, 'healthy', $3)`,
		created.Workspace.ID, created.Placement.ResourcePlaneID, time.Now().UTC()); err != nil {
		t.Fatalf("seed current Service runtime generation: %v", err)
	}

	deleted, err := workspaces.RequestAction(ctx, pool, workspaces.ActionInput{
		TenantID: created.Workspace.TenantID, WorkspaceID: created.Workspace.ID, UserID: "user_test",
		Action: domain.OperationDelete, IdempotencyKey: "runtime-binding-delete",
	}, transaction.DefaultPolicy())
	if err != nil {
		t.Fatalf("request Workspace deletion: %v", err)
	}
	applied, err = operations.ApplyResult(ctx, pool, transaction.DefaultPolicy(), operations.ResultInput{
		OperationID: deleted.Operation.ID, TenantID: deleted.Operation.TenantID, WorkspaceID: deleted.Operation.WorkspaceID,
		ResourcePlaneID: deleted.Operation.ResourcePlaneID, Type: domain.OperationDelete,
		Status: domain.OperationSucceeded, ObservedState: domain.ObservedWorkspaceDeleted, CompletedAt: time.Now().UTC(),
	})
	if err != nil || !applied {
		t.Fatalf("apply Workspace delete result: applied=%v error=%v", applied, err)
	}
	if err := pool.QueryRow(ctx, `SELECT retired FROM workspace_service_runtime_generations WHERE service_id = 'svc_generation_test' AND allocation_generation = 2`).Scan(&retired); err != nil || !retired {
		t.Fatalf("Workspace deletion did not retire its current Service runtime generation: retired=%v error=%v", retired, err)
	}
	var bindings int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM resource_bindings WHERE workspace_id = $1 AND kind = 'lambda_microvm'`, created.Workspace.ID).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("deleted Workspace retained MicroVM bindings: count=%d error=%v", bindings, err)
	}
}

func TestWorkspaceControllerResultConsumerProjectsBindingEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply schema migrations: %v", err)
	}
	seedPostgres(ctx, t, pool)
	created, err := workspaces.Create(ctx, pool, workspaces.CreateInput{
		TenantID: "tenant_test", OwnerID: "user_test", Name: "runtime-result-e2e",
		RuntimeClass: "standard", Image: "hako/workspace:local", ResourcePlaneID: "rp_test", IdempotencyKey: "runtime-result-create",
	}, transaction.DefaultPolicy())
	if err != nil {
		t.Fatalf("create Workspace: %v", err)
	}
	queue := &workspaceRuntimeResultQueue{}
	controller, err := resourcecontroller.New(created.Placement.ResourcePlaneID, workspaceBindingRuntime{}, queue, nil)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := operationresults.New(operationresults.StoreAdapter{Pool: pool, Policy: transaction.DefaultPolicy()}, queue, operationresults.Config{WaitTime: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	command := resourcecontroller.Command{SchemaVersion: 2, WorkspaceRevision: 1, OperationID: created.Operation.ID,
		TenantID: created.Operation.TenantID, WorkspaceID: created.Operation.WorkspaceID,
		ResourcePlaneID: created.Operation.ResourcePlaneID, Type: created.Operation.Type, CreatedAt: time.Now().UTC()}
	commandBody, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Handle(ctx, commandBody); err != nil {
		t.Fatalf("Resource Controller handle: %v", err)
	}
	stats, err := consumer.RunOnce(ctx)
	if err != nil || stats.Applied != 1 {
		t.Fatalf("Control Plane result consume: %+v, %v", stats, err)
	}
	var externalID string
	if err := pool.QueryRow(ctx, `SELECT external_id FROM resource_bindings WHERE workspace_id = $1 AND kind = 'lambda_microvm'`, created.Workspace.ID).Scan(&externalID); err != nil || externalID != "microvm-e2e-1" {
		t.Fatalf("end-to-end provider binding = %q, %v", externalID, err)
	}
}
