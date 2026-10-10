package githubregistry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

// SetupAppPin is an immutable, database-resolved OAuth identity. It contains
// no credential references and must not be populated from callback query IDs.
type SetupAppPin struct {
	TenantID             domain.TenantID
	IntegrationID        string
	AppRegistrationID    string
	GitHubAppID          int64
	ConfigRevision       int64
	CredentialGeneration int64
}

func ResolveSetupAppPin(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, integrationID string) (SetupAppPin, error) {
	if tx == nil || tenantID == "" || !validOpaqueMetadata(integrationID, 128) {
		return SetupAppPin{}, ErrAppBindingDenied
	}
	var pin SetupAppPin
	err := tx.QueryRow(ctx, `SELECT i.tenant_id, i.id, i.app_registration_id, i.github_app_id,
		i.config_revision, i.credential_generation
		FROM tenant_github_integrations i
		JOIN github_app_registrations r ON r.id=i.app_registration_id AND r.github_app_id=i.github_app_id
		JOIN github_credential_bindings b ON b.tenant_id=i.tenant_id AND b.id=i.credential_binding_id AND b.github_app_id=i.github_app_id
		WHERE i.tenant_id=$1 AND i.id=$2 AND i.status='active'
		AND r.mode='tenant_owned' AND r.owner_tenant_id=i.tenant_id AND b.status='approved'`, tenantID, integrationID).
		Scan(&pin.TenantID, &pin.IntegrationID, &pin.AppRegistrationID, &pin.GitHubAppID, &pin.ConfigRevision, &pin.CredentialGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return SetupAppPin{}, ErrAppBindingDenied
	}
	if err != nil {
		return SetupAppPin{}, fmt.Errorf("resolve GitHub setup App identity: %w", err)
	}
	return pin, nil
}

func validateSetupAppPin(ctx context.Context, tx pgx.Tx, pin SetupAppPin) error {
	current, err := ResolveSetupAppPin(ctx, tx, pin.TenantID, pin.IntegrationID)
	if errors.Is(err, ErrAppBindingDenied) || err == nil && current != pin {
		return ErrInvalidSetupState
	}
	return err
}

func CreateTenantAppSetupState(ctx context.Context, tx pgx.Tx, pin SetupAppPin, userID domain.UserID, stateHash, codeVerifier string, now, expiresAt time.Time) error {
	if tx == nil {
		return ErrInvalidSetupState
	}
	if err := validateSetupAppPin(ctx, tx, pin); err != nil {
		return err
	}
	if err := CreateSetupState(ctx, tx, pin.TenantID, userID, stateHash, codeVerifier, now, expiresAt); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE github_installation_setup_states
		SET integration_id=$2, app_registration_id=$3, github_app_id=$4, config_revision=$5, credential_generation=$6
		WHERE state_sha256=$1`, strings.ToLower(stateHash), pin.IntegrationID, pin.AppRegistrationID, pin.GitHubAppID, pin.ConfigRevision, pin.CredentialGeneration)
	return err
}

func bindSetupAppInstallation(ctx context.Context, tx pgx.Tx, pin SetupAppPin, installationID int64, now time.Time) error {
	if _, err := tx.Exec(ctx, `INSERT INTO tenant_github_app_installation_bindings (tenant_id,integration_id,installation_id,bound_at)
		VALUES ($1,$2,$3,$4) ON CONFLICT (installation_id) DO NOTHING`, pin.TenantID, pin.IntegrationID, installationID, now); err != nil {
		return fmt.Errorf("bind setup Installation to GitHub integration: %w", err)
	}
	var tenantID domain.TenantID
	var integrationID string
	if err := tx.QueryRow(ctx, `SELECT tenant_id,integration_id FROM tenant_github_app_installation_bindings WHERE installation_id=$1`, installationID).Scan(&tenantID, &integrationID); err != nil {
		return err
	}
	if tenantID != pin.TenantID || integrationID != pin.IntegrationID {
		return ErrInstallationBound
	}
	return nil
}
