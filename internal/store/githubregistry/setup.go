package githubregistry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/authz"
	"github.com/mo3789530/hako/internal/domain"
)

var (
	ErrInvalidSetupState         = errors.New("invalid or expired GitHub Installation setup state")
	ErrInstallationBound         = errors.New("GitHub Installation is already bound to another Tenant")
	ErrSetupInstallationMismatch = errors.New("GitHub setup state has a different Installation ID")
)

type SetupState struct {
	TenantID       domain.TenantID
	UserID         domain.UserID
	InstallationID int64
	CodeVerifier   string
}

// HashSetupState returns the irreversible database representation of an
// opaque browser state token. The raw token must only be sent to the browser.
func HashSetupState(state string) string {
	digest := sha256.Sum256([]byte(state))
	return hex.EncodeToString(digest[:])
}

func CreateSetupState(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, userID domain.UserID, stateHash, codeVerifier string, now, expiresAt time.Time) error {
	if tx == nil || tenantID == "" || userID == "" || len(stateHash) != 64 || len(codeVerifier) < 43 || len(codeVerifier) > 128 || now.IsZero() || !expiresAt.After(now) {
		return errors.New("transaction, Tenant, user, SHA-256 state, PKCE verifier, and valid expiry are required")
	}
	if _, err := hex.DecodeString(stateHash); err != nil {
		return errors.New("setup state hash must be lowercase hexadecimal SHA-256")
	}
	// The verifier is needed only while the OAuth authorization is in flight.
	// Reap expired/replayed rows when another setup starts so abandoned flows
	// do not retain PKCE material indefinitely.
	if _, err := tx.Exec(ctx, `DELETE FROM github_installation_setup_states WHERE expires_at <= $1 OR consumed_at IS NOT NULL`, now); err != nil {
		return fmt.Errorf("reap expired GitHub Installation setup states: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO github_installation_setup_states (state_sha256, tenant_id, user_id, code_verifier, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, strings.ToLower(stateHash), tenantID, userID, codeVerifier, now, expiresAt); err != nil {
		return fmt.Errorf("create GitHub Installation setup state: %w", err)
	}
	return nil
}

func ValidateSetupState(ctx context.Context, tx pgx.Tx, stateHash string, now time.Time) (SetupState, error) {
	if tx == nil || len(stateHash) != 64 || now.IsZero() {
		return SetupState{}, ErrInvalidSetupState
	}
	var state SetupState
	var expiresAt time.Time
	var consumedAt *time.Time
	err := tx.QueryRow(ctx, `SELECT tenant_id, user_id, COALESCE(installation_id, 0), code_verifier, expires_at, consumed_at
		FROM github_installation_setup_states WHERE state_sha256 = $1`, strings.ToLower(stateHash)).Scan(&state.TenantID, &state.UserID, &state.InstallationID, &state.CodeVerifier, &expiresAt, &consumedAt)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (consumedAt != nil || !expiresAt.After(now)) {
		return SetupState{}, ErrInvalidSetupState
	}
	if err != nil {
		return SetupState{}, fmt.Errorf("validate GitHub Installation setup state: %w", err)
	}
	return state, nil
}

func SetSetupInstallation(ctx context.Context, tx pgx.Tx, stateHash string, installationID int64, now time.Time) error {
	if installationID <= 0 {
		return ErrInvalidInstallation
	}
	state, err := ValidateSetupState(ctx, tx, stateHash, now)
	if err != nil {
		return err
	}
	if state.InstallationID != 0 && state.InstallationID != installationID {
		return ErrSetupInstallationMismatch
	}
	if state.InstallationID == installationID {
		return nil
	}
	tag, err := tx.Exec(ctx, `UPDATE github_installation_setup_states SET installation_id = $2
		WHERE state_sha256 = $1 AND installation_id IS NULL AND consumed_at IS NULL AND expires_at > $3`, strings.ToLower(stateHash), installationID, now)
	if err != nil {
		return fmt.Errorf("attach GitHub Installation to setup state: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrSetupInstallationMismatch
	}
	return nil
}

// CompleteSetupState consumes a still-valid state and creates the verified
// tenant claim/binding atomically. Callers must first verify through GitHub's
// user access token that the installation is visible to the authorizing user
// and belongs to the configured Hako App.
func CompleteSetupState(ctx context.Context, tx pgx.Tx, stateHash string, installationID int64, accountLogin string, now time.Time) (Installation, error) {
	if tx == nil || len(stateHash) != 64 || installationID <= 0 || now.IsZero() {
		return Installation{}, ErrInvalidSetupState
	}
	state, err := ValidateSetupState(ctx, tx, stateHash, now)
	if err != nil {
		return Installation{}, err
	}
	if state.InstallationID != installationID {
		return Installation{}, ErrSetupInstallationMismatch
	}
	if _, err := authz.RequireTenantRole(ctx, tx, state.UserID, state.TenantID, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
		return Installation{}, err
	}
	if tag, err := tx.Exec(ctx, `UPDATE github_installation_setup_states SET consumed_at = $2, code_verifier = ''
		WHERE state_sha256 = $1 AND installation_id = $3 AND consumed_at IS NULL AND expires_at > $2`, strings.ToLower(stateHash), now, installationID); err != nil {
		return Installation{}, fmt.Errorf("consume GitHub Installation setup state: %w", err)
	} else if tag.RowsAffected() != 1 {
		return Installation{}, ErrInvalidSetupState
	}
	accountLogin = strings.TrimSpace(accountLogin)
	_, err = NormalizeInstallation(Installation{TenantID: state.TenantID, InstallationID: installationID, AccountLogin: accountLogin, RequestedBy: state.UserID})
	if err != nil {
		return Installation{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tenant_github_installations
		(tenant_id, installation_id, account_login, status, requested_by, requested_at, updated_at)
		VALUES ($1, $2, $3, 'active', $4, $5, $5)
		ON CONFLICT (tenant_id, installation_id) DO UPDATE SET account_login = $3, status = 'active', updated_at = $5`,
		state.TenantID, installationID, accountLogin, state.UserID, now); err != nil {
		return Installation{}, fmt.Errorf("activate Tenant GitHub Installation claim: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO github_app_installation_bindings (installation_id, tenant_id, bound_at)
		VALUES ($1, $2, $3) ON CONFLICT (installation_id) DO NOTHING`, installationID, state.TenantID, now); err != nil {
		return Installation{}, fmt.Errorf("bind GitHub Installation to Tenant: %w", err)
	}
	var boundTenantID domain.TenantID
	if err := tx.QueryRow(ctx, `SELECT tenant_id FROM github_app_installation_bindings WHERE installation_id = $1`, installationID).Scan(&boundTenantID); err != nil {
		return Installation{}, fmt.Errorf("verify GitHub Installation binding: %w", err)
	}
	if boundTenantID != state.TenantID {
		return Installation{}, ErrInstallationBound
	}
	return Get(ctx, tx, state.TenantID, installationID)
}
