//go:build integration

package githubwebhook

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestInboxRecordIsDurableAndIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	inbox := Inbox{Pool: pool, Policy: transaction.DefaultPolicy()}
	now := time.Now().UTC()
	delivery := githubwebhook.VerifiedDelivery{
		DeliveryID: "a1b2c3d4-e5f6-4789-8abc-def012345678",
		Event:      "pull_request", Action: "opened", Payload: []byte(`{"action":"opened","installation":{"id":12},"repository":{"id":42,"name":"api","owner":{"login":"acme"}},"pull_request":{"number":17,"state":"open","head":{"ref":"feature/example","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"base":{"ref":"main"}}}`),
	}
	inserted, err := inbox.Record(ctx, delivery, now)
	if err != nil || !inserted {
		t.Fatalf("first record = inserted %v, err %v", inserted, err)
	}
	inserted, err = inbox.Record(ctx, delivery, now.Add(time.Second))
	if err != nil || inserted {
		t.Fatalf("replay = inserted %v, err %v", inserted, err)
	}
	conflicting := delivery
	conflicting.Payload = []byte(strings.Replace(string(delivery.Payload), `"action":"opened"`, `"action":"opened","changed":true`, 1))
	if _, err := inbox.Record(ctx, conflicting, now); !errors.Is(err, ErrDeliveryIDConflict) {
		t.Fatalf("delivery ID collision error = %v, want %v", err, ErrDeliveryIDConflict)
	}
	var count int
	var payload string
	var schemaVersion int
	var eventJSON string
	if err := pool.QueryRow(ctx, `SELECT COUNT(*), MIN(payload_json), MAX(event_schema_version), MIN(normalized_event_json) FROM github_webhook_deliveries WHERE delivery_id = $1`, delivery.DeliveryID).Scan(&count, &payload, &schemaVersion, &eventJSON); err != nil {
		t.Fatal(err)
	}
	if count != 1 || payload != string(delivery.Payload) || schemaVersion != githubwebhook.RepositoryEventSchemaVersion {
		t.Fatalf("persisted rows = %d schema=%d payload=%s; want one original payload and normalized schema", count, schemaVersion, payload)
	}
	var normalized githubwebhook.RepositoryEvent
	if err := json.Unmarshal([]byte(eventJSON), &normalized); err != nil || normalized.Type != "repository.pull_request.opened" || normalized.Repository == nil || normalized.Repository.GitHubID != 42 || normalized.PullRequest == nil || normalized.PullRequest.Number != 17 {
		t.Fatalf("persisted normalized event = %+v; decode error=%v", normalized, err)
	}
}

