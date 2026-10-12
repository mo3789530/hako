//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/operations"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workspaces"
	"github.com/mo3789530/hako/internal/testutil"
)

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
		ResourceKind: "lambda_microvm", ExternalResourceID: "microvm-binding-e2e", CompletedAt: completedAt,
	})
	if err != nil || !applied {
		t.Fatalf("apply runtime result: applied=%v error=%v", applied, err)
	}
	var externalID string
	if err := pool.QueryRow(ctx, `SELECT external_id FROM resource_bindings WHERE workspace_id = $1 AND resource_plane_id = $2 AND kind = 'lambda_microvm'`, created.Workspace.ID, created.Placement.ResourcePlaneID).Scan(&externalID); err != nil || externalID != "microvm-binding-e2e" {
		t.Fatalf("Workspace provider binding = %q, %v", externalID, err)
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
	var bindings int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM resource_bindings WHERE workspace_id = $1 AND kind = 'lambda_microvm'`, created.Workspace.ID).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("deleted Workspace retained MicroVM bindings: count=%d error=%v", bindings, err)
	}
}
