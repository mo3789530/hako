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
		{"10000000-0000-4000-8000-000000000003", "closed", "closed", oldSHA, now.Add(2 * time.Second), now.Add(time.Second)},
		{"10000000-0000-4000-8000-000000000004", "reopened", "open", newSHA, now.Add(3 * time.Second), now.Add(2 * time.Second)},
	}
	for _, item := range deliveries {
		delivery := githubwebhook.VerifiedDelivery{IntegrationID: "shared", DeliveryID: item.id, Event: "pull_request", Action: item.action,
			Payload: []byte(fmt.Sprintf(`{"action":%q,"installation":{"id":9801},"repository":{"id":9802,"name":"preview-app","default_branch":"main","owner":{"login":"acme"}},"pull_request":{"number":73,"state":%q,"updated_at":%q,"head":{"ref":"feature/preview","sha":%q},"base":{"ref":"main"}}}`,
				item.action, item.state, item.updated.Format(time.RFC3339), item.sha))}
		if inserted, err := (webhookstore.Inbox{Pool: pool}).Record(ctx, delivery, item.received); err != nil || !inserted {
			t.Fatalf("record %s delivery: inserted=%v error=%v", item.action, inserted, err)
		}
	}
	for _, item := range deliveries {
		stats, err := webhookstore.ProcessBatchWithRepositoryPreviews(ctx, pool, transaction.DefaultPolicy(), 10, item.received.Add(time.Second), nil, ApplyWebhookRepositoryPreview)
		if err != nil {
			t.Fatalf("process %s: stats=%+v error=%v", item.action, stats, err)
		}
	}
	var tenant, sha, desired, state, lastDelivery string
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT tenant_id,head_sha,desired_state,state,last_delivery_id,cleanup_attempts FROM repository_previews WHERE tenant_id='tenant_preview_hook' AND installation_id=9801 AND github_repository_id=9802 AND pull_request_number=73`).
		Scan(&tenant, &sha, &desired, &state, &lastDelivery, &attempts); err != nil {
		t.Fatal(err)
	}
	if tenant != "tenant_preview_hook" || sha != newSHA || desired != "present" || state != "awaiting_runtime" ||
		lastDelivery != deliveries[3].id || attempts != 0 {
		t.Fatalf("reopened Preview desired state = tenant:%s sha:%s desired:%s state:%s delivery:%s attempts:%d", tenant, sha, desired, state, lastDelivery, attempts)
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
