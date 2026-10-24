//go:build integration

package workloadjobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/previewprovision"
	"github.com/mo3789530/hako/internal/store/dsql"
	webhookstore "github.com/mo3789530/hako/internal/store/githubwebhook"
	previewstore "github.com/mo3789530/hako/internal/store/previews"
	"github.com/mo3789530/hako/internal/store/quotas"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/store/workspaces"
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
		`INSERT INTO tenant_quota_limits(tenant_id,workspace_limit,concurrent_workload_run_limit,preview_run_limit,preview_lifetime_seconds,revision,updated_by,updated_at) VALUES ('tenant_preview_hook',10,5,1,3600,1,'usr_preview_hook',$1)`,
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
	wantGenerations := []int64{1, 1, 1, 2, 3}
	for index, item := range deliveries {
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
		var generation int64
		if err := pool.QueryRow(ctx, `SELECT generation FROM repository_previews WHERE tenant_id='tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=73`).Scan(&generation); err != nil {
			t.Fatal(err)
		}
		if generation != wantGenerations[index] {
			t.Fatalf("Preview generation after %s = %d, want %d", item.action, generation, wantGenerations[index])
		}
	}
	var tenant, sha, desired, state, lastDelivery string
	var attempts int
	var expiresAt time.Time
	var lifecycleGeneration int64
	if err := pool.QueryRow(ctx, `SELECT tenant_id,head_sha,desired_state,state,last_delivery_id,cleanup_attempts,expires_at,generation FROM repository_previews WHERE tenant_id='tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=73`).
		Scan(&tenant, &sha, &desired, &state, &lastDelivery, &attempts, &expiresAt, &lifecycleGeneration); err != nil {
		t.Fatal(err)
	}
	if tenant != "tenant_preview_hook" || sha != newSHA || desired != "present" || state != "awaiting_runtime" ||
		lastDelivery != deliveries[4].id || attempts != 0 || lifecycleGeneration != 3 || !expiresAt.Equal(now.Add(5*time.Second+time.Hour)) {
		t.Fatalf("reopened Preview desired state = tenant:%s sha:%s desired:%s state:%s delivery:%s attempts:%d generation:%d expires:%s", tenant, sha, desired, state, lastDelivery, attempts, lifecycleGeneration, expiresAt)
	}
	var allocationRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM repository_preview_runtime_allocations
		WHERE tenant_id='tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=73`).Scan(&allocationRows); err != nil || allocationRows != 2 {
		t.Fatalf("PR head generation allocation rows = %d, err=%v; want old and reopened generations", allocationRows, err)
	}
	var oldDesired, oldState, currentDesired, currentState string
	if err := pool.QueryRow(ctx, `SELECT desired_state,state FROM repository_preview_runtime_allocations
		WHERE tenant_id='tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=73 AND generation=1`).Scan(&oldDesired, &oldState); err != nil || oldDesired != "absent" || oldState != "cleanup_pending" {
		t.Fatalf("closed Preview generation remains eligible to run: desired=%s state=%s err=%v", oldDesired, oldState, err)
	}
	if err := pool.QueryRow(ctx, `SELECT desired_state,state FROM repository_preview_runtime_allocations
		WHERE tenant_id='tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=73 AND generation=3`).Scan(&currentDesired, &currentState); err != nil || currentDesired != "present" || currentState != "awaiting_runtime" {
		t.Fatalf("reopened Preview generation was not independently recorded: desired=%s state=%s err=%v", currentDesired, currentState, err)
	}
	provisionTime := now.Add(10 * time.Second)
	provisioner := &previewProvisionFake{failures: []error{previewprovision.ErrMicroVMLaunch}, binding: previewstore.RuntimeBinding{
		ResourcePlaneID: "rp_preview_01", WorkspaceID: "ws_preview_01", MicroVMID: "microvm_preview_01", GatewayRouteID: "route_preview_01"}}
	workerNow := provisionTime
	claimIDs := []string{"preview-provision-a1", "preview-provision-early", "preview-provision-a2"}
	worker := previewprovision.Worker{Pool: pool, Provisioner: provisioner, Lease: 30 * time.Second, Timeout: 5 * time.Second, BatchSize: 1,
		Now: func() time.Time { return workerNow }, NewClaimID: func() (string, error) {
			claimID := claimIDs[0]
			claimIDs = claimIDs[1:]
			return claimID, nil
		}}
	firstStats, err := worker.RunOnce(ctx)
	if err != nil || firstStats.Claimed != 1 || firstStats.RetryScheduled != 1 || len(provisioner.jobs) != 1 || provisioner.jobs[0].Attempt != 1 {
		t.Fatalf("first Preview provision worker pass = %+v, jobs=%+v, err=%v", firstStats, provisioner.jobs, err)
	}
	firstProvisionJob := provisioner.jobs[0]
	var provisionRetryAt time.Time
	var retryError string
	if err := pool.QueryRow(ctx, `SELECT provision_next_attempt_at,provision_last_error FROM repository_preview_runtime_allocations
		WHERE tenant_id='tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=73 AND generation=3`).Scan(&provisionRetryAt, &retryError); err != nil || retryError != "microvm_launch_failed" || !provisionRetryAt.Equal(provisionTime.Add(5*time.Second)) {
		t.Fatalf("Preview provision retry projection = %s %q err=%v", provisionRetryAt, retryError, err)
	}
	workerNow = provisionRetryAt.Add(-time.Second)
	earlyStats, err := worker.RunOnce(ctx)
	if err != nil || earlyStats.Claimed != 0 {
		t.Fatalf("Preview provision retried before backoff: stats=%+v err=%v", earlyStats, err)
	}
	workerNow = provisionRetryAt
	secondStats, err := worker.RunOnce(ctx)
	if err != nil || secondStats.Claimed != 1 || secondStats.Bound != 1 || len(provisioner.jobs) != 2 || provisioner.jobs[1].Attempt != 2 {
		t.Fatalf("second Preview provision worker pass = %+v jobs=%+v err=%v", secondStats, provisioner.jobs, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := previewstore.BindRuntimeAllocation(ctx, tx, firstProvisionJob,
		provisioner.binding, provisionRetryAt.Add(time.Second))
	if err != nil || stale {
		_ = tx.Rollback(ctx)
		t.Fatalf("superseded Preview provision claim bound runtime: bound=%v err=%v", stale, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM repository_previews WHERE tenant_id='tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=73`).Scan(&state); err != nil || state != "awaiting_runtime" {
		t.Fatalf("Preview lifecycle state after runtime binding = %q, err=%v", state, err)
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
	var cleanupGenerations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM repository_preview_runtime_allocations
		WHERE tenant_id='tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=73
		  AND desired_state='absent' AND state='cleanup_pending'`).Scan(&cleanupGenerations); err != nil || cleanupGenerations != 2 {
		t.Fatalf("expiry did not queue all Preview runtime generations for cleanup: pending=%d err=%v", cleanupGenerations, err)
	}
	applyPreview := func(number int64, action, prState, deliveryID string, eventAt time.Time) (bool, error) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return false, err
		}
		updated, err := previewstore.ApplyPullRequestDelivery(ctx, tx, "tenant_preview_hook", deliveryID,
			githubwebhook.RepositoryEvent{GitHubEvent: "pull_request", Action: action, InstallationID: 9801,
				Repository:  &githubwebhook.RepositoryRef{GitHubID: 9802},
				PullRequest: &githubwebhook.PullRequestRef{Number: number, State: prState, HeadRef: "feature/quota", HeadSHA: strings.Repeat("d", 40), UpdatedAt: eventAt}},
			eventAt, eventAt)
		if err != nil {
			_ = tx.Rollback(ctx)
			return false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return updated, nil
	}
	if updated, err := applyPreview(75, "opened", "open", "30000000-0000-4000-8000-000000000001", now.Add(4*time.Hour)); err != nil || !updated {
		t.Fatalf("record quota-blocked PR: updated=%v err=%v", updated, err)
	}
	var quotaDesired, quotaState string
	if err := pool.QueryRow(ctx, `SELECT desired_state,state FROM repository_previews WHERE tenant_id='tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=75`).Scan(&quotaDesired, &quotaState); err != nil || quotaDesired != "absent" || quotaState != "quota_exceeded" {
		t.Fatalf("concurrent Preview over Tenant quota = desired:%s state:%s err=%v", quotaDesired, quotaState, err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		_, _, err := workloads.CreateOrGet(ctx, tx, domain.WorkloadRun{ID: "run_preview_shares_pr_quota", TenantID: "tenant_preview_hook",
			Kind: domain.WorkloadPreview, RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted,
			IdempotencyKey: "run_preview_shares_pr_quota", TimeoutSeconds: 1800, CreatedAt: now.Add(4 * time.Hour)})
		return struct{}{}, err
	}); !errors.Is(err, workloads.ErrPreviewQuotaExceeded) {
		t.Fatalf("Workload Preview should share the active PR Preview quota: %v", err)
	}
	usage, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (workspaces.QuotaUsage, error) {
		return workspaces.ReadQuotaUsage(ctx, tx, "tenant_preview_hook")
	})
	if err != nil || usage.PreviewRunsUsed != 1 || usage.PreviewRunsLimit != 1 {
		t.Fatalf("Tenant Preview usage should include cleanup-pending PR reservation: usage=%+v err=%v", usage, err)
	}
	if updated, err := applyPreview(75, "closed", "closed", "30000000-0000-4000-8000-000000000002", now.Add(4*time.Hour+time.Second)); err != nil || !updated {
		t.Fatalf("close quota-rejected PR: updated=%v err=%v", updated, err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM repository_previews WHERE tenant_id='tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=75`).Scan(&quotaState); err != nil || quotaState != "cleaned" {
		t.Fatalf("closed quota-rejected PR should require no physical cleanup: state=%q err=%v", quotaState, err)
	}
	if err := pool.QueryRow(ctx, `SELECT generation,head_sha FROM repository_previews WHERE tenant_id='tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=73`).Scan(&lifecycleGeneration, &sha); err != nil || lifecycleGeneration != 4 || sha != newSHA {
		t.Fatalf("expiry generation/head = generation:%d sha:%s err=%v, want 4 and latest SHA", lifecycleGeneration, sha, err)
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
	if err != nil || !claimed || firstJob.Attempt != 1 || firstJob.Generation != 4 || firstJob.LeaseExpiresAt != cleanupNow.Add(30*time.Second) {
		t.Fatalf("first Preview cleanup claim = %+v claimed=%v error=%v", firstJob, claimed, err)
	}
	staleGenerationJob := firstJob
	staleGenerationJob.Generation--
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, updated, err := previewstore.RecordCleanupFailure(ctx, tx, staleGenerationJob, cleanupNow.Add(time.Second), "provider_unavailable"); err != nil || updated {
		_ = tx.Rollback(ctx)
		t.Fatalf("stale Preview generation recorded cleanup failure: updated=%v error=%v", updated, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err = pool.Begin(ctx)
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
	staleGenerationCompletion := thirdJob
	staleGenerationCompletion.Generation--
	if stale, err := previewstore.MarkCleaned(ctx, tx, staleGenerationCompletion, cleanupNow.Add(18*time.Second)); err != nil || stale {
		_ = tx.Rollback(ctx)
		t.Fatalf("stale Preview generation completed cleanup: cleaned=%v error=%v", stale, err)
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
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM repository_preview_runtime_allocations
		WHERE tenant_id='tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=73
		  AND state<>'cleaned'`).Scan(&cleanupGenerations); err != nil || cleanupGenerations != 0 {
		t.Fatalf("confirmed Preview cleanup left live generation records: count=%d err=%v", cleanupGenerations, err)
	}
	usage, err = transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (workspaces.QuotaUsage, error) {
		return workspaces.ReadQuotaUsage(ctx, tx, "tenant_preview_hook")
	})
	if err != nil || usage.PreviewRunsUsed != 0 {
		t.Fatalf("confirmed Preview cleanup should release its Tenant quota slot: usage=%+v err=%v", usage, err)
	}
	if updated, err := applyPreview(75, "reopened", "open", "30000000-0000-4000-8000-000000000003", now.Add(5*time.Hour)); err != nil || !updated {
		t.Fatalf("reopen quota-rejected PR after cleanup releases its slot: updated=%v err=%v", updated, err)
	}
	if err := pool.QueryRow(ctx, `SELECT desired_state,state FROM repository_previews WHERE tenant_id='tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=75`).Scan(&quotaDesired, &quotaState); err != nil || quotaDesired != "present" || quotaState != "awaiting_runtime" {
		t.Fatalf("reopened Preview after cleanup did not acquire quota: desired=%s state=%s err=%v", quotaDesired, quotaState, err)
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

func TestRepositoryPreviewConcurrentAdmissionRespectsTenantQuota(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, query := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_preview_race','Preview race',$1)`,
		`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_preview_race','preview-race','',$1)`,
		`INSERT INTO tenant_quota_limits(tenant_id,workspace_limit,concurrent_workload_run_limit,preview_run_limit,preview_lifetime_seconds,revision,updated_by,updated_at) VALUES ('tenant_preview_race',10,5,1,3600,1,'usr_preview_race',$1)`,
		`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_preview_race',9803,'acme','active','usr_preview_race',$1,$1)`,
		`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (9803,'tenant_preview_race',$1)`,
		`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,preview_enabled,preview_enabled_at) VALUES ('tenant_preview_race',9803,9804,'acme','preview-race','main',$1,true,$1)`,
	} {
		if _, err := pool.Exec(ctx, query, now); err != nil {
			t.Fatalf("seed concurrent Preview quota case: %v", err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for _, number := range []int64{81, 82} {
		number := number
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
				return previewstore.ApplyPullRequestDelivery(ctx, tx, "tenant_preview_race",
					fmt.Sprintf("50000000-0000-4000-8000-%012d", number), githubwebhook.RepositoryEvent{
						GitHubEvent: "pull_request", Action: "opened", InstallationID: 9803,
						Repository:  &githubwebhook.RepositoryRef{GitHubID: 9804},
						PullRequest: &githubwebhook.PullRequestRef{Number: number, State: "open", HeadRef: "feature/race", HeadSHA: strings.Repeat("e", 40), UpdatedAt: now}}, now, now)
			})
			results <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent Preview admission: %v", err)
		}
	}
	var admitted, rejected int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE desired_state='present' AND state='awaiting_runtime'),
		COUNT(*) FILTER (WHERE desired_state='absent' AND state='quota_exceeded')
		FROM repository_previews WHERE tenant_id='tenant_preview_race'`).Scan(&admitted, &rejected); err != nil || admitted != 1 || rejected != 1 {
		t.Fatalf("concurrent Preview quota outcomes admitted=%d rejected=%d err=%v", admitted, rejected, err)
	}
}

func TestRepositoryPreviewQuotaConfigurationIncludesPRReservations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, query := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_preview_config','Preview config',$1)`,
		`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_preview_config','preview-config','',$1)`,
		`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_preview_config',9805,'acme','active','usr_preview_config',$1,$1)`,
		`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (9805,'tenant_preview_config',$1)`,
		`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,preview_enabled,preview_enabled_at) VALUES ('tenant_preview_config',9805,9806,'acme','preview-config','main',$1,true,$1)`,
	} {
		if _, err := pool.Exec(ctx, query, now); err != nil {
			t.Fatalf("seed Preview quota configuration case: %v", err)
		}
	}
	setLimits := func(previewRuns int16, lifetimeSeconds int64, at time.Time) error {
		_, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
			return quotas.SetWithPreview(ctx, tx, "tenant_preview_config", "usr_preview_config", quotas.DefaultWorkspaces,
				quotas.DefaultConcurrentWorkloadRuns, previewRuns, lifetimeSeconds, quotas.DefaultArtifactBytes,
				quotas.DefaultWorkspaceServiceMemoryBytes, quotas.DefaultWorkspaceServiceMilliCPU, at)
		})
		return err
	}
	if err := setLimits(2, 3600, now); err != nil {
		t.Fatalf("configure initial Preview quota: %v", err)
	}
	for index, number := range []int64{91, 92} {
		deliveryID := fmt.Sprintf("60000000-0000-4000-8000-%012d", index+1)
		if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
			return previewstore.ApplyPullRequestDelivery(ctx, tx, "tenant_preview_config", deliveryID,
				githubwebhook.RepositoryEvent{GitHubEvent: "pull_request", Action: "opened", InstallationID: 9805,
					Repository:  &githubwebhook.RepositoryRef{GitHubID: 9806},
					PullRequest: &githubwebhook.PullRequestRef{Number: number, State: "open", HeadRef: "feature/config", HeadSHA: strings.Repeat("f", 40), UpdatedAt: now}}, now, now)
		}); err != nil {
			t.Fatalf("create active PR Preview %d: %v", number, err)
		}
	}
	usage, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (workspaces.QuotaUsage, error) {
		return workspaces.ReadQuotaUsage(ctx, tx, "tenant_preview_config")
	})
	if err != nil || usage.PreviewRunsUsed != 2 || usage.PreviewRunsLimit != 2 {
		t.Fatalf("active PR Preview usage = %+v, error=%v; want two reservations", usage, err)
	}
	if err := setLimits(1, 3600, now.Add(time.Second)); !errors.Is(err, quotas.ErrBelowCurrentUsage) {
		t.Fatalf("lowering Preview run limit below active PR reservations = %v, want ErrBelowCurrentUsage", err)
	}
	if err := setLimits(2, 1800, now.Add(2*time.Second)); !errors.Is(err, quotas.ErrBelowCurrentUsage) {
		t.Fatalf("lowering Preview lifetime below active PR expiries = %v, want ErrBelowCurrentUsage", err)
	}
	stored, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
		return quotas.Read(ctx, tx, "tenant_preview_config")
	})
	if err != nil || stored.PreviewRuns != 2 || stored.PreviewLifetimeSeconds != 3600 {
		t.Fatalf("rejected quota update changed configured limits: %+v, error=%v", stored, err)
	}
}

type previewProvisionFake struct {
	failures []error
	binding  previewstore.RuntimeBinding
	jobs     []previewstore.ProvisionJob
	cleaned  int
}

func (f *previewProvisionFake) Provision(_ context.Context, job previewstore.ProvisionJob) (previewstore.RuntimeBinding, error) {
	f.jobs = append(f.jobs, job)
	if len(f.failures) > 0 {
		err := f.failures[0]
		f.failures = f.failures[1:]
		return previewstore.RuntimeBinding{}, err
	}
	return f.binding, nil
}

func (f *previewProvisionFake) Compensate(context.Context, previewstore.ProvisionJob, previewstore.RuntimeBinding) error {
	f.cleaned++
	return nil
}
