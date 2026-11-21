package githubregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/agenttask"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubwebhook"
)

var ErrInstallationNotActive = errors.New("GitHub App installation is not active for Tenant")
var ErrRepositoryNotFound = errors.New("GitHub repository is not registered for Tenant")
var actionsRunnerBranchRefPattern = regexp.MustCompile(`^refs/heads/[A-Za-z0-9._/-]{1,220}$`)

type Repository struct {
	TenantID               domain.TenantID `json:"tenant_id"`
	InstallationID         int64           `json:"installation_id"`
	GitHubRepositoryID     int64           `json:"github_repository_id"`
	OwnerLogin             string          `json:"owner_login"`
	RepositoryName         string          `json:"repository_name"`
	DefaultBranch          string          `json:"default_branch"`
	FactoryEnabled         bool            `json:"factory_enabled"`
	FactoryEnabledAt       *time.Time      `json:"factory_enabled_at,omitempty"`
	PreviewEnabled         bool            `json:"preview_enabled"`
	PreviewEnabledAt       *time.Time      `json:"preview_enabled_at,omitempty"`
	ActionsRunnerEnabled   bool            `json:"actions_runner_enabled"`
	ActionsRunnerEnabledAt *time.Time      `json:"actions_runner_enabled_at,omitempty"`
	SynchronizedAt         time.Time       `json:"synchronized_at"`
}

type AgentModelAccess struct {
	Enabled   bool       `json:"enabled"`
	EnabledAt *time.Time `json:"enabled_at,omitempty"`
}

