//go:build integration

package githubregistry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestRepositorySyncRequiresActiveTenantBindingAndAppliesMembershipEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, created_at) VALUES ('tenant_registry', 'Registry', $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('usr_registry', 'registry-sub', '', $1)`, now); err != nil {
		t.Fatal(err)
	}
	requestTx := mustBegin(t, pool, ctx)
	if _, _, err := Request(ctx, requestTx, Installation{TenantID: "tenant_registry", InstallationID: 4401, AccountLogin: "acme", RequestedBy: "usr_registry"}, now); err != nil {
		_ = requestTx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := requestTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// A pending claim must not be sufficient to sync repositories.
	pendingTx := mustBegin(t, pool, ctx)
	if err := SyncInstallationRepositories(ctx, pendingTx, "tenant_registry", addedRepositoriesEvent(), now); !errors.Is(err, ErrInstallationNotActive) {
		_ = pendingTx.Rollback(ctx)
		t.Fatalf("sync pending Installation error = %v", err)
	}
	_ = pendingTx.Rollback(ctx)

	tx := mustBegin(t, pool, ctx)
	if _, err := tx.Exec(ctx, `UPDATE tenant_github_installations SET status = 'active', updated_at = $3 WHERE tenant_id = $1 AND installation_id = $2`, "tenant_registry", int64(4401), now); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO github_app_installation_bindings (installation_id, tenant_id, bound_at) VALUES ($1, $2, $3)`, int64(4401), "tenant_registry", now); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := SyncInstallationRepositories(ctx, tx, "tenant_registry", addedRepositoriesEvent(), now); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("sync added repositories: %v", err)
	}
	if err := SyncInstallationRepositories(ctx, tx, "tenant_registry", addedRepositoriesEvent(), now.Add(time.Second)); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("idempotent repository sync: %v", err)
	}
	repositories, err := ListRepositories(ctx, tx, "tenant_registry", 4401)
	if err != nil || len(repositories) != 1 || repositories[0].GitHubRepositoryID != 8842 || repositories[0].RepositoryName != "api" {
		_ = tx.Rollback(ctx)
		t.Fatalf("list synced repositories = %+v, err %v", repositories, err)
	}
	removedEvent := addedRepositoriesEvent()
	removedEvent.Type = "github.installation_repositories.removed"
	removedEvent.RepositoriesAdded = nil
	removedEvent.RepositoriesRemoved = []githubwebhook.RepositoryRef{{GitHubID: 8842, Owner: "acme", Name: "api"}}
	if err := SyncInstallationRepositories(ctx, tx, "tenant_registry", removedEvent, now.Add(2*time.Second)); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("sync removed repository: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	readTx := mustBegin(t, pool, ctx)
	defer readTx.Rollback(ctx)
	repositories, err = ListRepositories(ctx, readTx, "tenant_registry", 4401)
	if err != nil || len(repositories) != 0 {
		t.Fatalf("repositories after removal = %+v, err %v", repositories, err)
	}
	if _, err := ListRepositories(ctx, readTx, domain.TenantID("tenant_other"), 4401); !errors.Is(err, ErrInstallationNotActive) {
		t.Fatalf("cross-Tenant repository list error = %v", err)
	}
}

func addedRepositoriesEvent() githubwebhook.RepositoryEvent {
	return githubwebhook.RepositoryEvent{
		SchemaVersion: githubwebhook.RepositoryEventSchemaVersion,
		Type:          "github.installation_repositories.added", GitHubEvent: "installation_repositories",
		Action: "added", InstallationID: 4401,
		RepositoriesAdded: []githubwebhook.RepositoryRef{{GitHubID: 8842, Owner: "acme", Name: "api", DefaultBranch: "main"}},
	}
}

func mustBegin(t *testing.T, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, ctx context.Context) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}
