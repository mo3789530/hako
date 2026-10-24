// Package softwarecatalog persists Tenant-scoped Software Catalog metadata.
package softwarecatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/deploypolicy"
	"github.com/mo3789530/hako/internal/softwarecatalog"
)

var (
	ErrRepositoryNotFound = errors.New("Tenant Repository is not registered")
	ErrDependencyNotFound = errors.New("Software Catalog dependency is not registered")
	ErrServiceNotFound    = errors.New("Software Catalog service is not registered")
)

func List(ctx context.Context, tx pgx.Tx, tenantID string) ([]softwarecatalog.Service, error) {
	if ctx == nil || tx == nil || tenantID == "" {
		return nil, softwarecatalog.ErrInvalid
	}
	rows, err := tx.Query(ctx, `SELECT c.tenant_id,c.service_id,c.owner_team,c.installation_id,c.github_repository_id,
		r.owner_login || '/' || r.repository_name,c.runtime,c.language,c.dependencies,COALESCE(c.production_url,'')
		FROM tenant_software_catalog_services c
		JOIN tenant_github_repositories r ON r.tenant_id=c.tenant_id AND r.installation_id=c.installation_id
			AND r.github_repository_id=c.github_repository_id
		WHERE c.tenant_id=$1 ORDER BY c.service_id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list Tenant Software Catalog: %w", err)
	}
	defer rows.Close()
	items := make([]softwarecatalog.Service, 0)
	for rows.Next() {
		var item softwarecatalog.Service
		var dependencies []byte
		if err := rows.Scan(&item.TenantID, &item.ServiceID, &item.OwnerTeam, &item.InstallationID, &item.GitHubRepositoryID,
			&item.Repository, &item.Runtime, &item.Language, &dependencies, &item.ProductionURL); err != nil {
			return nil, fmt.Errorf("scan Tenant Software Catalog service: %w", err)
		}
		if err := json.Unmarshal(dependencies, &item.Dependencies); err != nil || softwarecatalog.Validate(item) != nil {
			return nil, errors.New("stored Software Catalog service is invalid")
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Tenant Software Catalog: %w", err)
	}
	return items, nil
}

// Upsert writes metadata only. Repository and dependency references are
// revalidated in the same transaction as the catalog mutation.
func Upsert(ctx context.Context, tx pgx.Tx, item softwarecatalog.Service, actor string, now time.Time) (softwarecatalog.Service, error) {
	if ctx == nil || tx == nil || actor == "" || now.IsZero() || softwarecatalog.Validate(item) != nil {
		return softwarecatalog.Service{}, softwarecatalog.ErrInvalid
	}
	var repositoryExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenant_github_repositories
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3)`, item.TenantID, item.InstallationID,
		item.GitHubRepositoryID).Scan(&repositoryExists); err != nil {
		return softwarecatalog.Service{}, fmt.Errorf("verify Software Catalog Repository: %w", err)
	}
	if !repositoryExists {
		return softwarecatalog.Service{}, ErrRepositoryNotFound
	}
	if len(item.Dependencies) != 0 {
		var dependencyCount int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tenant_software_catalog_services
			WHERE tenant_id=$1 AND service_id=ANY($2::text[])`, item.TenantID, item.Dependencies).Scan(&dependencyCount); err != nil {
			return softwarecatalog.Service{}, fmt.Errorf("verify Software Catalog dependencies: %w", err)
		}
		if dependencyCount != len(item.Dependencies) {
			return softwarecatalog.Service{}, ErrDependencyNotFound
		}
	}
	dependencies := item.Dependencies
	if dependencies == nil {
		dependencies = []string{}
	}
	encodedDependencies, err := json.Marshal(dependencies)
	if err != nil {
		return softwarecatalog.Service{}, errors.New("encode Software Catalog dependencies")
	}
	var stored softwarecatalog.Service
	var storedDependencies []byte
	err = tx.QueryRow(ctx, `INSERT INTO tenant_software_catalog_services
		(tenant_id,service_id,owner_team,installation_id,github_repository_id,runtime,language,dependencies,production_url,created_by,updated_by,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$10,$10,$11,$11)
		ON CONFLICT (tenant_id,service_id) DO UPDATE SET owner_team=EXCLUDED.owner_team,
			installation_id=EXCLUDED.installation_id,github_repository_id=EXCLUDED.github_repository_id,
			runtime=EXCLUDED.runtime,language=EXCLUDED.language,dependencies=EXCLUDED.dependencies,
			production_url=EXCLUDED.production_url,updated_by=EXCLUDED.updated_by,updated_at=EXCLUDED.updated_at
		RETURNING tenant_id,service_id,owner_team,installation_id,github_repository_id,runtime,language,dependencies,COALESCE(production_url,'')`,
		item.TenantID, item.ServiceID, item.OwnerTeam, item.InstallationID, item.GitHubRepositoryID, item.Runtime, item.Language,
		string(encodedDependencies), nullableURL(item.ProductionURL), actor, now.UTC()).Scan(&stored.TenantID, &stored.ServiceID, &stored.OwnerTeam,
		&stored.InstallationID, &stored.GitHubRepositoryID, &stored.Runtime, &stored.Language, &storedDependencies, &stored.ProductionURL)
	if err != nil {
		return softwarecatalog.Service{}, fmt.Errorf("upsert Tenant Software Catalog service: %w", err)
	}
	if err := json.Unmarshal(storedDependencies, &stored.Dependencies); err != nil {
		return softwarecatalog.Service{}, errors.New("decode stored Software Catalog dependencies")
	}
	if err := tx.QueryRow(ctx, `SELECT owner_login || '/' || repository_name FROM tenant_github_repositories
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3`, stored.TenantID, stored.InstallationID,
		stored.GitHubRepositoryID).Scan(&stored.Repository); err != nil {
		return softwarecatalog.Service{}, fmt.Errorf("resolve Software Catalog Repository name: %w", err)
	}
	return stored, nil
}

func nullableURL(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func ReadDeployPolicy(ctx context.Context, tx pgx.Tx, tenantID, serviceID string) (deploypolicy.Policy, error) {
	if ctx == nil || tx == nil || tenantID == "" || serviceID == "" {
		return deploypolicy.Policy{}, deploypolicy.ErrInvalid
	}
	var data []byte
	err := tx.QueryRow(ctx, `SELECT COALESCE(oidc_deploy_policy,'{}'::jsonb) FROM tenant_software_catalog_services
		WHERE tenant_id=$1 AND service_id=$2`, tenantID, serviceID).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return deploypolicy.Policy{}, ErrServiceNotFound
	}
	if err != nil {
		return deploypolicy.Policy{}, fmt.Errorf("read Software Catalog OIDC policy: %w", err)
	}
	var policy deploypolicy.Policy
	if err := json.Unmarshal(data, &policy); err != nil || policy.Validate() != nil {
		return deploypolicy.Policy{}, errors.New("stored Software Catalog OIDC policy is invalid")
	}
	return policy, nil
}

// ReadDeployPolicyBinding returns the registered Repository identity and the
// policy from one Tenant Catalog row for token verification.
func ReadDeployPolicyBinding(ctx context.Context, tx pgx.Tx, tenantID, serviceID string) (string, int64, deploypolicy.Policy, error) {
	if ctx == nil || tx == nil || tenantID == "" || serviceID == "" {
		return "", 0, deploypolicy.Policy{}, deploypolicy.ErrInvalid
	}
	var repository string
	var repositoryID int64
	var data []byte
	err := tx.QueryRow(ctx, `SELECT r.owner_login || '/' || r.repository_name,c.github_repository_id,
		COALESCE(c.oidc_deploy_policy,'{}'::jsonb)
		FROM tenant_software_catalog_services c JOIN tenant_github_repositories r
		ON r.tenant_id=c.tenant_id AND r.installation_id=c.installation_id AND r.github_repository_id=c.github_repository_id
		WHERE c.tenant_id=$1 AND c.service_id=$2`, tenantID, serviceID).Scan(&repository, &repositoryID, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, deploypolicy.Policy{}, ErrServiceNotFound
	}
	if err != nil {
		return "", 0, deploypolicy.Policy{}, fmt.Errorf("read Software Catalog OIDC binding: %w", err)
	}
	var policy deploypolicy.Policy
	if err := json.Unmarshal(data, &policy); err != nil || policy.Validate() != nil {
		return "", 0, deploypolicy.Policy{}, errors.New("stored Software Catalog OIDC policy is invalid")
	}
	return repository, repositoryID, policy, nil
}

func SetDeployPolicy(ctx context.Context, tx pgx.Tx, tenantID, serviceID, actor string, policy deploypolicy.Policy, now time.Time) error {
	if ctx == nil || tx == nil || tenantID == "" || serviceID == "" || actor == "" || now.IsZero() || policy.Validate() != nil {
		return deploypolicy.ErrInvalid
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenant_software_catalog_services WHERE tenant_id=$1 AND service_id=$2)`,
		tenantID, serviceID).Scan(&exists); err != nil {
		return fmt.Errorf("verify Software Catalog service for OIDC policy: %w", err)
	}
	if !exists {
		return ErrServiceNotFound
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return errors.New("encode Software Catalog OIDC policy")
	}
	if _, err := tx.Exec(ctx, `UPDATE tenant_software_catalog_services SET oidc_deploy_policy=$3::jsonb,updated_by=$4,updated_at=$5
		WHERE tenant_id=$1 AND service_id=$2`, tenantID, serviceID, string(data), actor, now.UTC()); err != nil {
		return fmt.Errorf("store Software Catalog OIDC policy: %w", err)
	}
	return nil
}