// ActionsRunnerWorkflowPolicy contains only an explicit workflow/ref/event and
// label allowlist. It has no command, Secret, image, IAM role, or network
// configuration fields.
type ActionsRunnerWorkflowPolicy struct {
	WorkflowPath   string   `json:"workflow_path"`
	AllowedRefs    []string `json:"allowed_refs"`
	AllowedEvents  []string `json:"allowed_events"`
	RequiredLabels []string `json:"required_labels"`
	AllowedLabels  []string `json:"allowed_labels"`
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
		WHERE b.tenant_id = $1 AND b.installation_id = $2 AND i.status = 'active' AND i.suspended_at IS NULL
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
		WHERE b.tenant_id = $1 AND b.installation_id = $2 AND i.status = 'active' AND i.suspended_at IS NULL
	)`, tenantID, installationID).Scan(&active); err != nil {
		return nil, fmt.Errorf("verify active GitHub Installation binding: %w", err)
	}
	if !active {
		return nil, ErrInstallationNotActive
	}
	rows, err := tx.Query(ctx, `SELECT tenant_id, installation_id, github_repository_id, owner_login, repository_name, default_branch,
		factory_enabled, factory_enabled_at, preview_enabled, preview_enabled_at, actions_runner_enabled, actions_runner_enabled_at, synchronized_at
		FROM tenant_github_repositories WHERE tenant_id = $1 AND installation_id = $2
		ORDER BY owner_login, repository_name, github_repository_id`, tenantID, installationID)
	if err != nil {
		return nil, fmt.Errorf("list Tenant GitHub repositories: %w", err)
	}
	defer rows.Close()
	repositories := make([]Repository, 0)
	for rows.Next() {
		var repository Repository
		if err := rows.Scan(&repository.TenantID, &repository.InstallationID, &repository.GitHubRepositoryID, &repository.OwnerLogin, &repository.RepositoryName, &repository.DefaultBranch,
			&repository.FactoryEnabled, &repository.FactoryEnabledAt, &repository.PreviewEnabled, &repository.PreviewEnabledAt,
			&repository.ActionsRunnerEnabled, &repository.ActionsRunnerEnabledAt, &repository.SynchronizedAt); err != nil {
			return nil, fmt.Errorf("scan Tenant GitHub repository: %w", err)
		}
		repositories = append(repositories, repository)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Tenant GitHub repositories: %w", err)
	}
	return repositories, nil
}

// SetRepositoryPreviewEnabled explicitly opts a Tenant-owned repository into
// PR Preview desired-state tracking. Runtime allocation remains separately
// gated until the Preview runtime and Gateway route are enabled.
func SetRepositoryPreviewEnabled(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, installationID, repositoryID int64, enabled bool, configuredAt time.Time) (Repository, error) {
	if ctx == nil || tx == nil || tenantID == "" || installationID <= 0 || repositoryID <= 0 || configuredAt.IsZero() {
		return Repository{}, ErrRepositoryNotFound
	}
	var repository Repository
	err := tx.QueryRow(ctx, `UPDATE tenant_github_repositories r
		SET preview_enabled=$4, preview_enabled_at=CASE WHEN $4 THEN COALESCE(r.preview_enabled_at,$5) ELSE NULL END
		FROM github_app_installation_bindings b
		JOIN tenant_github_installations i ON i.tenant_id=b.tenant_id AND i.installation_id=b.installation_id
		WHERE r.tenant_id=$1 AND r.installation_id=$2 AND r.github_repository_id=$3
		  AND b.tenant_id=r.tenant_id AND b.installation_id=r.installation_id
		  AND i.status='active' AND i.suspended_at IS NULL
		RETURNING r.tenant_id,r.installation_id,r.github_repository_id,r.owner_login,r.repository_name,r.default_branch,
		          r.factory_enabled,r.factory_enabled_at,r.preview_enabled,r.preview_enabled_at,
		          r.actions_runner_enabled,r.actions_runner_enabled_at,r.synchronized_at`,
		tenantID, installationID, repositoryID, enabled, configuredAt.UTC()).Scan(
		&repository.TenantID, &repository.InstallationID, &repository.GitHubRepositoryID, &repository.OwnerLogin, &repository.RepositoryName,
		&repository.DefaultBranch, &repository.FactoryEnabled, &repository.FactoryEnabledAt, &repository.PreviewEnabled, &repository.PreviewEnabledAt,
		&repository.ActionsRunnerEnabled, &repository.ActionsRunnerEnabledAt, &repository.SynchronizedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Repository{}, ErrRepositoryNotFound
	}
	if err != nil {
		return Repository{}, fmt.Errorf("configure repository Preview opt-in: %w", err)
	}
	return repository, nil
}

// SetRepositoryFactoryEnabled opts one currently registered repository into
// or out of Hako's default webhook-triggered verification Job. Triggering is
// per repository and disabled by default; workflow_job runner scheduling is
// deliberately handled by the separate Actions runner implementation.
func SetRepositoryFactoryEnabled(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, installationID, repositoryID int64, enabled bool, configuredAt time.Time) (Repository, error) {
	if ctx == nil || tx == nil || tenantID == "" || installationID <= 0 || repositoryID <= 0 || configuredAt.IsZero() {
		return Repository{}, ErrRepositoryNotFound
	}
	var repository Repository
	err := tx.QueryRow(ctx, `UPDATE tenant_github_repositories r
		SET factory_enabled = $4,
		    agent_write_policy = CASE WHEN $4 THEN r.agent_write_policy ELSE NULL END,
		    agent_model_access_enabled = CASE WHEN $4 THEN r.agent_model_access_enabled ELSE false END,
		    agent_model_access_enabled_at = CASE WHEN $4 THEN r.agent_model_access_enabled_at ELSE NULL END,
		    factory_enabled_at = CASE WHEN $4 THEN COALESCE(r.factory_enabled_at, $5) ELSE NULL END
		FROM github_app_installation_bindings b
		JOIN tenant_github_installations i ON i.tenant_id = b.tenant_id AND i.installation_id = b.installation_id
		WHERE r.tenant_id = $1 AND r.installation_id = $2 AND r.github_repository_id = $3
		  AND b.tenant_id = r.tenant_id AND b.installation_id = r.installation_id
		  AND i.status = 'active' AND i.suspended_at IS NULL
		RETURNING r.tenant_id, r.installation_id, r.github_repository_id, r.owner_login, r.repository_name,
			        r.default_branch, r.factory_enabled, r.factory_enabled_at, r.preview_enabled, r.preview_enabled_at, r.actions_runner_enabled, r.actions_runner_enabled_at, r.synchronized_at`, tenantID, installationID, repositoryID, enabled, configuredAt.UTC()).
		Scan(&repository.TenantID, &repository.InstallationID, &repository.GitHubRepositoryID, &repository.OwnerLogin,
			&repository.RepositoryName, &repository.DefaultBranch, &repository.FactoryEnabled, &repository.FactoryEnabledAt,
			&repository.PreviewEnabled, &repository.PreviewEnabledAt,
			&repository.ActionsRunnerEnabled, &repository.ActionsRunnerEnabledAt, &repository.SynchronizedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		var active bool
		if lookupErr := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM github_app_installation_bindings b
			JOIN tenant_github_installations i ON i.tenant_id = b.tenant_id AND i.installation_id = b.installation_id
			WHERE b.tenant_id = $1 AND b.installation_id = $2 AND i.status = 'active' AND i.suspended_at IS NULL
		)`, tenantID, installationID).Scan(&active); lookupErr != nil {
			return Repository{}, fmt.Errorf("verify active Tenant Installation for factory configuration: %w", lookupErr)
		}
		if !active {
			return Repository{}, ErrInstallationNotActive
		}
		return Repository{}, ErrRepositoryNotFound
	}
	if err != nil {
		return Repository{}, fmt.Errorf("update Tenant repository factory setting: %w", err)
	}
	return repository, nil
}

