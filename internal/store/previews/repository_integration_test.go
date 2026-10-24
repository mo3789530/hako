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

func previewDeliveryID(pullRequest int) string {
	return fmt.Sprintf("10000000-0000-4000-8000-%012x", pullRequest)
}
