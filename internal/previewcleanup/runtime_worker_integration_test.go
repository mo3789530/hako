//go:build integration

package previewcleanup

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/previews"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestRuntimeWorkerCleansRetiredGenerationsAndReleasesPreviewQuota(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	seeds := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_preview_runtime_cleanup','Preview Runtime Cleanup',$1)`, []any{now}},
		{`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_preview_runtime_cleanup','preview-runtime-cleanup','',$1)`, []any{now}},
		{`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_preview_runtime_cleanup',9821,'acme','active','usr_preview_runtime_cleanup',$1,$1)`, []any{now}},
		{`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (9821,'tenant_preview_runtime_cleanup',$1)`, []any{now}},
		{`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,preview_enabled,preview_enabled_at) VALUES ('tenant_preview_runtime_cleanup',9821,9822,'acme','preview-runtime-cleanup','main',$1,true,$1)`, []any{now}},
		{`INSERT INTO repository_previews(tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,desired_state,state,last_delivery_id,last_event_at,expires_at,created_at,updated_at,generation)
		 VALUES ('tenant_preview_runtime_cleanup',9821,9822,41,$2,'feature/runtime-cleanup','present','awaiting_runtime','80000000-0000-4000-8000-000000000001',$1,$3,$1,$1,2)`, []any{now, sha, now.Add(time.Hour)}},
		{`INSERT INTO repository_preview_runtime_allocations(tenant_id,installation_id,github_repository_id,pull_request_number,generation,head_sha,head_ref,desired_state,state,expires_at,resource_plane_id,workspace_id,microvm_id,gateway_route_id,created_at,updated_at)
		 VALUES ('tenant_preview_runtime_cleanup',9821,9822,41,1,$2,'feature/runtime-cleanup','absent','cleanup_pending',$3,'rp_test','prvws_generation_1','microvm_generation_1','route_generation_1',$1,$1),
		 ('tenant_preview_runtime_cleanup',9821,9822,41,2,$2,'feature/runtime-cleanup','present','running',$3,'rp_test','prvws_generation_2','microvm_generation_2','route_generation_2',$1,$1)`, []any{now, sha, now.Add(time.Hour)}},
	}
	for _, seed := range seeds {
		if _, err := pool.Exec(ctx, seed.query, seed.args...); err != nil {
			t.Fatalf("seed runtime cleanup case: %v", err)
		}
	}

	clock := now.Add(time.Second)
	claimRuntimeCleanup := func(at time.Time, claimID string) ([]previews.RuntimeCleanupJob, error) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		jobs, err := previews.ClaimRuntimeCleanupDue(ctx, tx, claimID, at, 10*time.Second, 1)
		if err != nil {
			_ = tx.Rollback(ctx)
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return jobs, nil
	}
	crashedClaim, err := claimRuntimeCleanup(now, "preview-runtime-claim-a")
	if err != nil || len(crashedClaim) != 1 || crashedClaim[0].Attempt != 1 {
		t.Fatalf("initial generation cleanup claim: jobs=%+v err=%v", crashedClaim, err)
	}
	activeClaim, err := claimRuntimeCleanup(now.Add(5*time.Second), "preview-runtime-claim-b")
	if err != nil || len(activeClaim) != 0 {
		t.Fatalf("active generation cleanup lease was not exclusive: jobs=%+v err=%v", activeClaim, err)
	}
	clock = now.Add(11 * time.Second)
	claimSequence := 0
	executor := &runtimeCleanupFake{failures: []error{ErrMicroVMTermination}}
	worker := RuntimeWorker{
		Pool: pool, Executor: executor, Lease: 20 * time.Second, Timeout: 2 * time.Second, BatchSize: 1,
		Now: func() time.Time { return clock },
		NewClaimID: func() (string, error) {
			claimSequence++
			return fmt.Sprintf("preview-runtime-cleanup-%04d", claimSequence), nil
		},
	}
	failed, err := worker.RunOnce(ctx)
	if err != nil || failed.Claimed != 1 || failed.RetryScheduled != 1 || len(executor.jobs) != 1 || executor.jobs[0].Generation != 1 || executor.jobs[0].Attempt != 2 {
		t.Fatalf("first generation cleanup: stats=%+v jobs=%+v err=%v", failed, executor.jobs, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	staleConfirmed, err := previews.MarkRuntimeAllocationCleaned(ctx, tx, crashedClaim[0], clock.Add(time.Second))
	if err != nil || staleConfirmed {
		_ = tx.Rollback(ctx)
		t.Fatalf("expired generation cleanup claim acknowledged a newer attempt: confirmed=%v err=%v", staleConfirmed, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var retryAt time.Time
	var lastError string
	if err := pool.QueryRow(ctx, `SELECT runtime_cleanup_next_attempt_at,runtime_cleanup_last_error
		FROM repository_preview_runtime_allocations WHERE tenant_id='tenant_preview_runtime_cleanup' AND generation=1`).Scan(&retryAt, &lastError); err != nil || lastError != "microvm_terminate_failed" {
		t.Fatalf("runtime cleanup retry state: retry=%s error=%q err=%v", retryAt, lastError, err)
	}
	clock = retryAt.Add(-time.Second)
	early, err := worker.RunOnce(ctx)
	if err != nil || early.Claimed != 0 {
		t.Fatalf("runtime cleanup ignored retry deadline: stats=%+v err=%v", early, err)
	}
	clock = retryAt
	retired, err := worker.RunOnce(ctx)
	if err != nil || retired.Claimed != 1 || retired.Confirmed != 1 || len(executor.jobs) != 2 || executor.jobs[1].Generation != 1 {
		t.Fatalf("retired generation cleanup retry: stats=%+v jobs=%+v err=%v", retired, executor.jobs, err)
	}
	var previewState string
	if err := pool.QueryRow(ctx, `SELECT state FROM repository_previews WHERE tenant_id='tenant_preview_runtime_cleanup' AND pull_request_number=41`).Scan(&previewState); err != nil || previewState != "awaiting_runtime" {
		t.Fatalf("cleaning old head changed live Preview reservation: state=%q err=%v", previewState, err)
	}

	closeAt := retryAt.Add(time.Second)
	if _, err := pool.Exec(ctx, `UPDATE repository_previews SET desired_state='absent',state='cleanup_pending',generation=3,updated_at=$2
		WHERE tenant_id=$1 AND pull_request_number=41`, "tenant_preview_runtime_cleanup", closeAt); err != nil {
		t.Fatalf("close Preview for final generation cleanup: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE repository_preview_runtime_allocations SET desired_state='absent',state='cleanup_pending',updated_at=$2
		WHERE tenant_id=$1 AND pull_request_number=41 AND generation=2`, "tenant_preview_runtime_cleanup", closeAt); err != nil {
		t.Fatalf("retire final Preview generation: %v", err)
	}
	parentJobs, err := func() ([]previews.CleanupJob, error) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		jobs, err := previews.ClaimDue(ctx, tx, "preview-parent-cleanup", closeAt, 10*time.Second, 10)
		if err != nil {
			_ = tx.Rollback(ctx)
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return jobs, nil
	}()
	if err != nil || len(parentJobs) != 0 {
		t.Fatalf("legacy PR-wide cleanup raced generation-scoped cleanup: jobs=%+v err=%v", parentJobs, err)
	}
	clock = closeAt.Add(time.Second)
	final, err := worker.RunOnce(ctx)
	if err != nil || final.Claimed != 1 || final.Confirmed != 1 || executor.jobs[2].Generation != 2 {
		t.Fatalf("final Preview generation cleanup: stats=%+v jobs=%+v err=%v", final, executor.jobs, err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM repository_previews WHERE tenant_id='tenant_preview_runtime_cleanup' AND pull_request_number=41`).Scan(&previewState); err != nil || previewState != "cleaned" {
		t.Fatalf("Preview quota not released after every generation was cleaned: state=%q err=%v", previewState, err)
	}

	orphanAt := closeAt.Add(2 * time.Second)
	if _, err := pool.Exec(ctx, `INSERT INTO repository_previews
		(tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,desired_state,state,last_delivery_id,last_event_at,expires_at,created_at,updated_at,generation)
		VALUES ('tenant_preview_runtime_cleanup',9821,9822,42,$1,'feature/orphan','absent','cleanup_pending','80000000-0000-4000-8000-000000000002',$2,$3,$2,$2,1)`,
		sha, orphanAt, orphanAt.Add(time.Hour)); err != nil {
		t.Fatalf("seed interrupted-provision Preview: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repository_preview_runtime_allocations
		(tenant_id,installation_id,github_repository_id,pull_request_number,generation,head_sha,head_ref,desired_state,state,expires_at,target_resource_plane_id,provision_attempts,provision_claim_id,provision_lease_expires_at,created_at,updated_at)
		VALUES ('tenant_preview_runtime_cleanup',9821,9822,42,1,$1,'feature/orphan','absent','cleanup_pending',$2,'rp_test',1,'preview-provision-crashed',$4,$3,$3)`,
		sha, orphanAt.Add(time.Hour), orphanAt, orphanAt.Add(-time.Minute)); err != nil {
		t.Fatalf("seed unbound interrupted runtime generation: %v", err)
	}
	clock = orphanAt
	orphan, err := worker.RunOnce(ctx)
	if err != nil || orphan.Claimed != 1 || orphan.Confirmed != 1 || len(executor.jobs) != 4 {
		t.Fatalf("tag-based orphan cleanup: stats=%+v jobs=%+v err=%v", orphan, executor.jobs, err)
	}
	wantRuntimeID, err := previews.RuntimeWorkspaceID(previews.CleanupIdentity{TenantID: "tenant_preview_runtime_cleanup", InstallationID: 9821,
		RepositoryID: 9822, PullRequestNumber: 42}, 1)
	if err != nil || executor.jobs[3].ProviderBound || executor.jobs[3].ProvisionAttempts != 1 || executor.jobs[3].RuntimeWorkspaceID != wantRuntimeID {
		t.Fatalf("unbound orphan cleanup identity = %+v expected %q err=%v", executor.jobs[3], wantRuntimeID, err)
	}
}

type runtimeCleanupFake struct {
	failures []error
	jobs     []previews.RuntimeCleanupJob
}

func (f *runtimeCleanupFake) CleanupGeneration(_ context.Context, job previews.RuntimeCleanupJob) error {
	f.jobs = append(f.jobs, job)
	if len(f.failures) > 0 {
		err := f.failures[0]
		f.failures = f.failures[1:]
		return err
	}
	return nil
}
