//go:build integration

package githubregistry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestTenantAppMigrationDryRunBackfillAndDefaultCutover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_migration','Migration',now())`,
		`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_migration','migration-sub','',now())`,
		`INSERT INTO github_credential_bindings(id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id,oauth_reader_role_arn,oauth_secret_version_id,webhook_secret_reader_role_arn,webhook_secret_version_id) VALUES ('binding_migration','tenant_migration',771,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:private','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:oauth','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:webhook',now(),'arn:aws:iam::123456789012:role/hako/private','11111111111111111111111111111111','arn:aws:iam::123456789012:role/hako/oauth','22222222222222222222222222222222','arn:aws:iam::123456789012:role/hako/webhook','33333333333333333333333333333333')`,
		`INSERT INTO github_credential_bindings(id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at) VALUES ('binding_previous','tenant_migration',770,'approved','private-old','oauth-old','webhook-old',now())`,
		`INSERT INTO github_app_registrations(id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_migration',771,'tenant_owned','tenant_migration','migration-app','client',now())`,
		`INSERT INTO github_app_registrations(id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_previous',770,'tenant_owned','tenant_migration','previous-app','old-client',now())`,
		`INSERT INTO tenant_github_integrations(id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_migration','tenant_migration','registration_migration',771,'binding_migration','ready',4,9,'migration-key','migration-hash','usr_migration',now(),now())`,
		`INSERT INTO tenant_github_integrations(id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_previous','tenant_migration','registration_previous',770,'binding_previous','active',2,3,'previous-key','previous-hash','usr_migration',now(),now())`,
		`INSERT INTO tenant_github_default_integrations(tenant_id,integration_id,revision) VALUES ('tenant_migration','integration_previous',2)`,
		`INSERT INTO tenant_github_installations(tenant_id,installation_id,github_app_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_migration',77101,771,'acme','active','usr_migration',now(),now()),('tenant_migration',77102,772,'other','active','usr_migration',now(),now())`,
		`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (77101,'tenant_migration',now()),(77102,'tenant_migration',now())`,
		`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_migration',77103,'historical','active','usr_migration',now(),now())`,
		`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (77103,'tenant_migration',now())`,
		`INSERT INTO workload_runs(id,tenant_id,kind,requested_by,ref,runtime_class,trust_level,desired_state,state,idempotency_key,request_hash,timeout_seconds,timeout_at,created_at,updated_at) VALUES ('run_legacy_migration','tenant_migration','job','usr_migration','main','standard','trusted','running','pending','legacy-key','legacy-hash',3600,now()+interval '1 hour',now(),now())`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	selectDefault := func(revision, generation int64) error {
		_, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (AppIntegration, error) {
			return SelectDefaultAppGeneration(ctx, tx, "tenant_migration", "integration_migration", revision, generation, now)
		})
		return err
	}
	if err := selectDefault(4, 9); !errors.Is(err, ErrLegacyRunsNotDrained) {
		t.Fatalf("legacy active Run did not block activation: %v", err)
	}
	if err := selectDefault(3, 9); !errors.Is(err, ErrAppConflict) {
		t.Fatalf("stale revision accepted: %v", err)
	}
	if err := selectDefault(4, 8); !errors.Is(err, ErrAppConflict) {
		t.Fatalf("stale credential generation accepted: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workload_runs SET desired_state='cancelled',state='cancelled',completed_at=now() WHERE id='run_legacy_migration'`); err != nil {
		t.Fatal(err)
	}
	active, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (AppIntegration, error) {
		return SelectDefaultAppGeneration(ctx, tx, "tenant_migration", "integration_migration", 4, 9, now)
	})
	if err != nil || active.Status != "active" {
		t.Fatalf("ready App could not be selected after drain: %+v %v", active, err)
	}
	var previousStatus string
	var previousRevision, previousGeneration int64
	if err := pool.QueryRow(ctx, `SELECT status,config_revision,credential_generation FROM tenant_github_integrations WHERE id='integration_previous'`).Scan(&previousStatus, &previousRevision, &previousGeneration); err != nil || previousStatus != "disabled" || previousRevision != 3 || previousGeneration != 4 {
		t.Fatalf("previous owned integration was not retired during cutover: %s r%d g%d err=%v", previousStatus, previousRevision, previousGeneration, err)
	}
	request := TenantAppMigrationRequest{TenantID: "tenant_migration", IntegrationID: "integration_migration", ExpectedRevision: 4, ExpectedGeneration: 9, InstallationIDs: []int64{77101}}
	plan, err := PlanTenantAppMigration(ctx, pool, transaction.DefaultPolicy(), request)
	if err != nil || !plan.CanBackfill || len(plan.Installations) != 1 || plan.Installations[0].Disposition != "candidate" {
		t.Fatalf("dry-run did not report safe candidate: %+v %v", plan, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tenant_github_app_installation_bindings WHERE installation_id=77101`).Scan(&count); err != nil || count != 0 {
		t.Fatal("dry-run modified Installation bindings")
	}
	backfilled, err := BackfillTenantAppInstallationBindings(ctx, pool, transaction.DefaultPolicy(), request, now)
	if err != nil || backfilled.Installations[0].Disposition != "already_bound" {
		t.Fatalf("safe backfill failed: %+v %v", backfilled, err)
	}
	repeated, err := BackfillTenantAppInstallationBindings(ctx, pool, transaction.DefaultPolicy(), request, now.Add(time.Second))
	if err != nil || repeated.Installations[0].Disposition != "already_bound" {
		t.Fatalf("idempotent backfill failed: %+v %v", repeated, err)
	}
	conflictRequest := request
	conflictRequest.InstallationIDs = []int64{77101, 77102}
	conflictPlan, err := PlanTenantAppMigration(ctx, pool, transaction.DefaultPolicy(), conflictRequest)
	if err != nil || conflictPlan.CanBackfill || conflictPlan.Installations[1].Disposition != "blocked_app_id_mismatch" {
		t.Fatalf("App identity mismatch was not blocked: %+v %v", conflictPlan, err)
	}
	if _, err := BackfillTenantAppInstallationBindings(ctx, pool, transaction.DefaultPolicy(), conflictRequest, now); !errors.Is(err, ErrTenantAppMigrationConflict) {
		t.Fatalf("conflicting batch backfill was accepted: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tenant_github_app_installation_bindings WHERE installation_id=77102`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed mixed batch partially committed")
	}
	unknownIdentity := request
	unknownIdentity.InstallationIDs = []int64{77103}
	unknownPlan, err := PlanTenantAppMigration(ctx, pool, transaction.DefaultPolicy(), unknownIdentity)
	if err != nil || unknownPlan.CanBackfill || unknownPlan.Installations[0].Disposition != "blocked_app_identity_unverified" {
		t.Fatalf("historical App identity was inferred: %+v %v", unknownPlan, err)
	}
	var integration, revision, generation any
	if err := pool.QueryRow(ctx, `SELECT github_integration_id,github_config_revision,github_credential_generation FROM workload_runs WHERE id='run_legacy_migration'`).Scan(&integration, &revision, &generation); err != nil {
		t.Fatal(err)
	}
	if integration != nil || revision != nil || generation != nil {
		t.Fatal("cutover rewrote an existing legacy Run pin")
	}
}

func TestTenantAppMigrationValidationRequiresExplicitUniqueInstallations(t *testing.T) {
	for _, request := range []TenantAppMigrationRequest{
		{TenantID: "tenant", IntegrationID: "integration", ExpectedRevision: 1, ExpectedGeneration: 1},
		{TenantID: "tenant", IntegrationID: "integration", ExpectedRevision: 1, ExpectedGeneration: 1, InstallationIDs: []int64{10, 10}},
		{TenantID: "tenant", IntegrationID: "integration", ExpectedRevision: 1, ExpectedGeneration: 1, InstallationIDs: []int64{0}},
	} {
		if err := validateTenantAppMigrationRequest(request); !errors.Is(err, ErrTenantAppMigrationInvalid) {
			t.Fatalf("invalid migration request accepted: %+v", request)
		}
	}
}
