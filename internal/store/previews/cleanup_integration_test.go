//go:build integration

package previews

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestCleanupLeaseBackoffRecoveryAndFencedCompletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, query := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_preview_cleanup','Preview cleanup',$1)`,
		`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_preview_cleanup','preview-cleanup','',$1)`,
		`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_preview_cleanup',9911,'acme','active','usr_preview_cleanup',$1,$1)`,
		`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (9911,'tenant_preview_cleanup',$1)`,
		`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,preview_enabled,preview_enabled_at) VALUES ('tenant_preview_cleanup',9911,9912,'acme','preview-cleanup','main',$1,true,$1)`,
		`INSERT INTO repository_previews(tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,desired_state,state,last_delivery_id,last_event_at,expires_at,created_at,updated_at) VALUES ('tenant_preview_cleanup',9911,9912,17,$1,'feature/cleanup','absent','cleanup_pending','10000000-0000-4000-8000-000000000001',$2,NULL,$2,$2)`,
	} {
		args := []any{now}
		if strings.Contains(query, "repository_previews") {
			args = []any{strings.Repeat("a", 40), now}
		}
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	identity := CleanupIdentity{TenantID: "tenant_preview_cleanup", InstallationID: 9911, RepositoryID: 9912, PullRequestNumber: 17}
	claim := func(at time.Time, id string) CleanupJob {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		jobs, err := ClaimDue(ctx, tx, id, at, 30*time.Second, 1)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("claim due cleanup: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 1 || jobs[0].CleanupIdentity != identity {
			t.Fatalf("claimed jobs = %+v, want exactly the Preview identity", jobs)
		}
		return jobs[0]
	}
	first := claim(now, "claim_preview_cleanup_0001")
	if first.Attempt != 1 || !first.LeaseExpiresAt.Equal(now.Add(30*time.Second)) {
		t.Fatalf("first cleanup lease = %+v", first)
	}
	if jobs, err := claimNone(ctx, pool, now.Add(time.Second)); err != nil || len(jobs) != 0 {
		t.Fatalf("active lease was claimable: jobs=%+v err=%v", jobs, err)
	}

	failureAt := now.Add(2 * time.Second)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	retryAt, updated, err := RecordCleanupFailure(ctx, tx, first, failureAt, "microvm_terminate_failed")
	if err != nil || !updated || !retryAt.Equal(failureAt.Add(5*time.Second)) {
		_ = tx.Rollback(ctx)
		t.Fatalf("record first cleanup failure: retry=%s updated=%v err=%v", retryAt, updated, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if jobs, err := claimNone(ctx, pool, retryAt.Add(-time.Second)); err != nil || len(jobs) != 0 {
		t.Fatalf("cleanup retry ignored backoff: jobs=%+v err=%v", jobs, err)
	}
	second := claim(retryAt, "claim_preview_cleanup_0002")
	if second.Attempt != 2 {
		t.Fatalf("retry attempt = %d, want 2", second.Attempt)
	}

	// Simulate a worker crash. The next worker may reclaim only after lease expiry.
	third := claim(second.LeaseExpiresAt.Add(time.Second), "claim_preview_cleanup_0003")
	if third.Attempt != 3 {
		t.Fatalf("recovered attempt = %d, want 3", third.Attempt)
	}
	completedAt := third.LeaseExpiresAt.Add(-time.Second)
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if completed, err := MarkCleaned(ctx, tx, second, completedAt); err != nil || completed {
		_ = tx.Rollback(ctx)
		t.Fatalf("stale claimant completed cleanup: completed=%v err=%v", completed, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if completed, err := MarkCleaned(ctx, tx, third, completedAt); err != nil || !completed {
		_ = tx.Rollback(ctx)
		t.Fatalf("current claimant did not complete cleanup: completed=%v err=%v", completed, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if completed, err := MarkCleaned(ctx, tx, third, completedAt.Add(time.Second)); err != nil || !completed {
		_ = tx.Rollback(ctx)
		t.Fatalf("duplicate cleanup acknowledgement was not idempotent: completed=%v err=%v", completed, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var state, desired, lastError string
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT desired_state,state,cleanup_attempts,cleanup_last_error FROM repository_previews WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND pull_request_number=$4`, identity.TenantID, identity.InstallationID, identity.RepositoryID, identity.PullRequestNumber).
		Scan(&desired, &state, &attempts, &lastError); err != nil {
		t.Fatal(err)
	}
	if desired != "absent" || state != "cleaned" || attempts != 3 || lastError != "" {
		t.Fatalf("final Preview cleanup state = desired:%s state:%s attempts:%d error:%q", desired, state, attempts, lastError)
	}
}

func claimNone(ctx context.Context, pool *pgxpool.Pool, at time.Time) ([]CleanupJob, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	jobs, err := ClaimDue(ctx, tx, "claim_preview_cleanup_none", at, 30*time.Second, 1)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return jobs, nil
}
