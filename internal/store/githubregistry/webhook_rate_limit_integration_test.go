//go:build integration

package githubregistry

import (
	"context"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestWebhookAttemptLimitIsAtomicBoundedAndExpires(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_rate','rate',now())`,
		`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_rate','rate','',now())`,
		`INSERT INTO github_credential_bindings(id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at) VALUES ('binding_rate','tenant_rate',1,'approved','private','oauth','webhook',now())`,
		`INSERT INTO github_app_registrations(id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_rate',1,'tenant_owned','tenant_rate','rate-app','client',now())`,
		`INSERT INTO tenant_github_integrations(id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_rate','tenant_rate','registration_rate',1,'binding_rate','active',1,1,'key','hash','usr_rate',now(),now())`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now().UTC().Truncate(time.Microsecond)
	allow := func(id string, at time.Time) (bool, error) {
		return AllowWebhookAttempt(ctx, pool, transaction.DefaultPolicy(), id, at, 2)
	}
	for i, want := range []bool{true, true, false} {
		got, err := allow("integration_rate", start.Add(time.Duration(i)*time.Second))
		if err != nil || got != want {
			t.Fatalf("attempt %d allowed=%v err=%v want=%v", i+1, got, err, want)
		}
	}
	if got, err := allow("unknown_rate", start); err != nil || got {
		t.Fatalf("unknown route allowed=%v err=%v", got, err)
	}
	if got, err := allow("integration_rate", start.Add(TenantWebhookRateWindow+time.Second)); err != nil || !got {
		t.Fatalf("expired window did not reset: %v %v", got, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_integrations SET status='disabled' WHERE id='integration_rate'`); err != nil {
		t.Fatal(err)
	}
	if got, err := allow("integration_rate", start.Add(2*TenantWebhookRateWindow)); err != nil || got {
		t.Fatalf("disabled integration was allowed: %v %v", got, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM github_webhook_rate_limits`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("unknown routes created rate rows: rows=%d err=%v", rows, err)
	}
}
