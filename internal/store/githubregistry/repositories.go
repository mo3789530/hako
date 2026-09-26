package githubregistry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubwebhook"
)

var ErrInstallationNotActive = errors.New("GitHub App installation is not active for Tenant")

type Repository struct {
	TenantID           domain.TenantID `json:"tenant_id"`
	InstallationID     int64           `json:"installation_id"`
	GitHubRepositoryID int64           `json:"github_repository_id"`
	OwnerLogin         string          `json:"owner_login"`
	RepositoryName     string          `json:"repository_name"`
	DefaultBranch      string          `json:"default_branch"`
	SynchronizedAt     time.Time       `json:"synchronized_at"`
}

// SyncInstallationRepositories applies one normalized Installation repository
// event only after a verified active binding exists. Callers should update the
// webhook delivery status in the same transaction.
func SyncInstallationRepositories(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, event githubwebhook.RepositoryEvent, now time.Time) error {
	if tx == nil || tenantID == "" || event.SchemaVersion != githubwebhook.RepositoryEventSchemaVersion || event.GitHubEvent != "installation_repositories" || event.InstallationID <= 0 {
		return errors.New("Tenant, transaction, and normalized Installation repository event are required")
	}
	if event.Type != "github.installation_repositories.added" && event.Type != "github.installation_repositories.removed" {
		return errors.New("unsupported normalized Installation repository event")
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM github_app_installation_bindings b
		JOIN tenant_github_installations i ON i.tenant_id = b.tenant_id AND i.installation_id = b.installation_id
		WHERE b.tenant_id = $1 AND b.installation_id = $2 AND i.status = 'active'
	)`, tenantID, event.InstallationID).Scan(&active); err != nil {
		return fmt.Errorf("verify active GitHub Installation binding: %w", err)
	}
	if !active {
		return ErrInstallationNotActive
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if event.Type == "github.installation_repositories.added" {
		if len(event.RepositoriesAdded) == 0 || len(event.RepositoriesRemoved) != 0 {
			return errors.New("added event must contain only added repositories")
		}
		for _, repository := range event.RepositoriesAdded {
			if !validRepositoryRef(repository) {
				return errors.New("invalid repository in Installation event")
			}
			if _, err := tx.Exec(ctx, `INSERT INTO tenant_github_repositories
				(tenant_id, installation_id, github_repository_id, owner_login, repository_name, default_branch, synchronized_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (tenant_id, installation_id, github_repository_id) DO UPDATE SET
				owner_login = $4, repository_name = $5, default_branch = $6, synchronized_at = $7`,
				tenantID, event.InstallationID, repository.GitHubID, repository.Owner, repository.Name, repository.DefaultBranch, now); err != nil {
				return fmt.Errorf("sync GitHub repository %d: %w", repository.GitHubID, err)
			}
		}
		return nil
	}
	if len(event.RepositoriesRemoved) == 0 || len(event.RepositoriesAdded) != 0 {
		return errors.New("removed event must contain only removed repositories")
	}
	for _, repository := range event.RepositoriesRemoved {
		if !validRepositoryRef(repository) {
			return errors.New("invalid repository in Installation event")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM tenant_github_repositories
			WHERE tenant_id = $1 AND installation_id = $2 AND github_repository_id = $3`, tenantID, event.InstallationID, repository.GitHubID); err != nil {
			return fmt.Errorf("remove GitHub repository %d: %w", repository.GitHubID, err)
		}
	}
	return nil
}

func ListRepositories(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, installationID int64) ([]Repository, error) {
	if tx == nil || tenantID == "" || installationID <= 0 {
		return nil, ErrInvalidInstallation
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM github_app_installation_bindings b
		JOIN tenant_github_installations i ON i.tenant_id = b.tenant_id AND i.installation_id = b.installation_id
		WHERE b.tenant_id = $1 AND b.installation_id = $2 AND i.status = 'active'
	)`, tenantID, installationID).Scan(&active); err != nil {
		return nil, fmt.Errorf("verify active GitHub Installation binding: %w", err)
	}
	if !active {
		return nil, ErrInstallationNotActive
	}
	rows, err := tx.Query(ctx, `SELECT tenant_id, installation_id, github_repository_id, owner_login, repository_name, default_branch, synchronized_at
		FROM tenant_github_repositories WHERE tenant_id = $1 AND installation_id = $2
		ORDER BY owner_login, repository_name, github_repository_id`, tenantID, installationID)
	if err != nil {
		return nil, fmt.Errorf("list Tenant GitHub repositories: %w", err)
	}
	defer rows.Close()
	repositories := make([]Repository, 0)
	for rows.Next() {
		var repository Repository
		if err := rows.Scan(&repository.TenantID, &repository.InstallationID, &repository.GitHubRepositoryID, &repository.OwnerLogin, &repository.RepositoryName, &repository.DefaultBranch, &repository.SynchronizedAt); err != nil {
			return nil, fmt.Errorf("scan Tenant GitHub repository: %w", err)
		}
		repositories = append(repositories, repository)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Tenant GitHub repositories: %w", err)
	}
	return repositories, nil
}

func validRepositoryRef(repository githubwebhook.RepositoryRef) bool {
	return repository.GitHubID > 0 && validAccountLogin(repository.Owner) && validRepositoryName(repository.Name) && len(repository.DefaultBranch) <= 255 && !strings.ContainsAny(repository.DefaultBranch, "\r\n\x00")
}

func validRepositoryName(name string) bool {
	if name == "" || len(name) > 100 || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, "/\\\r\n\x00")
}
