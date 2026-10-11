package githubregistry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var (
	ErrTenantAppMigrationInvalid  = errors.New("invalid Tenant GitHub App migration request")
	ErrTenantAppMigrationConflict = errors.New("Tenant GitHub App migration conflicts with current bindings")
	ErrLegacyRunsNotDrained       = errors.New("legacy Workload Runs must be drained before selecting a Tenant GitHub App")
)

type TenantAppMigrationRequest struct {
	TenantID           domain.TenantID
	IntegrationID      string
	ExpectedRevision   int64
	ExpectedGeneration int64
	InstallationIDs    []int64
}

type TenantAppMigrationItem struct {
	InstallationID int64  `json:"installation_id"`
	Disposition    string `json:"disposition"`
}

type TenantAppMigrationPlan struct {
	TenantID                  domain.TenantID          `json:"tenant_id"`
	IntegrationID             string                   `json:"integration_id"`
	GitHubAppID               int64                    `json:"github_app_id"`
	ConfigRevision            int64                    `json:"config_revision"`
	CredentialGeneration      int64                    `json:"credential_generation"`
	LegacyNonterminalRunCount int64                    `json:"legacy_nonterminal_run_count"`
	Installations             []TenantAppMigrationItem `json:"installations"`
	CanBackfill               bool                     `json:"can_backfill"`
}

// PlanTenantAppMigration is read-only. It uses only explicit Installation IDs
// and reports whether database-recorded App identity is sufficient to repair
// a missing integration binding. Unknown historical App identity is never
// inferred from a Tenant, Repository name, or caller-supplied App ID.
func PlanTenantAppMigration(ctx context.Context, pool transaction.Beginner, policy transaction.Policy, request TenantAppMigrationRequest) (TenantAppMigrationPlan, error) {
	if err := validateTenantAppMigrationRequest(request); err != nil || pool == nil {
		return TenantAppMigrationPlan{}, ErrTenantAppMigrationInvalid
	}
	return transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (TenantAppMigrationPlan, error) {
		return PlanTenantAppMigrationTx(ctx, tx, request)
	})
}

func PlanTenantAppMigrationTx(ctx context.Context, tx pgx.Tx, request TenantAppMigrationRequest) (TenantAppMigrationPlan, error) {
	if tx == nil || validateTenantAppMigrationRequest(request) != nil {
		return TenantAppMigrationPlan{}, ErrTenantAppMigrationInvalid
	}
	return planTenantAppMigration(ctx, tx, request)
}

// BackfillTenantAppInstallationBindings applies only rows classified as safe
// by the same transaction's dry-run checks. Any mismatch rolls back all rows.
// It never changes a Workload Run pin or Tenant default.
func BackfillTenantAppInstallationBindings(ctx context.Context, pool transaction.Beginner, policy transaction.Policy, request TenantAppMigrationRequest, now time.Time) (TenantAppMigrationPlan, error) {
	if err := validateTenantAppMigrationRequest(request); err != nil || pool == nil || now.IsZero() {
		return TenantAppMigrationPlan{}, ErrTenantAppMigrationInvalid
	}
	return transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (TenantAppMigrationPlan, error) {
		return BackfillTenantAppInstallationBindingsTx(ctx, tx, request, now)
	})
}