func TestProcessInstallationRepositoryDeliveryIsAtomicAndRetryable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, created_at) VALUES ('tenant_hook_process', 'Webhook processing', $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('usr_hook_process', 'hook-process-sub', '', $1)`, now); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"action":"added","installation":{"id":701},"repositories_added":[{"id":702,"name":"api","default_branch":"main","owner":{"login":"acme"}}]}`)
	delivery := githubwebhook.VerifiedDelivery{
		DeliveryID: "b1b2c3d4-e5f6-4789-8abc-def012345678",
		Event:      "installation_repositories", Action: "added", Payload: body,
	}
	inbox := Inbox{Pool: pool, Policy: transaction.DefaultPolicy()}
	if _, err := inbox.Record(ctx, delivery, now); err != nil {
		t.Fatalf("record delivery: %v", err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return ProcessInstallationRepositoryDelivery(ctx, tx, delivery.DeliveryID, now)
	}); !errors.Is(err, githubregistry.ErrInstallationNotActive) {
		t.Fatalf("process without active binding error = %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT processing_status FROM github_webhook_deliveries WHERE delivery_id = $1`, delivery.DeliveryID).Scan(&status); err != nil || status != "received" {
		t.Fatalf("inactive delivery state = %q, err=%v; want received", status, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_github_installations (tenant_id, installation_id, account_login, status, requested_by, requested_at, updated_at)
		VALUES ('tenant_hook_process', 701, 'acme', 'active', 'usr_hook_process', $1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO github_app_installation_bindings (installation_id, tenant_id, bound_at) VALUES (701, 'tenant_hook_process', $1)`, now); err != nil {
		t.Fatal(err)
	}
	processed, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return ProcessInstallationRepositoryDelivery(ctx, tx, delivery.DeliveryID, now)
	})
	if err != nil || !processed {
		t.Fatalf("process delivery = %v, err=%v", processed, err)
	}
	processed, err = transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return ProcessInstallationRepositoryDelivery(ctx, tx, delivery.DeliveryID, now)
	})
	if err != nil || processed {
		t.Fatalf("reprocess delivery = %v, err=%v", processed, err)
	}
	if err := pool.QueryRow(ctx, `SELECT processing_status FROM github_webhook_deliveries WHERE delivery_id = $1`, delivery.DeliveryID).Scan(&status); err != nil || status != "processed" {
		t.Fatalf("processed delivery state = %q, err=%v", status, err)
	}
	var repositoryCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM tenant_github_repositories WHERE tenant_id = 'tenant_hook_process' AND installation_id = 701 AND github_repository_id = 702`).Scan(&repositoryCount); err != nil {
		t.Fatalf("read synced Repository: %v", err)
	}
	if repositoryCount != 1 {
		t.Fatalf("synced Repository count = %d, want 1", repositoryCount)
	}
}

func TestProcessBatchDrainsOnlyActiveInstallationRepositoryEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, created_at) VALUES ('tenant_hook_batch', 'Webhook batch', $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('usr_hook_batch', 'hook-batch-sub', '', $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_github_installations (tenant_id, installation_id, account_login, status, requested_by, requested_at, updated_at)
		VALUES ('tenant_hook_batch', 801, 'acme', 'active', 'usr_hook_batch', $1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO github_app_installation_bindings (installation_id, tenant_id, bound_at) VALUES (801, 'tenant_hook_batch', $1)`, now); err != nil {
		t.Fatal(err)
	}
	inbox := Inbox{Pool: pool, Policy: transaction.DefaultPolicy()}
	active := githubwebhook.VerifiedDelivery{DeliveryID: "c1b2c3d4-e5f6-4789-8abc-def012345678", Event: "installation_repositories", Action: "added", Payload: []byte(`{"action":"added","installation":{"id":801},"repositories_added":[{"id":802,"name":"api","default_branch":"main","owner":{"login":"acme"}}]}`)}
	inactive := githubwebhook.VerifiedDelivery{DeliveryID: "d1b2c3d4-e5f6-4789-8abc-def012345678", Event: "installation_repositories", Action: "added", Payload: []byte(`{"action":"added","installation":{"id":803},"repositories_added":[{"id":804,"name":"worker","default_branch":"main","owner":{"login":"acme"}}]}`)}
	otherEvent := githubwebhook.VerifiedDelivery{DeliveryID: "e1b2c3d4-e5f6-4789-8abc-def012345678", Event: "pull_request", Action: "opened", Payload: []byte(`{"action":"opened","installation":{"id":801},"repository":{"id":805,"name":"api","owner":{"login":"acme"}},"pull_request":{"number":1,"state":"open","head":{"ref":"feature/test","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"base":{"ref":"main"}}}`)}
	for _, delivery := range []githubwebhook.VerifiedDelivery{active, inactive, otherEvent} {
		if _, err := inbox.Record(ctx, delivery, now); err != nil {
			t.Fatalf("record %s: %v", delivery.Event, err)
		}
	}
	stats, err := ProcessBatch(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(time.Second))
	if err != nil {
		t.Fatalf("process batch: %v", err)
	}
	if stats != (BatchStats{Claimed: 1, Processed: 1}) {
		t.Fatalf("batch stats = %+v, want one eligible processed event", stats)
	}
	stats, err = ProcessBatch(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(2*time.Second))
	if err != nil || stats != (BatchStats{}) {
		t.Fatalf("empty batch stats = %+v, err=%v", stats, err)
	}
	var activeStatus, inactiveStatus, otherStatus string
	for id, target := range map[string]*string{active.DeliveryID: &activeStatus, inactive.DeliveryID: &inactiveStatus, otherEvent.DeliveryID: &otherStatus} {
		if err := pool.QueryRow(ctx, `SELECT processing_status FROM github_webhook_deliveries WHERE delivery_id = $1`, id).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if activeStatus != "processed" || inactiveStatus != "received" || otherStatus != "received" {
		t.Fatalf("statuses active=%s inactive=%s other=%s", activeStatus, inactiveStatus, otherStatus)
	}
}

func TestProcessBatchAppliesInstallationSuspendUnsuspendAndDeleteLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, created_at) VALUES ('tenant_hook_lifecycle', 'Webhook lifecycle', $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('usr_hook_lifecycle', 'hook-lifecycle-sub', '', $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_github_installations (tenant_id, installation_id, account_login, status, requested_by, requested_at, updated_at)
		VALUES ('tenant_hook_lifecycle', 901, 'acme', 'active', 'usr_hook_lifecycle', $1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO github_app_installation_bindings (installation_id, tenant_id, bound_at) VALUES (901, 'tenant_hook_lifecycle', $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_github_repositories (tenant_id, installation_id, github_repository_id, owner_login, repository_name, default_branch, synchronized_at)
		VALUES ('tenant_hook_lifecycle', 901, 902, 'acme', 'api', 'main', $1)`, now); err != nil {
		t.Fatal(err)
	}
	inbox := Inbox{Pool: pool, Policy: transaction.DefaultPolicy()}
	makeDelivery := func(id, action string) githubwebhook.VerifiedDelivery {
		return githubwebhook.VerifiedDelivery{DeliveryID: id, Event: "installation", Action: action, Payload: []byte(`{"action":"` + action + `","installation":{"id":901}}`)}
	}
	suspended := makeDelivery("f1b2c3d4-e5f6-4789-8abc-def012345678", "suspend")
	unsuspended := makeDelivery("f2b2c3d4-e5f6-4789-8abc-def012345678", "unsuspend")
	deleted := makeDelivery("f3b2c3d4-e5f6-4789-8abc-def012345678", "deleted")
	process := func(delivery githubwebhook.VerifiedDelivery, timestamp time.Time) {
		t.Helper()
		if inserted, err := inbox.Record(ctx, delivery, timestamp); err != nil || !inserted {
			t.Fatalf("record %s event: inserted=%v err=%v", delivery.Action, inserted, err)
		}
		stats, err := ProcessBatch(ctx, pool, transaction.DefaultPolicy(), 10, timestamp.Add(time.Second))
		if err != nil || stats != (BatchStats{Claimed: 1, Processed: 1}) {
			t.Fatalf("process %s event = %+v, err=%v", delivery.Action, stats, err)
		}
	}
	process(suspended, now)
	installations, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]githubregistry.Installation, error) {
		return githubregistry.List(ctx, tx, "tenant_hook_lifecycle")
	})
	if err != nil || len(installations) != 1 || installations[0].Status != "active" || !installations[0].Suspended {
		t.Fatalf("suspended Tenant Installation = %+v, err=%v", installations, err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]githubregistry.Repository, error) {
		return githubregistry.ListRepositories(ctx, tx, "tenant_hook_lifecycle", 901)
	}); !errors.Is(err, githubregistry.ErrInstallationNotActive) {
		t.Fatalf("suspended Installation Repository access error = %v, want inactive", err)
	}
	process(unsuspended, now.Add(2*time.Second))
	repositories, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]githubregistry.Repository, error) {
		return githubregistry.ListRepositories(ctx, tx, "tenant_hook_lifecycle", 901)
	})
	if err != nil || len(repositories) != 1 {
		t.Fatalf("unsuspended Installation repositories = %+v, err=%v", repositories, err)
	}
	process(deleted, now.Add(4*time.Second))
	var status string
	var suspendedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, suspended_at FROM tenant_github_installations WHERE tenant_id = 'tenant_hook_lifecycle' AND installation_id = 901`).Scan(&status, &suspendedAt); err != nil {
		t.Fatal(err)
	}
	if status != "revoked" || suspendedAt != nil {
		t.Fatalf("deleted Installation state = status %q suspended_at %v", status, suspendedAt)
	}
	var repositoryCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM tenant_github_repositories WHERE tenant_id = 'tenant_hook_lifecycle' AND installation_id = 901`).Scan(&repositoryCount); err != nil || repositoryCount != 0 {
		t.Fatalf("deleted Installation repositories = %d, err=%v", repositoryCount, err)
	}
	installations, err = transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]githubregistry.Installation, error) {
		return githubregistry.List(ctx, tx, "tenant_hook_lifecycle")
	})
	if err != nil || len(installations) != 1 || installations[0].Status != "revoked" || installations[0].Suspended {
		t.Fatalf("Tenant Installation record = %+v, err=%v", installations, err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]githubregistry.Repository, error) {
		return githubregistry.ListRepositories(ctx, tx, "tenant_hook_lifecycle", 901)
	}); !errors.Is(err, githubregistry.ErrInstallationNotActive) {
		t.Fatalf("deleted Installation Repository access error = %v, want inactive", err)
	}
}
