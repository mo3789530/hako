//go:build integration

package previews

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestRepositoryPreviewListingAndExpiryBatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, query := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_preview_list','Preview list',$1)`,
		`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_preview_list','preview-list','',$1)`,
		`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_preview_list',9921,'acme','active','usr_preview_list',$1,$1)`,
		`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (9921,'tenant_preview_list',$1)`,
		`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,preview_enabled,preview_enabled_at) VALUES ('tenant_preview_list',9921,9922,'acme','preview-list','main',$1,true,$1)`,
	} {
		if _, err := pool.Exec(ctx, query, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, preview := range []struct {
		pr      int
		sha     string
		updated time.Time
		expires time.Time
	}{
		{pr: 41, sha: strings.Repeat("a", 40), updated: now, expires: now.Add(-time.Second)},
		{pr: 42, sha: strings.Repeat("b", 40), updated: now.Add(time.Second), expires: now.Add(time.Hour)},
	} {
		_, err := pool.Exec(ctx, `INSERT INTO repository_previews
			(tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,desired_state,state,last_delivery_id,last_event_at,expires_at,created_at,updated_at)
			VALUES ('tenant_preview_list',9921,9922,$1,$2,'feature/preview','present','awaiting_runtime',$3,$4,$5,$4,$4)`,
			preview.pr, preview.sha, previewDeliveryID(preview.pr), preview.updated, preview.expires)
		if err != nil {
			t.Fatal(err)
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	items, err := ListForRepository(ctx, tx, domain.TenantID("tenant_preview_list"), 9921, 9922, 1)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].PullRequestNumber != 42 || items[0].HeadSHA != strings.Repeat("b", 40) || items[0].Generation != 1 {
		t.Fatalf("bounded Repository Preview list = %+v", items)
	}
	for _, identity := range []struct {
		tenant       string
		installation int64
		repository   int64
	}{
		{tenant: "tenant_other", installation: 9921, repository: 9922},
		{tenant: "tenant_preview_list", installation: 1, repository: 9922},
		{tenant: "tenant_preview_list", installation: 9921, repository: 1},
	} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got, listErr := ListForRepository(ctx, tx, domain.TenantID(identity.tenant), identity.installation, identity.repository, 100)
		if listErr == nil {
			listErr = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(ctx)
		}
		if listErr != nil || len(got) != 0 {
			t.Fatalf("foreign Repository Preview list for %+v = %+v, %v", identity, got, listErr)
		}
	}
	if _, err := ListForRepository(ctx, nil, domain.TenantID("tenant_preview_list"), 9921, 9922, 10); err == nil {
		t.Fatal("Preview list accepted a nil transaction")
	}
	if _, err := ListForRepository(ctx, nil, domain.TenantID("tenant_preview_list"), 9921, 9922, 101); err == nil {
		t.Fatal("Preview list accepted an out-of-range limit")
	}

	if expired, err := ExpireBatch(ctx, pool, now, 1); err != nil || expired != 1 {
		t.Fatalf("ExpireBatch() = %d, %v; want one transition", expired, err)
	}
	if expired, err := ExpireBatch(ctx, pool, now, 1); err != nil || expired != 0 {
		t.Fatalf("ExpireBatch() repeated = %d, %v; want idempotent no-op", expired, err)
	}
	if _, err := ExpireBatch(ctx, pool, now, 0); err == nil {
		t.Fatal("ExpireBatch accepted a zero batch size")
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := ListForRepository(ctx, tx, domain.TenantID("tenant_preview_list"), 9921, 9922, 10)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 || remaining[0].State != "awaiting_runtime" || remaining[1].State != "cleanup_pending" {
		t.Fatalf("Preview expiry should preserve active record and queue only expired metadata: %+v", remaining)
	}
}

func TestPreviewRuntimeGenerationPinsTenantGitHubAppCredentialIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_preview_pin','Preview pin',$1)`, []any{now}},
		{`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_preview_pin','preview-pin','',$1)`, []any{now}},
		{`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at) VALUES ('preview_pin_credential','tenant_preview_pin',921,'approved','private','oauth','webhook',$1)`, []any{now}},
		{`INSERT INTO github_app_registrations (id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('preview_pin_app',921,'tenant_owned','tenant_preview_pin','preview-pin','client',$1)`, []any{now}},
		{`INSERT INTO tenant_github_integrations (id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('preview_pin_integration','tenant_preview_pin','preview_pin_app',921,'preview_pin_credential','active',3,8,'preview-pin-key','hash','usr_preview_pin',$1,$1)`, []any{now}},
		{`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_preview_pin',9931,'acme','active','usr_preview_pin',$1,$1)`, []any{now}},
		{`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (9931,'tenant_preview_pin',$1)`, []any{now}},
		{`INSERT INTO tenant_github_app_installation_bindings(tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_preview_pin','preview_pin_integration',9931,$1)`, []any{now}},
		{`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at) VALUES ('tenant_preview_pin',9931,9932,'acme','preview-pin','main',$1)`, []any{now}},
		{`INSERT INTO repository_previews(tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,desired_state,state,last_delivery_id,last_event_at,generation,expires_at,created_at,updated_at) VALUES ('tenant_preview_pin',9931,9932,9,$2,'feature/pin','present','awaiting_runtime','50000000-0000-4000-8000-000000000001',$1,1,$3,$1,$1)`, []any{now, strings.Repeat("a", 40), now.Add(time.Hour)}},
	} {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	runSync := func() {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := syncRuntimeAllocations(ctx, tx, CleanupIdentity{TenantID: "tenant_preview_pin", InstallationID: 9931,
			RepositoryID: 9932, PullRequestNumber: 9}, now); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	assertPin := func(generation int64, integration string, revision, credentialGeneration int64) {
		t.Helper()
		var gotIntegration *string
		var gotRevision, gotCredentialGeneration *int64
		if err := pool.QueryRow(ctx, `SELECT github_integration_id,github_config_revision,github_credential_generation
			FROM repository_preview_runtime_allocations WHERE tenant_id='tenant_preview_pin' AND installation_id=9931
			AND github_repository_id=9932 AND pull_request_number=9 AND generation=$1`, generation).
			Scan(&gotIntegration, &gotRevision, &gotCredentialGeneration); err != nil {
			t.Fatal(err)
		}
		if integration == "" {
			if gotIntegration != nil || gotRevision != nil || gotCredentialGeneration != nil {
				t.Fatalf("generation %d unexpectedly pinned App identity: %v %v %v", generation, gotIntegration, gotRevision, gotCredentialGeneration)
			}
			return
		}
		if gotIntegration == nil || *gotIntegration != integration || gotRevision == nil || *gotRevision != revision ||
			gotCredentialGeneration == nil || *gotCredentialGeneration != credentialGeneration {
			t.Fatalf("generation %d App pin = %v/%v/%v, want %s/%d/%d", generation, gotIntegration, gotRevision, gotCredentialGeneration,
				integration, revision, credentialGeneration)
		}
	}
	runSync()
	assertPin(1, "preview_pin_integration", 3, 8)
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_integrations SET config_revision=4,credential_generation=9 WHERE id='preview_pin_integration'`); err != nil {
		t.Fatal(err)
	}
	runSync()
	assertPin(1, "preview_pin_integration", 3, 8)
	if _, err := pool.Exec(ctx, `UPDATE repository_previews SET generation=2,head_sha=$1,head_ref='feature/pin-v2',updated_at=$2
		WHERE tenant_id='tenant_preview_pin' AND installation_id=9931 AND github_repository_id=9932 AND pull_request_number=9`,
		strings.Repeat("b", 40), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	runSync()
	assertPin(1, "preview_pin_integration", 3, 8)
	assertPin(2, "preview_pin_integration", 4, 9)
}

func previewDeliveryID(pullRequest int) string {
	return fmt.Sprintf("10000000-0000-4000-8000-%012x", pullRequest)
}
