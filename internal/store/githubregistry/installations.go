// Package githubregistry stores tenant-scoped GitHub App installation claims.
// A pending claim is not authorization to use an installation; it must be
// confirmed through the GitHub App installation callback before binding.
package githubregistry

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

var (
	ErrInvalidInstallation  = errors.New("invalid GitHub App installation")
	ErrInstallationConflict = errors.New("GitHub App installation claim conflicts with existing state")
)

var accountLoginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)

type Installation struct {
	TenantID       domain.TenantID `json:"tenant_id"`
	InstallationID int64           `json:"installation_id"`
	AccountLogin   string          `json:"account_login"`
	Status         string          `json:"status"`
	RequestedBy    domain.UserID   `json:"requested_by"`
	RequestedAt    time.Time       `json:"requested_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

func NormalizeInstallation(installation Installation) (Installation, error) {
	installation.AccountLogin = strings.TrimSpace(installation.AccountLogin)
	if installation.TenantID == "" || installation.InstallationID <= 0 || !accountLoginPattern.MatchString(installation.AccountLogin) || installation.RequestedBy == "" {
		return Installation{}, ErrInvalidInstallation
	}
	return installation, nil
}

// Request creates an inert pending tenant claim. It never binds the GitHub
// installation globally; that requires a later authenticated callback flow.
func Request(ctx context.Context, tx pgx.Tx, installation Installation, now time.Time) (Installation, bool, error) {
	if tx == nil {
		return Installation{}, false, errors.New("transaction is required")
	}
	installation, err := NormalizeInstallation(installation)
	if err != nil {
		return Installation{}, false, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	installation.Status, installation.RequestedAt, installation.UpdatedAt = "pending", now, now
	tag, err := tx.Exec(ctx, `INSERT INTO tenant_github_installations
		(tenant_id, installation_id, account_login, status, requested_by, requested_at, updated_at)
		VALUES ($1, $2, $3, 'pending', $4, $5, $5) ON CONFLICT (tenant_id, installation_id) DO NOTHING`,
		installation.TenantID, installation.InstallationID, installation.AccountLogin, installation.RequestedBy, now)
	if err != nil {
		return Installation{}, false, fmt.Errorf("request GitHub App installation: %w", err)
	}
	created := tag.RowsAffected() == 1
	current, err := Get(ctx, tx, installation.TenantID, installation.InstallationID)
	if err != nil {
		return Installation{}, false, err
	}
	if !strings.EqualFold(current.AccountLogin, installation.AccountLogin) || (current.Status != "pending" && current.Status != "active") {
		return Installation{}, false, ErrInstallationConflict
	}
	return current, created, nil
}

func Get(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, installationID int64) (Installation, error) {
	if tx == nil || tenantID == "" || installationID <= 0 {
		return Installation{}, ErrInvalidInstallation
	}
	var installation Installation
	err := tx.QueryRow(ctx, `SELECT tenant_id, installation_id, account_login, status, requested_by, requested_at, updated_at
		FROM tenant_github_installations WHERE tenant_id = $1 AND installation_id = $2`, tenantID, installationID).
		Scan(&installation.TenantID, &installation.InstallationID, &installation.AccountLogin, &installation.Status, &installation.RequestedBy, &installation.RequestedAt, &installation.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Installation{}, pgx.ErrNoRows
	}
	if err != nil {
		return Installation{}, fmt.Errorf("get Tenant GitHub installation: %w", err)
	}
	return installation, nil
}

func List(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID) ([]Installation, error) {
	if tx == nil || tenantID == "" {
		return nil, ErrInvalidInstallation
	}
	rows, err := tx.Query(ctx, `SELECT tenant_id, installation_id, account_login, status, requested_by, requested_at, updated_at
		FROM tenant_github_installations WHERE tenant_id = $1 ORDER BY requested_at DESC, installation_id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list Tenant GitHub installations: %w", err)
	}
	defer rows.Close()
	installations := make([]Installation, 0)
	for rows.Next() {
		var installation Installation
		if err := rows.Scan(&installation.TenantID, &installation.InstallationID, &installation.AccountLogin, &installation.Status, &installation.RequestedBy, &installation.RequestedAt, &installation.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan Tenant GitHub installation: %w", err)
		}
		installations = append(installations, installation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Tenant GitHub installations: %w", err)
	}
	return installations, nil
}