func BackfillTenantAppInstallationBindingsTx(ctx context.Context, tx pgx.Tx, request TenantAppMigrationRequest, now time.Time) (TenantAppMigrationPlan, error) {
	if tx == nil || validateTenantAppMigrationRequest(request) != nil || now.IsZero() {
		return TenantAppMigrationPlan{}, ErrTenantAppMigrationInvalid
	}
	plan, err := planTenantAppMigration(ctx, tx, request)
	if err != nil {
		return TenantAppMigrationPlan{}, err
	}
	if !plan.CanBackfill {
		return TenantAppMigrationPlan{}, ErrTenantAppMigrationConflict
	}
	for _, item := range plan.Installations {
		if item.Disposition != "candidate" {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO tenant_github_app_installation_bindings (tenant_id,integration_id,installation_id,bound_at)
			VALUES ($1,$2,$3,$4) ON CONFLICT (installation_id) DO NOTHING`, request.TenantID, request.IntegrationID, item.InstallationID, now.UTC()); err != nil {
			return TenantAppMigrationPlan{}, fmt.Errorf("backfill Tenant App Installation binding: %w", err)
		}
		var tenant domain.TenantID
		var integration string
		if err := tx.QueryRow(ctx, `SELECT tenant_id,integration_id FROM tenant_github_app_installation_bindings WHERE installation_id=$1`, item.InstallationID).Scan(&tenant, &integration); err != nil {
			return TenantAppMigrationPlan{}, err
		}
		if tenant != request.TenantID || integration != request.IntegrationID {
			return TenantAppMigrationPlan{}, ErrTenantAppMigrationConflict
		}
	}
	return planTenantAppMigration(ctx, tx, request)
}

func planTenantAppMigration(ctx context.Context, tx pgx.Tx, request TenantAppMigrationRequest) (TenantAppMigrationPlan, error) {
	var plan TenantAppMigrationPlan
	var privateRole, privateARN, privateVersion, oauthRole, oauthARN, oauthVersion, webhookRole, webhookARN, webhookVersion string
	err := tx.QueryRow(ctx, `SELECT COALESCE(i.github_app_id,0),i.config_revision,i.credential_generation,
		b.private_key_reader_role_arn,b.private_key_secret_ref,b.private_key_secret_version_id,
		b.oauth_reader_role_arn,b.oauth_secret_ref,b.oauth_secret_version_id,
		b.webhook_secret_reader_role_arn,b.webhook_secret_ref,b.webhook_secret_version_id
		FROM tenant_github_integrations i JOIN github_app_registrations a ON a.id=i.app_registration_id AND a.github_app_id=i.github_app_id
		JOIN github_credential_bindings b ON b.tenant_id=i.tenant_id AND b.id=i.credential_binding_id AND b.github_app_id=i.github_app_id
		WHERE i.tenant_id=$1 AND i.id=$2 AND i.status='active' AND i.config_revision=$3 AND i.credential_generation=$4
		AND a.mode='tenant_owned' AND a.owner_tenant_id=i.tenant_id AND b.status='approved'`,
		request.TenantID, request.IntegrationID, request.ExpectedRevision, request.ExpectedGeneration).Scan(
		&plan.GitHubAppID, &plan.ConfigRevision, &plan.CredentialGeneration,
		&privateRole, &privateARN, &privateVersion, &oauthRole, &oauthARN, &oauthVersion,
		&webhookRole, &webhookARN, &webhookVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantAppMigrationPlan{}, ErrAppBindingDenied
	}
	if err != nil {
		return TenantAppMigrationPlan{}, err
	}
	if !validExactIAMRoleARN(privateRole) || !validExactSecretARN(privateARN) || !validSecretVersionID(privateVersion) ||
		!validExactIAMRoleARN(oauthRole) || !validExactSecretARN(oauthARN) || !validSecretVersionID(oauthVersion) ||
		!validExactIAMRoleARN(webhookRole) || !validExactSecretARN(webhookARN) || !validSecretVersionID(webhookVersion) ||
		privateARN == oauthARN || privateARN == webhookARN || oauthARN == webhookARN || privateRole == oauthRole || privateRole == webhookRole || oauthRole == webhookRole {
		return TenantAppMigrationPlan{}, ErrAppBindingDenied
	}
	plan.TenantID, plan.IntegrationID = request.TenantID, request.IntegrationID
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM workload_runs
		WHERE tenant_id=$1 AND github_integration_id IS NULL AND desired_state='running'
		AND state IN ('pending','queued','provisioning','running')`, request.TenantID).Scan(&plan.LegacyNonterminalRunCount); err != nil {
		return TenantAppMigrationPlan{}, err
	}
	plan.Installations = make([]TenantAppMigrationItem, 0, len(request.InstallationIDs))
	plan.CanBackfill = true
	for _, installationID := range request.InstallationIDs {
		item := TenantAppMigrationItem{InstallationID: installationID, Disposition: "candidate"}
		var storedAppID int64
		var status string
		var suspended bool
		var boundTenant domain.TenantID
		err := tx.QueryRow(ctx, `SELECT COALESCE(i.github_app_id,0), i.status, i.suspended_at IS NOT NULL, g.tenant_id
			FROM tenant_github_installations i JOIN github_app_installation_bindings g ON g.tenant_id=i.tenant_id AND g.installation_id=i.installation_id
			WHERE i.tenant_id=$1 AND i.installation_id=$2`, request.TenantID, installationID).Scan(&storedAppID, &status, &suspended, &boundTenant)
		if errors.Is(err, pgx.ErrNoRows) {
			item.Disposition = "blocked_missing_tenant_binding"
		} else if err != nil {
			return TenantAppMigrationPlan{}, err
		} else if boundTenant != request.TenantID {
			item.Disposition = "blocked_cross_tenant_binding"
		} else if status != "active" || suspended {
			item.Disposition = "blocked_installation_inactive"
		} else if storedAppID <= 0 {
			item.Disposition = "blocked_app_identity_unverified"
		} else if storedAppID != plan.GitHubAppID {
			item.Disposition = "blocked_app_id_mismatch"
		} else {
			var existingTenant domain.TenantID
			var existingIntegration string
			err = tx.QueryRow(ctx, `SELECT tenant_id,integration_id FROM tenant_github_app_installation_bindings WHERE installation_id=$1`, installationID).Scan(&existingTenant, &existingIntegration)
			if errors.Is(err, pgx.ErrNoRows) {
				item.Disposition = "candidate"
			} else if err != nil {
				return TenantAppMigrationPlan{}, err
			} else if existingTenant == request.TenantID && existingIntegration == request.IntegrationID {
				item.Disposition = "already_bound"
			} else {
				item.Disposition = "blocked_integration_conflict"
			}
		}
		if item.Disposition != "candidate" && item.Disposition != "already_bound" {
			plan.CanBackfill = false
		}
		plan.Installations = append(plan.Installations, item)
	}
	return plan, nil
}

func validateTenantAppMigrationRequest(request TenantAppMigrationRequest) error {
	if request.TenantID == "" || !validOpaqueMetadata(request.IntegrationID, 128) || request.ExpectedRevision < 1 || request.ExpectedGeneration < 1 || len(request.InstallationIDs) == 0 || len(request.InstallationIDs) > 100 {
		return ErrTenantAppMigrationInvalid
	}
	ids := append([]int64(nil), request.InstallationIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for index, id := range ids {
		if id <= 0 || index > 0 && ids[index-1] == id {
			return ErrTenantAppMigrationInvalid
		}
	}
	return nil
}

// SelectDefaultApp is the explicit, revision-checked ready→active transition.
// It requires separately provisioned purpose-specific credential readers and
// a drained legacy queue; existing pinned Runs are never rewritten.
func SelectDefaultApp(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, integrationID string, expectedRevision int64, now time.Time) (AppIntegration, error) {
	return SelectDefaultAppGeneration(ctx, tx, tenant, integrationID, expectedRevision, 0, now)
}

// SelectDefaultAppGeneration additionally fences activation to the credential
// generation observed by the operator during verification. A zero generation
// is retained only for callers migrating from the older API contract.
func SelectDefaultAppGeneration(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, integrationID string, expectedRevision, expectedGeneration int64, now time.Time) (AppIntegration, error) {
	if tx == nil || tenant == "" || !validOpaqueMetadata(integrationID, 128) || expectedRevision < 1 || now.IsZero() {
		return AppIntegration{}, ErrInvalidApp
	}
	app, err := GetApp(ctx, tx, tenant, integrationID)
	if err != nil {
		return AppIntegration{}, err
	}
	if app.ConfigRevision != expectedRevision || (expectedGeneration > 0 && app.CredentialGeneration != expectedGeneration) || app.Mode != "tenant_owned" || (app.Status != "ready" && app.Status != "active") {
		return AppIntegration{}, ErrAppConflict
	}
	var privateRole, privateARN, privateVersion, oauthRole, oauthARN, oauthVersion, webhookRole, webhookARN, webhookVersion string
	err = tx.QueryRow(ctx, `SELECT b.private_key_reader_role_arn,b.private_key_secret_ref,b.private_key_secret_version_id,
		b.oauth_reader_role_arn,b.oauth_secret_ref,b.oauth_secret_version_id,
		b.webhook_secret_reader_role_arn,b.webhook_secret_ref,b.webhook_secret_version_id
		FROM tenant_github_integrations i JOIN github_credential_bindings b ON b.tenant_id=i.tenant_id AND b.id=i.credential_binding_id AND b.github_app_id=i.github_app_id
		WHERE i.tenant_id=$1 AND i.id=$2 AND i.github_app_id=$3 AND b.status='approved'`, tenant, integrationID, app.GitHubAppID).
		Scan(&privateRole, &privateARN, &privateVersion, &oauthRole, &oauthARN, &oauthVersion, &webhookRole, &webhookARN, &webhookVersion)
	if err != nil || !validExactIAMRoleARN(privateRole) || !validExactSecretARN(privateARN) || !validSecretVersionID(privateVersion) ||
		!validExactIAMRoleARN(oauthRole) || !validExactSecretARN(oauthARN) || !validSecretVersionID(oauthVersion) ||
		!validExactIAMRoleARN(webhookRole) || !validExactSecretARN(webhookARN) || !validSecretVersionID(webhookVersion) ||
		privateARN == oauthARN || privateARN == webhookARN || oauthARN == webhookARN || privateRole == oauthRole || privateRole == webhookRole || oauthRole == webhookRole {
		return AppIntegration{}, ErrAppBindingDenied
	}
	var legacyRuns int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM workload_runs WHERE tenant_id=$1 AND github_integration_id IS DISTINCT FROM $2 AND desired_state='running' AND state IN ('pending','queued','provisioning','running')`, tenant, integrationID).Scan(&legacyRuns); err != nil {
		return AppIntegration{}, err
	}
	if legacyRuns != 0 {
		return AppIntegration{}, ErrLegacyRunsNotDrained
	}
	var previousIntegration string
	var previousRevision int64
	previousErr := tx.QueryRow(ctx, `SELECT integration_id,revision FROM tenant_github_default_integrations WHERE tenant_id=$1`, tenant).Scan(&previousIntegration, &previousRevision)
	if previousErr != nil && !errors.Is(previousErr, pgx.ErrNoRows) {
		return AppIntegration{}, previousErr
	}
	if previousErr == nil && previousIntegration != integrationID {
		previous, err := GetApp(ctx, tx, tenant, previousIntegration)
		if err == nil && previous.Mode == "tenant_owned" && previous.Status == "active" {
			if previous.ConfigRevision != previousRevision {
				return AppIntegration{}, ErrAppConflict
			}
			tag, err := tx.Exec(ctx, `UPDATE tenant_github_integrations SET status='disabled',config_revision=config_revision+1,
				credential_generation=credential_generation+1,updated_at=$4
				WHERE tenant_id=$1 AND id=$2 AND status='active' AND config_revision=$3`, tenant, previousIntegration, previousRevision, now.UTC())
			if err != nil {
				return AppIntegration{}, err
			}
			if tag.RowsAffected() != 1 {
				return AppIntegration{}, ErrAppConflict
			}
		} else if err != nil && !errors.Is(err, ErrAppNotFound) {
			return AppIntegration{}, err
		}
	}
	if app.Status == "ready" {
		tag, err := tx.Exec(ctx, `UPDATE tenant_github_integrations SET status='active',updated_at=$4
			WHERE tenant_id=$1 AND id=$2 AND status='ready' AND config_revision=$3`, tenant, integrationID, expectedRevision, now.UTC())
		if err != nil {
			return AppIntegration{}, err
		}
		if tag.RowsAffected() != 1 {
			return AppIntegration{}, ErrAppConflict
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tenant_github_default_integrations (tenant_id,integration_id,revision)
		VALUES ($1,$2,$3) ON CONFLICT (tenant_id) DO UPDATE SET integration_id=$2,revision=$3`, tenant, integrationID, expectedRevision); err != nil {
		return AppIntegration{}, err
	}
	return GetApp(ctx, tx, tenant, integrationID)
}
