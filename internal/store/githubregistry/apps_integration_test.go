//go:build integration

package githubregistry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestAppDraftApprovalIdempotencyOwnershipAndDisable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, query := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_app_a','A',now()),('tenant_app_b','B',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_app','app-sub','',now())`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at) VALUES ('binding_a','tenant_app_a',123,'approved','private-ref-a','oauth-ref-a','webhook-ref-a',now()),('binding_b','tenant_app_b',123,'approved','private-ref-b','oauth-ref-b','webhook-ref-b',now()),('binding_disabled','tenant_app_a',456,'disabled','private-ref-c','oauth-ref-c','webhook-ref-c',now())`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	request := AppDraftRequest{GitHubAppID: 123, Slug: "my-app", ClientID: "Iv1.client", CredentialBindingID: "binding_a"}
	type createdResult struct {
		app     AppIntegration
		created bool
	}
	create := func(tenant domain.TenantID, key string, request AppDraftRequest) (AppIntegration, bool, error) {
		id, err := idgen.New("gint_")
		if err != nil {
			return AppIntegration{}, false, err
		}
		reg, err := idgen.New("gapp_")
		if err != nil {
			return AppIntegration{}, false, err
		}
		out, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (createdResult, error) {
			app, created, err := CreateAppDraft(ctx, tx, tenant, "usr_app", id, reg, key, request, now)
			return createdResult{app, created}, err
		})
		return out.app, out.created, err
	}
	app, created, err := create("tenant_app_a", "key-one", request)
	if err != nil || !created || app.Status != "draft" || app.ConfigRevision != 1 {
		t.Fatal("approved App draft creation failed")
	}
	replayed, created, err := create("tenant_app_a", "key-one", request)
	if err != nil || created || replayed.ID != app.ID {
		t.Fatal("idempotent replay created a different integration")
	}
	changed := request
	changed.Slug = "other"
	if _, _, err := create("tenant_app_a", "key-one", changed); !errors.Is(err, ErrAppIdempotencyConflict) {
		t.Fatal("changed idempotent request accepted")
	}
	if _, _, err := create("tenant_app_a", "key-two", request); !errors.Is(err, ErrAppConflict) {
		t.Fatal("duplicate App integration accepted")
	}
	if _, _, err := create("tenant_app_b", "key-three", request); !errors.Is(err, ErrAppBindingDenied) {
		t.Fatal("other Tenant binding accepted")
	}
	changed = request
	changed.CredentialBindingID = "binding_b"
	if _, _, err := create("tenant_app_b", "key-four", changed); !errors.Is(err, ErrAppBindingDenied) {
		t.Fatal("same App was owned by two Tenants")
	}
	changed = request
	changed.GitHubAppID = 456
	changed.CredentialBindingID = "binding_disabled"
	if _, _, err := create("tenant_app_a", "key-five", changed); !errors.Is(err, ErrAppBindingDenied) {
		t.Fatal("disabled approval accepted")
	}
	encoded, _ := json.Marshal(app)
	if strings.Contains(string(encoded), "private-ref") || strings.Contains(string(encoded), "credential_binding") {
		t.Fatal("App response exposed credential references")
	}
	tx := mustBegin(t, pool, ctx)
	if _, err := GetApp(ctx, tx, "tenant_app_b", app.ID); !errors.Is(err, ErrAppNotFound) {
		t.Fatal("cross-Tenant detail was visible")
	}
	if _, err := DisableApp(ctx, tx, "tenant_app_a", app.ID, 99, now); !errors.Is(err, ErrAppConflict) {
		t.Fatal("stale revision accepted")
	}
	_ = tx.Rollback(ctx)
	updated, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (AppIntegration, error) {
		return DisableApp(ctx, tx, "tenant_app_a", app.ID, 1, now.Add(time.Second))
	})
	if err != nil || updated.Status != "disabled" || updated.ConfigRevision != 2 || updated.CredentialGeneration != 2 {
		t.Fatal("draft disable did not advance revision/generation")
	}
	tx = mustBegin(t, pool, ctx)
	defer tx.Rollback(ctx)
	items, err := ListApps(ctx, tx, "tenant_app_a", 50, 0)
	if err != nil || len(items) != 1 {
		t.Fatal("Tenant App list failed")
	}
	other, err := ListApps(ctx, tx, "tenant_app_b", 50, 0)
	if err != nil || len(other) != 0 {
		t.Fatal("App list crossed Tenant")
	}
	if _, err := ListApps(ctx, tx, "tenant_app_a", 101, 0); !errors.Is(err, ErrInvalidApp) {
		t.Fatal("unbounded page accepted")
	}
	var defaults int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tenant_github_default_integrations`).Scan(&defaults); err != nil || defaults != 0 {
		t.Fatal("draft changed active defaults")
	}
}

func TestConcurrentAppDraftDeliveryCreatesOneIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_concurrent','Concurrent',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_concurrent','concurrent-sub','',now())`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at) VALUES ('binding','tenant_concurrent',123,'approved','private-ref','oauth-ref','webhook-ref',now())`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Go(func() {
			id, _ := idgen.New("gint_")
			reg, _ := idgen.New("gapp_")
			_, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
				_, created, err := CreateAppDraft(ctx, tx, "tenant_concurrent", "usr_concurrent", id, reg, "same-key", AppDraftRequest{GitHubAppID: 123, Slug: "app", ClientID: "client", CredentialBindingID: "binding"}, time.Now().UTC())
				return created, err
			})
			if err != nil {
				t.Error("concurrent draft transaction failed")
				return
			}
		})
	}
	// Count the durable result instead of mutating a counter in OCC callbacks.
	wg.Wait()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tenant_github_integrations`).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate integration after concurrent delivery")
	}
}

func TestConcurrentDefaultSelectionKeepsOneTenantRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_default_race','Default race',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_default_race','default-race','',now())`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at) VALUES ('binding_default_a','tenant_default_race',801,'approved','private-a','oauth-a','webhook-a',now()),('binding_default_b','tenant_default_race',802,'approved','private-b','oauth-b','webhook-b',now())`,
		`INSERT INTO github_app_registrations (id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_default_a',801,'tenant_owned','tenant_default_race','app-a','client-a',now()),('registration_default_b',802,'tenant_owned','tenant_default_race','app-b','client-b',now())`,
		`INSERT INTO tenant_github_integrations (id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_default_a','tenant_default_race','registration_default_a',801,'binding_default_a','ready',1,1,'key-a','hash-a','usr_default_race',now(),now()),('integration_default_b','tenant_default_race','registration_default_b',802,'binding_default_b','ready',1,1,'key-b','hash-b','usr_default_race',now(),now())`} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, integrationID := range []string{"integration_default_a", "integration_default_b"} {
		integrationID := integrationID
		go func() {
			<-start
			_, err := pool.Exec(ctx, `INSERT INTO tenant_github_default_integrations (tenant_id,integration_id,revision) VALUES ('tenant_default_race',$1,1)`, integrationID)
			results <- err
		}()
	}
	close(start)
	succeeded, conflicted := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case strings.Contains(err.Error(), "duplicate key"):
			conflicted++
		default:
			t.Fatalf("concurrent default selection failed unexpectedly: %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("expected one default winner and one uniqueness conflict; got %d and %d", succeeded, conflicted)
	}
}
