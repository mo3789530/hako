//go:build integration

package workloads

import (
	"context"
	"errors"
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

func TestWebhookDeliveryCreatesOnePinnedWorkloadRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, statement := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_webhook_run','Webhook Run',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_webhook_run','webhook-run','',now())`,
		`INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_webhook_run',9401,'acme','active','usr_webhook_run',now(),now())`,
		`INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at) VALUES (9401,'tenant_webhook_run',now())`,
		`INSERT INTO tenant_github_repositories (tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at) VALUES ('tenant_webhook_run',9401,9402,'acme','api','main',now())`,
		`INSERT INTO github_app_registrations (id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_webhook_run',9403,'tenant_owned','tenant_webhook_run','webhook-run','client',now())`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at) VALUES ('binding_webhook_run','tenant_webhook_run',9403,'approved','private','oauth','webhook',now())`,
		`INSERT INTO tenant_github_integrations (id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_webhook_run','tenant_webhook_run','registration_webhook_run',9403,'binding_webhook_run','active',2,4,'webhook-run-key','` + strings.Repeat("a", 64) + `','usr_webhook_run',now(),now())`,
		`INSERT INTO tenant_github_app_installation_bindings (tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_webhook_run','integration_webhook_run',9401,now())`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	delivery := githubwebhook.VerifiedDelivery{IntegrationID: "integration_webhook_run", CredentialGeneration: 4,
		DeliveryID: "f1b2c3d4-e5f6-4789-8abc-def012345678", Event: "workflow_job", Action: "queued",
		Payload: []byte(`{"action":"queued","installation":{"id":9401,"app_id":9403},"repository":{"id":9402,"name":"api","owner":{"login":"acme"}},"workflow_job":{"id":9404,"run_id":9405,"name":"Claude Review","head_branch":"feature/review","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","status":"queued"}}`)}
	if inserted, err := (webhookstore.Inbox{Pool: pool}).Record(ctx, delivery, now); err != nil || !inserted {
		t.Fatalf("record workflow_job webhook: inserted=%v err=%v", inserted, err)
	}
	makeRun := func(id string) domain.WorkloadRun {
		return domain.WorkloadRun{ID: domain.WorkloadRunID(id), TenantID: "tenant_webhook_run", Kind: domain.WorkloadJob,
			RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted, TimeoutSeconds: 120,
			TimeoutAt: now.Add(120 * time.Second), CreatedAt: now}
	}
	type result struct {
		run     domain.WorkloadRun
		created bool
	}
	first, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (result, error) {
		run, created, err := CreateOrGetFromWebhookDelivery(ctx, tx, delivery.IntegrationID, delivery.DeliveryID, makeRun("run_webhook_delivery"))
		return result{run: run, created: created}, err
	})
	if err != nil || !first.created {
		t.Fatalf("first webhook Run = %+v err=%v", first, err)
	}
	wantKey := WebhookDeliveryIdempotencyKey(delivery.IntegrationID, delivery.DeliveryID)
	if first.run.IdempotencyKey != wantKey || first.run.GitHubIntegrationID != delivery.IntegrationID || first.run.GitHubCredentialGeneration != 4 ||
		first.run.GitHubInstallation != 9401 || first.run.GitHubRepository != 9402 || first.run.Ref != "feature/review" || first.run.CommitSHA != strings.Repeat("a", 40) {
		t.Fatalf("webhook Run did not pin delivery identity: %+v", first.run)
	}
	second, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (result, error) {
		run, created, err := CreateOrGetFromWebhookDelivery(ctx, tx, delivery.IntegrationID, delivery.DeliveryID, makeRun("run_webhook_duplicate"))
		return result{run: run, created: created}, err
	})
	if err != nil || second.created || second.run.ID != first.run.ID {
		t.Fatalf("duplicate webhook Run = %+v err=%v", second, err)
	}
	conflicting := makeRun("run_webhook_conflict")
	conflicting.TrustLevel = domain.WorkloadTrusted
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		_, _, err := CreateOrGetFromWebhookDelivery(ctx, tx, delivery.IntegrationID, delivery.DeliveryID, conflicting)
		return struct{}{}, err
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed policy reused a Delivery ID Run: %v", err)
	}
	var runs, checks, checkOutbox int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_runs WHERE tenant_id='tenant_webhook_run' AND idempotency_key=$1`, wantKey).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_github_checks WHERE tenant_id='tenant_webhook_run' AND workload_run_id=$1`, first.run.ID).Scan(&checks); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='workload_run.github_check_requested'`, first.run.ID).Scan(&checkOutbox); err != nil {
		t.Fatal(err)
	}
	if runs != 1 || checks != 1 || checkOutbox != 1 {
		t.Fatalf("duplicate webhook delivery created runs=%d checks=%d check_outbox=%d; want one each", runs, checks, checkOutbox)
	}
}
