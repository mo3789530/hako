package githubregistry

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/arn"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idempotency"
)

var (
	ErrInvalidApp             = errors.New("invalid Tenant GitHub App configuration")
	ErrAppNotFound            = errors.New("Tenant GitHub App configuration not found")
	ErrAppConflict            = errors.New("Tenant GitHub App configuration conflicts with existing state")
	ErrAppIdempotencyConflict = errors.New("Tenant GitHub App idempotency key conflicts with request")
	ErrAppBindingDenied       = errors.New("GitHub App credential binding not approved for Tenant")
)

// AppIntegration intentionally excludes credential references and material.
// All mutations here are metadata-only: no verification, activation or token.
type AppIntegration struct {
	ID                   string          `json:"id"`
	TenantID             domain.TenantID `json:"tenant_id"`
	GitHubAppID          int64           `json:"github_app_id"`
	Mode                 string          `json:"mode"`
	Slug                 string          `json:"slug"`
	ClientID             string          `json:"client_id"`
	Status               string          `json:"status"`
	ConfigRevision       int64           `json:"config_revision"`
	CredentialGeneration int64           `json:"credential_generation"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

type AppDraftRequest struct {
	GitHubAppID         int64  `json:"github_app_id"`
	Slug                string `json:"slug"`
	ClientID            string `json:"client_id"`
	CredentialBindingID string `json:"credential_binding_id"`
}

// AppProofCredential is resolved from authoritative Tenant-owned metadata for
// one verification attempt. The key reference is not a secret value; callers
// must use it only in the trusted credential service and must not log it.
type AppProofCredential struct {
	TenantID             domain.TenantID `json:"tenant_id"`
	IntegrationID        string          `json:"integration_id"`
	GitHubAppID          int64           `json:"github_app_id"`
	PrivateKeySecretARN  string          `json:"-"`
	ConfigRevision       int64           `json:"config_revision"`
	CredentialGeneration int64           `json:"credential_generation"`
}

func (c AppProofCredential) String() string {
	return "Tenant GitHub App proof credential [REDACTED]"
}

func (c AppProofCredential) GoString() string { return c.String() }

// ResolveAppProofCredential permits only an approved, draft Tenant-owned App
// at the exact expected revision/generation. It deliberately exposes only the
// private-key reference needed to prove App ownership, never OAuth/webhook refs.
func ResolveAppProofCredential(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, integrationID string, expectedRevision, expectedGeneration int64) (AppProofCredential, error) {
	if tx == nil || tenant == "" || !validOpaqueMetadata(integrationID, 128) || expectedRevision < 1 || expectedGeneration < 1 {
		return AppProofCredential{}, ErrAppBindingDenied
	}
	var result AppProofCredential
	err := tx.QueryRow(ctx, `SELECT i.tenant_id, i.id, i.github_app_id, b.private_key_secret_ref,
		i.config_revision, i.credential_generation
		FROM tenant_github_integrations i
		JOIN github_app_registrations a ON a.id = i.app_registration_id AND a.github_app_id = i.github_app_id
		JOIN github_credential_bindings b ON b.tenant_id = i.tenant_id AND b.id = i.credential_binding_id AND b.github_app_id = i.github_app_id
		WHERE i.tenant_id = $1 AND i.id = $2 AND i.status = 'draft'
		AND a.mode = 'tenant_owned' AND a.owner_tenant_id = i.tenant_id
		AND b.status = 'approved'
		AND i.config_revision = $3 AND i.credential_generation = $4`, tenant, integrationID, expectedRevision, expectedGeneration).
		Scan(&result.TenantID, &result.IntegrationID, &result.GitHubAppID, &result.PrivateKeySecretARN, &result.ConfigRevision, &result.CredentialGeneration)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AppProofCredential{}, ErrAppBindingDenied
		}
		return AppProofCredential{}, err
	}
	if !validExactSecretARN(result.PrivateKeySecretARN) {
		return AppProofCredential{}, ErrAppBindingDenied
	}
	return result, nil
}

func validExactSecretARN(value string) bool {
	parsed, err := arn.Parse(value)
	return err == nil && parsed.Partition == "aws" && parsed.Service == "secretsmanager" && parsed.Region != "" && len(parsed.AccountID) == 12 && strings.HasPrefix(parsed.Resource, "secret:") && len(parsed.Resource) > len("secret:") && !strings.ContainsAny(value, "*?\r\n")
}

func NormalizeAppDraft(request AppDraftRequest) (AppDraftRequest, error) {
	request.Slug = strings.ToLower(strings.TrimSpace(request.Slug))
	request.ClientID = strings.TrimSpace(request.ClientID)
	request.CredentialBindingID = strings.TrimSpace(request.CredentialBindingID)
	if request.GitHubAppID <= 0 || !validAppSlug(request.Slug) || !validOpaqueMetadata(strings.ReplaceAll(request.ClientID, ".", "_"), 128) || !validOpaqueMetadata(request.CredentialBindingID, 128) {
		return AppDraftRequest{}, ErrInvalidApp
	}
	return request, nil
}

func validOpaqueMetadata(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func validAppSlug(value string) bool {
	if value == "" || len(value) > 100 {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			return false
		}
	}
	return value[0] != '-' && value[len(value)-1] != '-'
}

const appSelect = `SELECT i.id, i.tenant_id, i.github_app_id, a.mode, a.slug, a.client_id,
    i.status, i.config_revision, i.credential_generation, i.created_at, i.updated_at
    FROM tenant_github_integrations i JOIN github_app_registrations a ON a.id = i.app_registration_id`

func scanApp(row interface{ Scan(...any) error }) (AppIntegration, error) {
	var result AppIntegration
	err := row.Scan(&result.ID, &result.TenantID, &result.GitHubAppID, &result.Mode, &result.Slug, &result.ClientID, &result.Status, &result.ConfigRevision, &result.CredentialGeneration, &result.CreatedAt, &result.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AppIntegration{}, ErrAppNotFound
	}
	return result, err
}

func GetApp(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, id string) (AppIntegration, error) {
	if tx == nil || tenant == "" || !validOpaqueMetadata(id, 128) {
		return AppIntegration{}, ErrAppNotFound
	}
	return scanApp(tx.QueryRow(ctx, appSelect+` WHERE i.tenant_id = $1 AND i.id = $2`, tenant, id))
}

// ListApps is bounded; callers page explicitly rather than creating unbounded
// response bodies. Database actor authorization is the handler's responsibility.
func ListApps(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, limit, offset int) ([]AppIntegration, error) {
	if tx == nil || tenant == "" || limit < 1 || limit > 100 || offset < 0 || offset > 10000 {
		return nil, ErrInvalidApp
	}
	rows, err := tx.Query(ctx, appSelect+` WHERE i.tenant_id = $1 ORDER BY i.created_at, i.id LIMIT $2 OFFSET $3`, tenant, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]AppIntegration, 0)
	for rows.Next() {
		app, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, app)
	}
	return result, rows.Err()
}

// CreateAppDraft requires an operator-approved binding but never dereferences
// it. IDs are generated by the server before a database-only retry callback.
func CreateAppDraft(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, actor domain.UserID, integrationID, registrationID, key string, request AppDraftRequest, now time.Time) (AppIntegration, bool, error) {
	request, err := NormalizeAppDraft(request)
	if err != nil || tx == nil || tenant == "" || actor == "" || !validOpaqueMetadata(integrationID, 128) || !validOpaqueMetadata(registrationID, 128) || idempotency.ValidateKey(key) != nil || now.IsZero() {
		return AppIntegration{}, false, ErrInvalidApp
	}
	fingerprint, _ := idempotency.Fingerprint(struct {
		Tenant  domain.TenantID
		Actor   domain.UserID
		Request AppDraftRequest
	}{tenant, actor, request})
	var priorID, priorHash string
	err = tx.QueryRow(ctx, `SELECT id, request_sha256 FROM tenant_github_integrations WHERE tenant_id = $1 AND idempotency_key = $2`, tenant, key).Scan(&priorID, &priorHash)
	if err == nil {
		if priorHash != fingerprint {
			return AppIntegration{}, false, ErrAppIdempotencyConflict
		}
		app, err := GetApp(ctx, tx, tenant, priorID)
		return app, false, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AppIntegration{}, false, err
	}
	var approved bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM github_credential_bindings WHERE tenant_id = $1 AND id = $2 AND github_app_id = $3 AND status = 'approved')`, tenant, request.CredentialBindingID, request.GitHubAppID).Scan(&approved); err != nil {
		return AppIntegration{}, false, err
	}
	if !approved {
		return AppIntegration{}, false, ErrAppBindingDenied
	}
	_, err = tx.Exec(ctx, `INSERT INTO github_app_registrations (id, github_app_id, mode, owner_tenant_id, slug, client_id, created_at)
        VALUES ($1, $2, 'tenant_owned', $3, $4, $5, $6) ON CONFLICT (github_app_id) DO NOTHING`, registrationID, request.GitHubAppID, tenant, request.Slug, request.ClientID, now)
	if err != nil {
		return AppIntegration{}, false, err
	}
	var registeredID, mode, slug, clientID string
	var owner *string
	if err := tx.QueryRow(ctx, `SELECT id, mode, owner_tenant_id, slug, client_id FROM github_app_registrations WHERE github_app_id = $1`, request.GitHubAppID).Scan(&registeredID, &mode, &owner, &slug, &clientID); err != nil {
		return AppIntegration{}, false, err
	}
	if mode != "tenant_owned" || owner == nil || *owner != string(tenant) {
		return AppIntegration{}, false, ErrAppBindingDenied
	}
	if slug != request.Slug || clientID != request.ClientID {
		return AppIntegration{}, false, ErrAppConflict
	}
	tag, err := tx.Exec(ctx, `INSERT INTO tenant_github_integrations (id, tenant_id, app_registration_id, github_app_id, credential_binding_id, status,
        config_revision, credential_generation, idempotency_key, request_sha256, created_by, created_at, updated_at)
        VALUES ($1,$2,$3,$4,$5,'draft',1,1,$6,$7,$8,$9,$9) ON CONFLICT DO NOTHING`, integrationID, tenant, registeredID, request.GitHubAppID, request.CredentialBindingID, key, fingerprint, actor, now)
	if err != nil {
		return AppIntegration{}, false, err
	}
	if tag.RowsAffected() == 0 {
		if err := tx.QueryRow(ctx, `SELECT id, request_sha256 FROM tenant_github_integrations WHERE tenant_id = $1 AND idempotency_key = $2`, tenant, key).Scan(&priorID, &priorHash); errors.Is(err, pgx.ErrNoRows) {
			return AppIntegration{}, false, ErrAppConflict
		} else if err != nil {
			return AppIntegration{}, false, err
		}
		if priorHash != fingerprint {
			return AppIntegration{}, false, ErrAppIdempotencyConflict
		}
		app, err := GetApp(ctx, tx, tenant, priorID)
		return app, false, err
	}
	app, err := GetApp(ctx, tx, tenant, integrationID)
	return app, true, err
}

// DisableApp currently supports inert drafts only. Active integrations require
// the cancellation/outbox/cleanup implementation in Tasks 58/59, not a DB flag.
func DisableApp(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, id string, expectedRevision int64, now time.Time) (AppIntegration, error) {
	app, err := GetApp(ctx, tx, tenant, id)
	if err != nil {
		return AppIntegration{}, err
	}
	if expectedRevision < 1 || expectedRevision != app.ConfigRevision || now.IsZero() {
		return AppIntegration{}, ErrAppConflict
	}
	if app.Status == "disabled" {
		return app, nil
	}
	if app.Status != "draft" {
		return AppIntegration{}, ErrAppConflict
	}
	tag, err := tx.Exec(ctx, `UPDATE tenant_github_integrations SET status = 'disabled', config_revision = config_revision + 1,
        credential_generation = credential_generation + 1, updated_at = $4 WHERE tenant_id = $1 AND id = $2 AND config_revision = $3 AND status = 'draft'`, tenant, id, expectedRevision, now)
	if err != nil {
		return AppIntegration{}, err
	}
	if tag.RowsAffected() != 1 {
		return AppIntegration{}, ErrAppConflict
	}
	return GetApp(ctx, tx, tenant, id)
}