// SetRepositoryAgentModelAccess is a per-Repository, default-off gate for a
// future Native Agent model credential resolver. It never enables scheduling
// or returns credential material.
func SetRepositoryAgentModelAccess(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, installationID, repositoryID int64,
	enabled bool, configuredAt time.Time) (AgentModelAccess, error) {
	if ctx == nil || tx == nil || tenantID == "" || installationID <= 0 || repositoryID <= 0 || configuredAt.IsZero() {
		return AgentModelAccess{}, ErrRepositoryNotFound
	}
	var result AgentModelAccess
	err := tx.QueryRow(ctx, `UPDATE tenant_github_repositories r
		SET agent_model_access_enabled=$4,
		    agent_model_access_enabled_at=CASE WHEN $4 THEN COALESCE(r.agent_model_access_enabled_at,$5) ELSE NULL END
		FROM github_app_installation_bindings b
		JOIN tenant_github_installations i ON i.tenant_id=b.tenant_id AND i.installation_id=b.installation_id
		WHERE r.tenant_id=$1 AND r.installation_id=$2 AND r.github_repository_id=$3
		  AND b.tenant_id=r.tenant_id AND b.installation_id=r.installation_id
		  AND i.status='active' AND i.suspended_at IS NULL
		  AND (NOT $4 OR (r.factory_enabled=TRUE AND r.factory_enabled_at IS NOT NULL AND EXISTS (
			SELECT 1 FROM tenant_model_credentials c
			JOIN tenant_model_credential_bindings cb ON cb.tenant_id=c.tenant_id AND cb.id=c.binding_id
				AND cb.provider=c.provider AND cb.purpose=c.purpose AND cb.status='approved'
			WHERE c.tenant_id=r.tenant_id AND c.provider='anthropic' AND c.purpose='native_agent' AND c.status='active'
		  )))
		RETURNING r.agent_model_access_enabled,r.agent_model_access_enabled_at`, tenantID, installationID, repositoryID, enabled, configuredAt.UTC()).
		Scan(&result.Enabled, &result.EnabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentModelAccess{}, ErrRepositoryNotFound
	}
	if err != nil {
		return AgentModelAccess{}, fmt.Errorf("configure Repository Agent model access: %w", err)
	}
	return result, nil
}

// SetRepositoryAgentWritePolicy stores a narrow, dormant policy for future
// Issue-to-Agent GitHub writes. It requires the Tenant's verified Repository
// and active Factory opt-in; callers must still audit the configuration.
func SetRepositoryAgentWritePolicy(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, installationID, repositoryID int64,
	policy agenttask.WritePolicy) (agenttask.WritePolicy, error) {
	if ctx == nil || tx == nil || tenantID == "" || installationID <= 0 || repositoryID <= 0 || policy.Validate() != nil {
		return agenttask.WritePolicy{}, ErrRepositoryNotFound
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return agenttask.WritePolicy{}, errors.New("encode Agent repository write policy")
	}
	var stored []byte
	err = tx.QueryRow(ctx, `UPDATE tenant_github_repositories r
		SET agent_write_policy=$4::jsonb
		FROM github_app_installation_bindings b
		JOIN tenant_github_installations i ON i.tenant_id=b.tenant_id AND i.installation_id=b.installation_id
		WHERE r.tenant_id=$1 AND r.installation_id=$2 AND r.github_repository_id=$3
		AND r.factory_enabled=TRUE AND r.factory_enabled_at IS NOT NULL
		AND b.tenant_id=r.tenant_id AND b.installation_id=r.installation_id AND i.status='active' AND i.suspended_at IS NULL
		RETURNING r.agent_write_policy`, tenantID, installationID, repositoryID, encoded).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return agenttask.WritePolicy{}, ErrRepositoryNotFound
	}
	if err != nil {
		return agenttask.WritePolicy{}, fmt.Errorf("store Agent repository write policy: %w", err)
	}
	var result agenttask.WritePolicy
	if json.Unmarshal(stored, &result) != nil || result.Validate() != nil {
		return agenttask.WritePolicy{}, errors.New("decode stored Agent repository write policy")
	}
	return result, nil
}

