//go:build integration

package workloadjobs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/dsql"
	webhookstore "github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestWebhookProcessorCreatesOneUntrustedRunCheckAndOutboxForEnabledRepository(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, query := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_webhook_trigger','Webhook Trigger', $1)`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_webhook_trigger','webhook-trigger','', $1)`,
		`INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_webhook_trigger',9501,'acme','active','usr_webhook_trigger',$1,$1)`,
		`INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at) VALUES (9501,'tenant_webhook_trigger',$1)`,
	} {
		if _, err := pool.Exec(ctx, query, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_github_repositories (tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,factory_enabled,factory_enabled_at,synchronized_at) VALUES ('tenant_webhook_trigger',9501,9502,'acme','api','main',true,$1,$2)`, now.Add(-time.Second), now); err != nil {
		t.Fatal(err)
	}
	delivery := githubwebhook.VerifiedDelivery{IntegrationID: "shared", DeliveryID: "f2b2c3d4-e5f6-4789-8abc-def012345678", Event: "push",
		Payload: []byte(`{"ref":"refs/heads/main","after":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","installation":{"id":9501},"repository":{"id":9502,"name":"api","default_branch":"main","owner":{"login":"acme"}}}`)}
	if inserted, err := (webhookstore.Inbox{Pool: pool}).Record(ctx, delivery, now); err != nil || !inserted {
		t.Fatalf("record push delivery: inserted=%v error=%v", inserted, err)
	}
	creator := func(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, integrationID, deliveryID string, createdAt time.Time) (bool, error) {
		return CreateWebhookRepositoryRun(ctx, tx, tenantID, integrationID, deliveryID, createdAt)
	}
	stats, err := webhookstore.ProcessBatchWithRepositoryTriggers(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(time.Second), creator)
	if err != nil || stats.Claimed != 1 || stats.Processed != 1 || stats.WorkloadRunsCreated != 1 {
		t.Fatalf("trigger batch = %+v, error = %v", stats, err)
	}
	var runID, trust, state, commitSHA, ref, triggerEvent, triggerBranch string
	if err := pool.QueryRow(ctx, `SELECT id,trust_level,state,commit_sha,ref,trigger_event,trigger_branch FROM workload_runs WHERE tenant_id='tenant_webhook_trigger'`).Scan(&runID, &trust, &state, &commitSHA, &ref, &triggerEvent, &triggerBranch); err != nil {
		t.Fatal(err)
	}
	if trust != "untrusted" || state != "pending" || commitSHA != strings.Repeat("a", 40) || ref != "refs/heads/main" || triggerEvent != "push" || triggerBranch != "main" {
		t.Fatalf("scheduled run identity/policy = id=%s trust=%s state=%s sha=%s ref=%s trigger=%s/%s", runID, trust, state, commitSHA, ref, triggerEvent, triggerBranch)
	}
	var checkCount, checkOutboxCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_github_checks WHERE tenant_id='tenant_webhook_trigger' AND workload_run_id=$1`, runID).Scan(&checkCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='workload_run.github_check_requested'`, runID).Scan(&checkOutboxCount); err != nil {
		t.Fatal(err)
	}
	var deliveryStatus string
	if err := pool.QueryRow(ctx, `SELECT processing_status FROM github_webhook_deliveries WHERE integration_id='shared' AND delivery_id=$1`, delivery.DeliveryID).Scan(&deliveryStatus); err != nil {
		t.Fatal(err)
	}
	if checkCount != 1 || checkOutboxCount != 1 || deliveryStatus != "processed" {
		t.Fatalf("Check/outbox/delivery state = checks:%d outbox:%d delivery:%s", checkCount, checkOutboxCount, deliveryStatus)
	}
	duplicate, err := webhookstore.ProcessBatchWithRepositoryTriggers(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(2*time.Second), creator)
	if err != nil || duplicate.WorkloadRunsCreated != 0 || duplicate.Processed != 0 {
		t.Fatalf("duplicate trigger batch = %+v, error = %v", duplicate, err)
	}
	prDelivery := githubwebhook.VerifiedDelivery{IntegrationID: "shared", DeliveryID: "b2b2c3d4-e5f6-4789-8abc-def012345678", Event: "pull_request", Action: "opened",
		Payload: []byte(`{"action":"opened","installation":{"id":9501},"repository":{"id":9502,"name":"api","default_branch":"main","owner":{"login":"acme"}},"pull_request":{"number":42,"state":"open","head":{"ref":"feature/add-api","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"base":{"ref":"release/2026.10"}}}`)}
	if inserted, err := (webhookstore.Inbox{Pool: pool}).Record(ctx, prDelivery, now.Add(2*time.Second)); err != nil || !inserted {
		t.Fatalf("record pull-request delivery: inserted=%v error=%v", inserted, err)
	}
	prBatch, err := webhookstore.ProcessBatchWithRepositoryTriggers(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(3*time.Second), creator)
	if err != nil || prBatch.Processed != 1 || prBatch.WorkloadRunsCreated != 1 {
		t.Fatalf("process pull-request trigger = %+v, error=%v", prBatch, err)
	}
	var prTrigger, prBranch, prHeadRef, prCommit string
	if err := pool.QueryRow(ctx, `SELECT trigger_event,trigger_branch,ref,commit_sha FROM workload_runs WHERE tenant_id='tenant_webhook_trigger' AND id<>$1`, runID).Scan(&prTrigger, &prBranch, &prHeadRef, &prCommit); err != nil {
		t.Fatal(err)
	}
	if prTrigger != "pull_request" || prBranch != "release/2026.10" || prHeadRef != "feature/add-api" || prCommit != strings.Repeat("b", 40) {
		t.Fatalf("persisted pull-request trigger = %s/%s ref=%s sha=%s", prTrigger, prBranch, prHeadRef, prCommit)
	}
}

func TestWebhookProcessorConsumesEnabledEventWithoutSchedulingWhenRepositoryOptedOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, query := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_webhook_optout','Webhook Opt Out',$1)`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_webhook_optout','webhook-optout','',$1)`,
		`INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_webhook_optout',9601,'acme','active','usr_webhook_optout',$1,$1)`,
		`INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at) VALUES (9601,'tenant_webhook_optout',$1)`,
		`INSERT INTO tenant_github_repositories (tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,factory_enabled,synchronized_at) VALUES ('tenant_webhook_optout',9601,9602,'acme','api','main',false,$1)`,
	} {
		if _, err := pool.Exec(ctx, query, now); err != nil {
			t.Fatal(err)
		}
	}
	delivery := githubwebhook.VerifiedDelivery{IntegrationID: "shared", DeliveryID: "a3b2c3d4-e5f6-4789-8abc-def012345678", Event: "pull_request", Action: "opened",
		Payload: []byte(`{"action":"opened","installation":{"id":9601},"repository":{"id":9602,"name":"api","default_branch":"main","owner":{"login":"acme"}},"pull_request":{"number":1,"state":"open","head":{"ref":"feature/api","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"base":{"ref":"main"}}}`)}
	if inserted, err := (webhookstore.Inbox{Pool: pool}).Record(ctx, delivery, now); err != nil || !inserted {
		t.Fatalf("record opt-out PR delivery: inserted=%v error=%v", inserted, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_repositories SET factory_enabled=true,factory_enabled_at=$3 WHERE tenant_id=$1 AND installation_id=$2`, "tenant_webhook_optout", int64(9601), now.Add(time.Second)); err != nil {
		t.Fatalf("enable repository after delivery receipt: %v", err)
	}
	stats, err := webhookstore.ProcessBatchWithRepositoryTriggers(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(time.Second), CreateWebhookRepositoryRun)
	if err != nil || stats.Processed != 1 || stats.WorkloadRunsCreated != 0 {
		t.Fatalf("opt-out trigger batch = %+v, error = %v", stats, err)
	}
	var runs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_runs WHERE tenant_id='tenant_webhook_optout'`).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("opt-out repository created %d Workload Runs, error=%v", runs, err)
	}
}
