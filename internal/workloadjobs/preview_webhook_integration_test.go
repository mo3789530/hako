//go:build integration

package workloadjobs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/dsql"
	webhookstore "github.com/mo3789530/hako/internal/store/githubwebhook"
	previewstore "github.com/mo3789530/hako/internal/store/previews"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestRepositoryPreviewWebhookLifecycleFencesStalePRUpdates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, query := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_preview_hook','Preview Hook',$1)`,
		`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_preview_hook','preview-hook','',$1)`,
		`INSERT INTO tenant_quota_limits(tenant_id,workspace_limit,concurrent_workload_run_limit,preview_run_limit,preview_lifetime_seconds,revision,updated_by,updated_at) VALUES ('tenant_preview_hook',10,5,2,3600,1,'usr_preview_hook',$1)`,
		`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_preview_hook',9801,'acme','active','usr_preview_hook',$1,$1)`,
		`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (9801,'tenant_preview_hook',$1)`,
		`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,preview_enabled,preview_enabled_at) VALUES ('tenant_preview_hook',9801,9802,'acme','preview-app','main',$1,true,$1)`,
	} {
		if _, err := pool.Exec(ctx, query, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	newSHA, oldSHA := strings.Repeat("b", 40), strings.Repeat("a", 40)
	deliveries := []struct {
		id, action, state, sha string
		received, updated      time.Time
	}{
		{"10000000-0000-4000-8000-000000000001", "opened", "open", newSHA, now, now},
		// Simulate delayed delivery of an older PR head after the newer update.
		{"10000000-0000-4000-8000-000000000002", "synchronize", "open", oldSHA, now.Add(time.Second), now.Add(-time.Second)},
		{"10000000-0000-4000-8000-000000000003", "synchronize", "open", newSHA, now.Add(2 * time.Second), now.Add(time.Second)},
		{"10000000-0000-4000-8000-000000000004", "closed", "closed", oldSHA, now.Add(3 * time.Second), now.Add(2 * time.Second)},
		{"10000000-0000-4000-8000-000000000005", "reopened", "open", newSHA, now.Add(4 * time.Second), now.Add(3 * time.Second)},
	}
	var initialExpiry time.Time
	for _, item := range deliveries {
		delivery := githubwebhook.VerifiedDelivery{IntegrationID: "shared", DeliveryID: item.id, Event: "pull_request", Action: item.action,
			Payload: []byte(fmt.Sprintf(`{"action":%q,"installation":{"id":9801},"repository":{"id":9802,"name":"preview-app","default_branch":"main","owner":{"login":"acme"}},"pull_request":{"number":73,"state":%q,"updated_at":%q,"head":{"ref":"feature/preview","sha":%q},"base":{"ref":"main"}}}`,
				item.action, item.state, item.updated.Format(time.RFC3339), item.sha))}
		if inserted, err := (webhookstore.Inbox{Pool: pool}).Record(ctx, delivery, item.received); err != nil || !inserted {
			t.Fatalf("record %s delivery: inserted=%v error=%v", item.action, inserted, err)
		}
		stats, err := webhookstore.ProcessBatchWithRepositoryPreviews(ctx, pool, transaction.DefaultPolicy(), 10, item.received.Add(time.Second), nil, ApplyWebhookRepositoryPreview)
		if err != nil {
			t.Fatalf("process %s: stats=%+v error=%v", item.action, stats, err)
		}
		if item.action == "opened" {
			if err := pool.QueryRow(ctx, `SELECT expires_at FROM repository_previews WHERE tenant_id='tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=73`).Scan(&initialExpiry); err != nil {
				t.Fatal(err)
			}
		}
		if item.action == "synchronize" {
			var currentExpiry time.Time
			if err := pool.QueryRow(ctx, `SELECT expires_at FROM repository_previews WHERE tenant_id='tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=73`).Scan(&currentExpiry); err != nil {
				t.Fatal(err)
			}
			if !currentExpiry.Equal(initialExpiry) {
				t.Fatalf("synchronize delivery extended fixed Preview expiry: initial=%s current=%s", initialExpiry, currentExpiry)
			}
		}
	}
	var tenant, sha, desired, state, lastDelivery string
	var attempts int
	var expiresAt time.Time
	if err := pool.QueryRow(ctx, `SELECT tenant_id,head_sha,desired_state,state,last_delivery_id,cleanup_attempts,expires_at FROM repository_previews WHERE tenant_id='tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=73`).
		Scan(&tenant, &sha, &desired, &state, &lastDelivery, &attempts, &expiresAt); err != nil {
		t.Fatal(err)
	}
	if tenant != "tenant_preview_hook" || sha != newSHA || desired != "present" || state != "awaiting_runtime" ||
		lastDelivery != deliveries[4].id || attempts != 0 || !expiresAt.Equal(now.Add(5*time.Second+time.Hour)) {
		t.Fatalf("reopened Preview desired state = tenant:%s sha:%s desired:%s state:%s delivery:%s attempts:%d expires:%s", tenant, sha, desired, state, lastDelivery, attempts, expiresAt)
	}
	for i := 0; i < 2; i++ {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		expired, err := previewstore.ExpireDue(ctx, tx, now.Add(2*time.Hour), 10)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("expire Preview batch: %v", err)
		}
		if i == 0 {
			syncEvent := githubwebhook.RepositoryEvent{GitHubEvent: "pull_request", Action: "synchronize", InstallationID: 9801,
				Repository:  &githubwebhook.RepositoryRef{GitHubID: 9802},
				PullRequest: &githubwebhook.PullRequestRef{Number: 73, State: "open", HeadRef: "feature/preview", HeadSHA: newSHA, UpdatedAt: now.Add(3 * time.Hour)}}
			if updated, err := previewstore.ApplyPullRequestDelivery(ctx, tx, "tenant_preview_hook", "20000000-0000-4000-8000-000000000001", syncEvent, now.Add(3*time.Hour), now.Add(3*time.Hour)); err != nil || !updated {
				_ = tx.Rollback(ctx)
				t.Fatalf("synchronize must advance audit cursor but not revive expired Preview: updated=%v error=%v", updated, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if (i == 0 && expired != 1) || (i == 1 && expired != 0) {
			t.Fatalf("expiry pass %d transitioned %d records", i, expired)
		}
	}
	if err := pool.QueryRow(ctx, `SELECT desired_state,state FROM repository_previews WHERE tenant_id='tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=73`).Scan(&desired, &state); err != nil {
		t.Fatal(err)
	}
	if desired != "absent" || state != "cleanup_pending" {
		t.Fatalf("expired Preview state = desired:%s state:%s, physical cleanup must remain pending", desired, state)
	}
	claimCleanup := func(at time.Time, claimID string) (previewstore.CleanupJob, bool, error) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return previewstore.CleanupJob{}, false, err
		}
		jobs, err := previewstore.ClaimDue(ctx, tx, claimID, at, 30*time.Second, 10)
		if err != nil {
			_ = tx.Rollback(ctx)
			return previewstore.CleanupJob{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return previewstore.CleanupJob{}, false, err
		}
		if len(jobs) > 1 {
			return previewstore.CleanupJob{}, false, fmt.Errorf("claimed %d Preview cleanup records, expected at most one", len(jobs))
		}
		if len(jobs) == 0 {
			return previewstore.CleanupJob{}, false, nil
		}
		return jobs[0], true, nil
	}
	cleanupNow := now.Add(3 * time.Hour)
	firstJob, claimed, err := claimCleanup(cleanupNow, "preview-cleaner-a1")
	if err != nil || !claimed || firstJob.Attempt != 1 || firstJob.LeaseExpiresAt != cleanupNow.Add(30*time.Second) {
		t.Fatalf("first Preview cleanup claim = %+v claimed=%v error=%v", firstJob, claimed, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	retryAt, updated, err := previewstore.RecordCleanupFailure(ctx, tx, firstJob, cleanupNow.Add(time.Second), "microvm_terminate_failed")
	if err != nil || !updated || !retryAt.Equal(cleanupNow.Add(6*time.Second)) {
		_ = tx.Rollback(ctx)
		t.Fatalf("first Preview cleanup failure = retry:%s updated:%v error:%v", retryAt, updated, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := claimCleanup(cleanupNow.Add(5*time.Second), "preview-cleaner-a2"); err != nil || claimed {
		t.Fatalf("Preview cleanup retry claimed before backoff elapsed: claimed=%v error=%v", claimed, err)
	}
	secondJob, claimed, err := claimCleanup(cleanupNow.Add(6*time.Second), "preview-cleaner-a2")
	if err != nil || !claimed || secondJob.Attempt != 2 {
		t.Fatalf("second Preview cleanup claim = %+v claimed=%v error=%v", secondJob, claimed, err)
	}
	// A worker whose lease expired or was superseded cannot complete a recovered claim.
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stale, err := previewstore.MarkCleaned(ctx, tx, firstJob, cleanupNow.Add(7*time.Second)); err != nil || stale {
		_ = tx.Rollback(ctx)
		t.Fatalf("stale Preview cleanup claim completed: cleaned=%v error=%v", stale, err)
	}
	retryAt, updated, err = previewstore.RecordCleanupFailure(ctx, tx, secondJob, cleanupNow.Add(7*time.Second), "provider_unavailable")
	if err != nil || !updated || !retryAt.Equal(cleanupNow.Add(17*time.Second)) {
		_ = tx.Rollback(ctx)
		t.Fatalf("second Preview cleanup failure = retry:%s updated:%v error:%v", retryAt, updated, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	thirdJob, claimed, err := claimCleanup(cleanupNow.Add(17*time.Second), "preview-cleaner-a3")
	if err != nil || !claimed || thirdJob.Attempt != 3 {
		t.Fatalf("third Preview cleanup claim = %+v claimed=%v error=%v", thirdJob, claimed, err)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cleaned, err := previewstore.MarkCleaned(ctx, tx, thirdJob, cleanupNow.Add(18*time.Second))
	if err != nil || !cleaned {
		_ = tx.Rollback(ctx)
		t.Fatalf("mark confirmed Preview cleanup: cleaned=%v error=%v", cleaned, err)
	}
	if duplicate, err := previewstore.MarkCleaned(ctx, tx, thirdJob, cleanupNow.Add(18*time.Second)); err != nil || !duplicate {
		_ = tx.Rollback(ctx)
		t.Fatalf("duplicate Preview cleanup confirmation: cleaned=%v error=%v", duplicate, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var lastError, cleanupState string
	var cleanupAttempts int
	var cleanupClaim *string
	var cleanupLease *time.Time
	if err := pool.QueryRow(ctx, `SELECT state,cleanup_attempts,cleanup_last_error,cleanup_claim_id,cleanup_lease_expires_at FROM repository_previews WHERE tenant_id='tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=73`).Scan(&cleanupState, &cleanupAttempts, &lastError, &cleanupClaim, &cleanupLease); err != nil || cleanupState != "cleaned" || cleanupAttempts != 3 || lastError != "" || cleanupClaim != nil || cleanupLease != nil {
		t.Fatalf("persisted Preview cleanup state=%s attempts=%d last_error=%q claim=%v lease=%v err=%v", cleanupState, cleanupAttempts, lastError, cleanupClaim, cleanupLease, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repository_previews
		(tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,desired_state,state,last_delivery_id,last_event_at,created_at,updated_at)
		VALUES ('tenant_preview_hook',9801,9802,74,$1,'feature/lease','absent','cleanup_pending','40000000-0000-4000-8000-000000000001',$2,$2,$2)`, strings.Repeat("c", 40), cleanupNow); err != nil {
		t.Fatal(err)
	}
	crashedWorkerJob, claimed, err := claimCleanup(cleanupNow.Add(time.Minute), "preview-cleaner-crash")
	if err != nil || !claimed || crashedWorkerJob.PullRequestNumber != 74 || crashedWorkerJob.Attempt != 1 {
		t.Fatalf("crashed Worker initial claim = %+v claimed=%v error=%v", crashedWorkerJob, claimed, err)
	}
	recoveredWorkerJob, claimed, err := claimCleanup(cleanupNow.Add(90*time.Second), "preview-cleaner-recovery")
	if err != nil || !claimed || recoveredWorkerJob.PullRequestNumber != 74 || recoveredWorkerJob.Attempt != 2 {
		t.Fatalf("expired Preview cleanup lease recovery = %+v claimed=%v error=%v", recoveredWorkerJob, claimed, err)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stale, err := previewstore.MarkCleaned(ctx, tx, crashedWorkerJob, cleanupNow.Add(91*time.Second)); err != nil || stale {
		_ = tx.Rollback(ctx)
		t.Fatalf("expired Preview cleanup worker acknowledged recovered lease: cleaned=%v error=%v", stale, err)
	}
	if recovered, err := previewstore.MarkCleaned(ctx, tx, recoveredWorkerJob, cleanupNow.Add(91*time.Second)); err != nil || !recovered {
		_ = tx.Rollback(ctx)
		t.Fatalf("recovered Preview cleanup confirmation: cleaned=%v error=%v", recovered, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if expired, err := previewstore.ExpireBatch(ctx, pool, now.Add(4*time.Hour), 10); err != nil || expired != 0 {
		t.Fatalf("transactional Preview expiry wrapper = %d, %v", expired, err)
	}
	var records int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM repository_previews WHERE tenant_id <> 'tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=73`).Scan(&records); err != nil || records != 0 {
		t.Fatalf("Preview state escaped Tenant scope: records=%d error=%v", records, err)
	}
	duplicate, err := webhookstore.ProcessBatchWithRepositoryPreviews(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(10*time.Second), nil, ApplyWebhookRepositoryPreview)
	if err != nil || duplicate.RepositoryPreviewsUpdated != 0 || duplicate.Processed != 0 {
		t.Fatalf("duplicate deliveries changed Preview state: stats=%+v error=%v", duplicate, err)
	}
}