// SetRepositoryActionsRunnerEnabled is a separate, explicit opt-in from the
// default push/PR verification Factory. It records the time at which runner
// admission became eligible so older queued deliveries cannot be replayed
// into a newly-enabled execution policy.
func SetRepositoryActionsRunnerEnabled(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, installationID, repositoryID int64, enabled bool, configuredAt time.Time) (Repository, error) {
	if ctx == nil || tx == nil || tenantID == "" || installationID <= 0 || repositoryID <= 0 || configuredAt.IsZero() {
		return Repository{}, ErrRepositoryNotFound
	}
	var repository Repository
	err := tx.QueryRow(ctx, `UPDATE tenant_github_repositories r
		SET actions_runner_enabled = $4,
		    actions_runner_enabled_at = CASE WHEN $4 THEN COALESCE(r.actions_runner_enabled_at, $5) ELSE NULL END
		FROM github_app_installation_bindings b
		JOIN tenant_github_installations i ON i.tenant_id = b.tenant_id AND i.installation_id = b.installation_id
		WHERE r.tenant_id = $1 AND r.installation_id = $2 AND r.github_repository_id = $3
		  AND b.tenant_id = r.tenant_id AND b.installation_id = r.installation_id
		  AND i.status = 'active' AND i.suspended_at IS NULL
		RETURNING r.tenant_id, r.installation_id, r.github_repository_id, r.owner_login, r.repository_name,
		          r.default_branch, r.factory_enabled, r.factory_enabled_at, r.preview_enabled, r.preview_enabled_at, r.actions_runner_enabled, r.actions_runner_enabled_at, r.synchronized_at`,
		tenantID, installationID, repositoryID, enabled, configuredAt.UTC()).Scan(
		&repository.TenantID, &repository.InstallationID, &repository.GitHubRepositoryID, &repository.OwnerLogin,
		&repository.RepositoryName, &repository.DefaultBranch, &repository.FactoryEnabled, &repository.FactoryEnabledAt,
		&repository.PreviewEnabled, &repository.PreviewEnabledAt,
		&repository.ActionsRunnerEnabled, &repository.ActionsRunnerEnabledAt, &repository.SynchronizedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		var active bool
		if lookupErr := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM github_app_installation_bindings b
			JOIN tenant_github_installations i ON i.tenant_id = b.tenant_id AND i.installation_id = b.installation_id
			WHERE b.tenant_id = $1 AND b.installation_id = $2 AND i.status = 'active' AND i.suspended_at IS NULL
		)`, tenantID, installationID).Scan(&active); lookupErr != nil {
			return Repository{}, fmt.Errorf("verify active Tenant Installation for Actions runner configuration: %w", lookupErr)
		}
		if !active {
			return Repository{}, ErrInstallationNotActive
		}
		return Repository{}, ErrRepositoryNotFound
	}
	if err != nil {
		return Repository{}, fmt.Errorf("update Tenant GitHub Actions runner setting: %w", err)
	}
	if !enabled {
		if _, err := tx.Exec(ctx, `UPDATE tenant_github_repositories SET actions_runner_workflow_policy=NULL
			WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3`, tenantID, installationID, repositoryID); err != nil {
			return Repository{}, fmt.Errorf("clear disabled Actions runner workflow policy: %w", err)
		}
	}
	return repository, nil
}

// SetRepositoryActionsRunnerWorkflowPolicy stores a Tenant's explicit,
// secret-free workflow allowlist only while the registered repository's
// ephemeral runner opt-in is active.
func SetRepositoryActionsRunnerWorkflowPolicy(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, installationID, repositoryID int64,
	policy ActionsRunnerWorkflowPolicy, configuredAt time.Time) (ActionsRunnerWorkflowPolicy, error) {
	if ctx == nil || tx == nil || tenantID == "" || installationID <= 0 || repositoryID <= 0 || configuredAt.IsZero() || !validActionsRunnerWorkflowPolicy(policy) {
		return ActionsRunnerWorkflowPolicy{}, ErrRepositoryNotFound
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return ActionsRunnerWorkflowPolicy{}, errors.New("encode Actions runner workflow policy")
	}
	var stored []byte
	err = tx.QueryRow(ctx, `UPDATE tenant_github_repositories r
		SET actions_runner_workflow_policy=$4::jsonb
		FROM github_app_installation_bindings b
		JOIN tenant_github_installations i ON i.tenant_id=b.tenant_id AND i.installation_id=b.installation_id
		WHERE r.tenant_id=$1 AND r.installation_id=$2 AND r.github_repository_id=$3
		AND r.actions_runner_enabled=TRUE AND r.actions_runner_enabled_at IS NOT NULL
		AND b.tenant_id=r.tenant_id AND b.installation_id=r.installation_id AND i.status='active' AND i.suspended_at IS NULL
		RETURNING r.actions_runner_workflow_policy`, tenantID, installationID, repositoryID, encoded).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return ActionsRunnerWorkflowPolicy{}, ErrRepositoryNotFound
	}
	if err != nil {
		return ActionsRunnerWorkflowPolicy{}, fmt.Errorf("store Actions runner workflow policy: %w", err)
	}
	var result ActionsRunnerWorkflowPolicy
	if json.Unmarshal(stored, &result) != nil || !validActionsRunnerWorkflowPolicy(result) {
		return ActionsRunnerWorkflowPolicy{}, errors.New("decode stored Actions runner workflow policy")
	}
	return result, nil
}

func GetRepositoryActionsRunnerWorkflowPolicy(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, installationID, repositoryID int64) (ActionsRunnerWorkflowPolicy, error) {
	if ctx == nil || tx == nil || tenantID == "" || installationID <= 0 || repositoryID <= 0 {
		return ActionsRunnerWorkflowPolicy{}, ErrRepositoryNotFound
	}
	var stored []byte
	err := tx.QueryRow(ctx, `SELECT r.actions_runner_workflow_policy
		FROM tenant_github_repositories r
		JOIN github_app_installation_bindings b ON b.tenant_id=r.tenant_id AND b.installation_id=r.installation_id
		JOIN tenant_github_installations i ON i.tenant_id=b.tenant_id AND i.installation_id=b.installation_id
		WHERE r.tenant_id=$1 AND r.installation_id=$2 AND r.github_repository_id=$3
		AND r.actions_runner_enabled=TRUE AND i.status='active' AND i.suspended_at IS NULL`, tenantID, installationID, repositoryID).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return ActionsRunnerWorkflowPolicy{}, ErrRepositoryNotFound
	}
	if err != nil {
		return ActionsRunnerWorkflowPolicy{}, fmt.Errorf("read Actions runner workflow policy: %w", err)
	}
	var result ActionsRunnerWorkflowPolicy
	if len(stored) == 0 || json.Unmarshal(stored, &result) != nil || !validActionsRunnerWorkflowPolicy(result) {
		return ActionsRunnerWorkflowPolicy{}, ErrRepositoryNotFound
	}
	return result, nil
}

func validActionsRunnerWorkflowPolicy(policy ActionsRunnerWorkflowPolicy) bool {
	if !regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9_.-]+\.ya?ml$`).MatchString(policy.WorkflowPath) ||
		!validUniquePolicyValues(policy.AllowedRefs, 32, 255) || !validUniquePolicyEvents(policy.AllowedEvents) ||
		!validUniquePolicyValues(policy.RequiredLabels, 32, 100) || !validUniquePolicyValues(policy.AllowedLabels, 32, 100) {
		return false
	}
	for _, ref := range policy.AllowedRefs {
		if !actionsRunnerBranchRefPattern.MatchString(ref) || strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") {
			return false
		}
	}
	for _, label := range policy.RequiredLabels {
		if !slices.Contains(policy.AllowedLabels, label) {
			return false
		}
	}
	return true
}

func ValidActionsRunnerWorkflowPolicy(policy ActionsRunnerWorkflowPolicy) bool {
	return validActionsRunnerWorkflowPolicy(policy)
}

func validUniquePolicyValues(values []string, maximum, maxLength int) bool {
	if len(values) == 0 || len(values) > maximum {
		return false
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != value || value == "" || len(value) > maxLength || strings.ContainsAny(value, "\r\n\x00") {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validUniquePolicyEvents(values []string) bool {
	if !validUniquePolicyValues(values, 8, 40) {
		return false
	}
	for _, event := range values {
		if event != "push" && event != "pull_request" && event != "workflow_dispatch" && event != "schedule" {
			return false
		}
	}
	return true
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
