package githubregistry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

// AppWebhookCredentialAccess contains only database-approved metadata needed
// by a Webhook-purpose reader. It never contains SecretString bytes.
type AppWebhookCredentialAccess struct {
	TenantID             domain.TenantID
	IntegrationID        string
	GitHubAppID          int64
	CredentialGeneration int64
	ReaderRoleARN        string
	SecretARN            string
	SecretVersionID      string
	PreviousVersionID    string
	PreviousValidUntil   *time.Time
}

func (a AppWebhookCredentialAccess) String() string {
	return "Tenant GitHub App Webhook access [REDACTED]"
}
func (a AppWebhookCredentialAccess) GoString() string { return a.String() }
func (AppWebhookCredentialAccess) MarshalJSON() ([]byte, error) {
	return json.Marshal("[REDACTED Tenant GitHub App Webhook access]")
}

// ResolveActiveWebhookCredentialAccess resolves exactly one opaque route to
// the currently active App and approved Webhook-purpose reader binding.
func ResolveActiveWebhookCredentialAccess(ctx context.Context, tx pgx.Tx, integrationID string) (AppWebhookCredentialAccess, error) {
	if tx == nil || !validOpaqueMetadata(integrationID, 128) {
		return AppWebhookCredentialAccess{}, ErrAppBindingDenied
	}
	var access AppWebhookCredentialAccess
	var previousUntil sql.NullTime
	err := tx.QueryRow(ctx, `SELECT i.tenant_id, i.id, i.github_app_id, i.credential_generation,
		b.webhook_secret_reader_role_arn, b.webhook_secret_ref, b.webhook_secret_version_id,
		COALESCE(b.previous_webhook_secret_version_id, ''), b.previous_webhook_secret_valid_until
		FROM tenant_github_integrations i
		JOIN github_app_registrations r ON r.id = i.app_registration_id AND r.github_app_id = i.github_app_id
		JOIN github_credential_bindings b ON b.tenant_id = i.tenant_id AND b.id = i.credential_binding_id AND b.github_app_id = i.github_app_id
		WHERE i.id = $1 AND i.status = 'active' AND r.mode = 'tenant_owned'
		AND r.owner_tenant_id = i.tenant_id AND b.status = 'approved'`, integrationID).
		Scan(&access.TenantID, &access.IntegrationID, &access.GitHubAppID, &access.CredentialGeneration,
			&access.ReaderRoleARN, &access.SecretARN, &access.SecretVersionID, &access.PreviousVersionID, &previousUntil)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AppWebhookCredentialAccess{}, ErrAppBindingDenied
		}
		return AppWebhookCredentialAccess{}, err
	}
	if previousUntil.Valid {
		access.PreviousValidUntil = &previousUntil.Time
	}
	if !validExactIAMRoleARN(access.ReaderRoleARN) || !validExactSecretARN(access.SecretARN) || !validSecretVersionID(access.SecretVersionID) {
		return AppWebhookCredentialAccess{}, ErrAppBindingDenied
	}
	if access.PreviousVersionID != "" && (!validSecretVersionID(access.PreviousVersionID) || access.PreviousValidUntil == nil) {
		return AppWebhookCredentialAccess{}, ErrAppBindingDenied
	}
	if access.PreviousVersionID == "" && access.PreviousValidUntil != nil {
		return AppWebhookCredentialAccess{}, ErrAppBindingDenied
	}
	return access, nil
}
