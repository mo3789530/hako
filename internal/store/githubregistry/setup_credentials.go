package githubregistry

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// SetupCredential is broker-only metadata, never a secret value or API response.
// Separate columns prevent an OAuth request from choosing a private-key reader.
type SetupCredential struct {
	Pin           SetupAppPin
	Slug          string
	ClientID      string
	ReaderRoleARN string
	SecretARN     string
	VersionID     string
}

func (SetupCredential) String() string     { return "GitHub OAuth credential [REDACTED]" }
func (c SetupCredential) GoString() string { return c.String() }
func (SetupCredential) MarshalJSON() ([]byte, error) {
	return json.Marshal("[REDACTED GitHub OAuth credential]")
}

// ResolveSetupCredential revalidates the entire immutable identity before
// returning only OAuth-purpose access. Missing provisioning fails closed.
func ResolveSetupCredential(ctx context.Context, tx pgx.Tx, pin SetupAppPin) (SetupCredential, error) {
	if tx == nil {
		return SetupCredential{}, ErrAppBindingDenied
	}
	if err := validateSetupAppPin(ctx, tx, pin); err != nil {
		if errors.Is(err, ErrInvalidSetupState) {
			return SetupCredential{}, ErrAppBindingDenied
		}
		return SetupCredential{}, err
	}
	result := SetupCredential{Pin: pin}
	err := tx.QueryRow(ctx, `SELECT a.slug,a.client_id,b.oauth_reader_role_arn,b.oauth_secret_ref,b.oauth_secret_version_id
 FROM tenant_github_integrations i
 JOIN github_app_registrations a ON a.id=i.app_registration_id AND a.github_app_id=i.github_app_id
 JOIN github_credential_bindings b ON b.id=i.credential_binding_id AND b.tenant_id=i.tenant_id AND b.github_app_id=i.github_app_id
 WHERE i.tenant_id=$1 AND i.id=$2 AND i.app_registration_id=$3 AND i.github_app_id=$4
 AND i.config_revision=$5 AND i.credential_generation=$6 AND i.status='active'
 AND a.mode='tenant_owned' AND a.owner_tenant_id=i.tenant_id AND b.status='approved'
 AND b.oauth_reader_role_arn IS NOT NULL AND b.oauth_secret_version_id IS NOT NULL
 AND b.oauth_secret_ref<>b.private_key_secret_ref AND b.oauth_secret_ref<>b.webhook_secret_ref
 AND (b.private_key_reader_role_arn IS NULL OR b.oauth_reader_role_arn<>b.private_key_reader_role_arn)
 AND (b.webhook_secret_reader_role_arn IS NULL OR b.oauth_reader_role_arn<>b.webhook_secret_reader_role_arn)`,
		pin.TenantID, pin.IntegrationID, pin.AppRegistrationID, pin.GitHubAppID, pin.ConfigRevision, pin.CredentialGeneration).
		Scan(&result.Slug, &result.ClientID, &result.ReaderRoleARN, &result.SecretARN, &result.VersionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return SetupCredential{}, ErrAppBindingDenied
	}
	if err != nil {
		return SetupCredential{}, err
	}
	if !validAppSlug(result.Slug) || result.ClientID == "" || !validExactIAMRoleARN(result.ReaderRoleARN) || !validExactSecretARN(result.SecretARN) || !validSecretVersionID(result.VersionID) {
		return SetupCredential{}, ErrAppBindingDenied
	}
	return result, nil
}
