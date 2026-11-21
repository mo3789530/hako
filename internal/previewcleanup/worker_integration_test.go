//go:build integration

package previewcleanup

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/previews"
	"github.com/mo3789530/hako/internal/testutil"
)

type fakePreviewExecutor struct {
	results []error
	jobs    []previews.CleanupJob
}

func (e *fakePreviewExecutor) Cleanup(_ context.Context, job previews.CleanupJob) error {
	e.jobs = append(e.jobs, job)
	if len(e.results) == 0 {
		return nil
	}
	err := e.results[0]
	e.results = e.results[1:]
	return err
}

func TestPreviewCleanupWorkerRetriesAndOnlyReleasesQuotaAfterConfirmation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, query := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_preview_worker','Preview Worker',$1)`,
		`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_preview_worker','preview-worker','',$1)`,
		`INSERT INTO tenant_quota_limits(tenant_id,workspace_limit,concurrent_workload_run_limit,preview_run_limit,preview_lifetime_seconds,revision,updated_by,updated_at) VALUES ('tenant_preview_worker',10,5,1,3600,1,'usr_preview_worker',$1)`,
		`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_preview_worker',9911,'acme','active','usr_preview_worker',$1,$1)`,
		`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (9911,'tenant_preview_worker',$1)`,
		`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,preview_enabled,preview_enabled_at) VALUES ('tenant_preview_worker',9911,9912,'acme','preview-worker','main',$1,true,$1)`,
	} {
		if _, err := pool.Exec(ctx, query, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repository_previews
		(tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,desired_state,state,last_delivery_id,last_event_at,generation,expires_at,created_at,updated_at)
		VALUES ('tenant_preview_worker',9911,9912,17,$1,'feature/cleanup','absent','cleanup_pending','10000000-0000-4000-8000-000000000001',$2,4,NULL,$2,$2)`, strings.Repeat("c", 40), now); err != nil {
		t.Fatal(err)
	}
	clock := now.Add(time.Second)
	claimNumber := 0
	executor := &fakePreviewExecutor{results: []error{ErrMicroVMTermination, nil}}
	worker := Worker{Pool: pool, Executor: executor, Now: func() time.Time { return clock }, Lease: 30 * time.Second,
		Timeout: 10 * time.Second, BatchSize: 2, NewClaimID: func() (string, error) {
			claimNumber++
			return fmt.Sprintf("preview-worker-claim-%04d", claimNumber), nil
		}}
	stats, err := worker.RunOnce(ctx)
	if err != nil || stats != (Stats{Claimed: 1, RetryScheduled: 1}) {
		t.Fatalf("first cleanup pass stats=%+v err=%v", stats, err)
	}
	var state, failure string
	var nextAttempt time.Time
	if err := pool.QueryRow(ctx, `SELECT state,cleanup_last_error,cleanup_next_attempt_at FROM repository_previews
		WHERE tenant_id='tenant_preview_worker' AND installation_id=9911 AND github_repository_id=9912 AND pull_request_number=17`).Scan(&state, &failure, &nextAttempt); err != nil {
		t.Fatal(err)
	}
	if state != "cleanup_pending" || failure != "microvm_terminate_failed" || !nextAttempt.Equal(clock.Add(5*time.Second)) {
		t.Fatalf("failed physical cleanup released Preview reservation: state=%s failure=%s retry=%s", state, failure, nextAttempt)
	}
	clock = nextAttempt
	stats, err = worker.RunOnce(ctx)
	if err != nil || stats != (Stats{Claimed: 1, Confirmed: 1}) {
		t.Fatalf("confirmed cleanup pass stats=%+v err=%v", stats, err)
	}
	if len(executor.jobs) != 2 || executor.jobs[0].Generation != 4 || executor.jobs[1].Generation != 4 ||
		executor.jobs[0].Attempt != 1 || executor.jobs[1].Attempt != 2 ||
		executor.jobs[0].ClaimID == executor.jobs[1].ClaimID {
		t.Fatalf("cleanup retry identity was not fenced: %+v", executor.jobs)
	}
	if err := pool.QueryRow(ctx, `SELECT state,cleanup_last_error FROM repository_previews
		WHERE tenant_id='tenant_preview_worker' AND installation_id=9911 AND github_repository_id=9912 AND pull_request_number=17`).Scan(&state, &failure); err != nil {
		t.Fatal(err)
	}
	if state != "cleaned" || failure != "" {
		t.Fatalf("confirmed Preview cleanup state=%s failure=%q", state, failure)
	}
	stats, err = worker.RunOnce(ctx)
	if err != nil || stats != (Stats{}) {
		t.Fatalf("duplicate cleanup pass stats=%+v err=%v", stats, err)
	}
}
