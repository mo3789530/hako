//go:build integration

package githubwebhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	otherIntegration := delivery
	otherIntegration.IntegrationID = "integration_other"
	otherIntegration.CredentialGeneration = 7
	if inserted, err = inbox.Record(ctx, otherIntegration, now); err != nil || !inserted {
		t.Fatalf("same Delivery ID in another integration = inserted %v, err %v", inserted, err)
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
	if count != 2 || payload != string(delivery.Payload) || schemaVersion != githubwebhook.RepositoryEventSchemaVersion {
		t.Fatalf("persisted rows = %d schema=%d payload=%s; want one row per integration and normalized schema", count, schemaVersion, payload)
	}
	var normalized githubwebhook.RepositoryEvent
	if err := json.Unmarshal([]byte(eventJSON), &normalized); err != nil || normalized.Type != "repository.pull_request.opened" || normalized.Repository == nil || normalized.Repository.GitHubID != 42 || normalized.PullRequest == nil || normalized.PullRequest.Number != 17 {
		t.Fatalf("persisted normalized event = %+v; decode error=%v", normalized, err)
	}
}

func TestRawWebhookPayloadExpiresWithoutLosingNormalizedEventOrIdempotency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	inbox := Inbox{Pool: pool, Policy: transaction.DefaultPolicy()}
	delivery := githubwebhook.VerifiedDelivery{
		DeliveryID: "a2b2c3d4-e5f6-4789-8abc-def012345678", Event: "push", Action: "",
		Payload: []byte(`{"ref":"refs/heads/main","after":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","installation":{"id":44},"repository":{"id":45,"name":"api","owner":{"login":"acme"}},"sender":{"login":"octocat"}}`),
	}
	if inserted, err := inbox.Record(ctx, delivery, now); err != nil || !inserted {
		t.Fatalf("record webhook delivery = inserted %v err=%v", inserted, err)
	}
	beforeExpiry, err := PurgeExpiredRawPayloads(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(RawPayloadRetention-time.Second))
	if err != nil || beforeExpiry != 0 {
		t.Fatalf("premature raw payload cleanup = %d err=%v", beforeExpiry, err)
	}
	purged, err := PurgeExpiredRawPayloads(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(RawPayloadRetention))
	if err != nil || purged != 1 {
		t.Fatalf("expired raw payload cleanup = %d err=%v", purged, err)
	}
	var payload, payloadHash, normalizedJSON, actor string
	var purgedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT payload_json,payload_sha256,normalized_event_json,actor_login,payload_purged_at
		FROM github_webhook_deliveries WHERE integration_id='shared' AND delivery_id=$1`, delivery.DeliveryID).
		Scan(&payload, &payloadHash, &normalizedJSON, &actor, &purgedAt); err != nil {
		t.Fatal(err)
	}
	if payload != "" || payloadHash == "" || purgedAt == nil || actor != "octocat" {
		t.Fatalf("retained webhook metadata = payload=%q hash=%q actor=%q purged=%v", payload, payloadHash, actor, purgedAt)
	}
	var event githubwebhook.RepositoryEvent
	if err := json.Unmarshal([]byte(normalizedJSON), &event); err != nil || event.Type != "repository.push" || event.Actor != "octocat" || event.CommitSHA != strings.Repeat("a", 40) {
		t.Fatalf("normalized event was not retained: %+v err=%v", event, err)
	}
	if inserted, err := inbox.Record(ctx, delivery, now.Add(RawPayloadRetention+time.Second)); err != nil || inserted {
		t.Fatalf("duplicate after raw payload purge = inserted %v err=%v", inserted, err)
	}
	conflicting := delivery
	conflicting.Payload = []byte(strings.Replace(string(delivery.Payload), "octocat", "another-user", 1))
	if _, err := inbox.Record(ctx, conflicting, now.Add(RawPayloadRetention+2*time.Second)); !errors.Is(err, ErrDeliveryIDConflict) {
		t.Fatalf("purged delivery ID was reused with new content: %v", err)
	}
	if again, err := PurgeExpiredRawPayloads(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(2*RawPayloadRetention)); err != nil || again != 0 {
		t.Fatalf("raw payload purge was not idempotent: count=%d err=%v", again, err)
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
	firstTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer firstTx.Rollback(ctx)
	processed, err := ProcessInstallationRepositoryDelivery(ctx, firstTx, delivery.DeliveryID, now)
	if err != nil || !processed {
		t.Fatalf("process delivery = %v, err=%v", processed, err)
	}
	secondDone := make(chan struct {
		processed bool
		err       error
	}, 1)
	var started sync.WaitGroup
	started.Add(1)
	go func() {
		started.Done()
		processed, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
			return ProcessInstallationRepositoryDelivery(ctx, tx, delivery.DeliveryID, now)
		})
		secondDone <- struct {
			processed bool
			err       error
		}{processed, err}
	}()
	started.Wait()
	select {
	case result := <-secondDone:
		t.Fatalf("concurrent duplicate finished before first delivery transaction committed: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
	if err := firstTx.Commit(ctx); err != nil {
		t.Fatalf("commit first delivery processor: %v", err)
	}
	second := <-secondDone
	if second.err != nil || second.processed {
		t.Fatalf("concurrent duplicate result = processed %v err=%v; want a fenced no-op", second.processed, second.err)
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

func TestActionsRunnerAdmissionRequiresSeparateRepositoryOptInAndDeduplicatesJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, query := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_actions_admission','Actions admission',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_actions_admission','actions-admission','',now())`,
		`INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_actions_admission',8801,'acme','active','usr_actions_admission',now(),now())`,
		`INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at) VALUES (8801,'tenant_actions_admission',now())`,
		`INSERT INTO tenant_github_repositories (tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,actions_runner_enabled,actions_runner_enabled_at) VALUES ('tenant_actions_admission',8801,8802,'acme','enabled','main',now(),true,now()-interval '1 minute'),('tenant_actions_admission',8801,8803,'acme','disabled','main',now(),false,NULL)`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	inbox := Inbox{Pool: pool, Policy: transaction.DefaultPolicy()}
	makeDelivery := func(id string, repositoryID int, name string) githubwebhook.VerifiedDelivery {
		return githubwebhook.VerifiedDelivery{DeliveryID: id, Event: "workflow_job", Action: "queued", Payload: []byte(fmt.Sprintf(`{"action":"queued","installation":{"id":8801},"repository":{"id":%d,"name":%q,"owner":{"login":"acme"}},"workflow_job":{"id":9901,"run_id":9902,"run_attempt":1,"workflow_name":"CI","name":"test","head_branch":"feature/task","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","status":"queued","labels":["self-hosted","hako"]}}`, repositoryID, name))}
	}
	accepted := makeDelivery("a3b2c3d4-e5f6-4789-8abc-def012345678", 8802, "enabled")
	duplicate := makeDelivery("b3b2c3d4-e5f6-4789-8abc-def012345678", 8802, "enabled")
	optedOut := makeDelivery("c3b2c3d4-e5f6-4789-8abc-def012345678", 8803, "disabled")
	preOptIn := makeDelivery("d3b2c3d4-e5f6-4789-8abc-def012345678", 8802, "enabled")
	missingAttempt := makeDelivery("343b2c3d-e5f6-4789-8abc-def012345678", 8802, "enabled")
	missingAttempt.Payload = []byte(strings.Replace(string(missingAttempt.Payload), `,"run_attempt":1`, "", 1))
	for _, item := range []struct {
		delivery githubwebhook.VerifiedDelivery
		received time.Time
	}{{accepted, now}, {duplicate, now}, {optedOut, now}, {preOptIn, now.Add(-2 * time.Minute)}, {missingAttempt, now}} {
		if inserted, err := inbox.Record(ctx, item.delivery, item.received); err != nil || !inserted {
			t.Fatalf("record %s: inserted=%v error=%v", item.delivery.DeliveryID, inserted, err)
		}
	}
	stats, err := ProcessBatchWithActionsRunnerAdmissions(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(time.Second), nil)
	if err != nil || stats.Claimed != 5 || stats.Processed != 5 || stats.ActionsRunnerJobsAdmitted != 1 {
		t.Fatalf("Actions runner admission batch = %+v, error=%v", stats, err)
	}
	var jobs int
	var tenantID string
	var labels []byte
	var state string
	if err := pool.QueryRow(ctx, `SELECT count(*),min(tenant_id),min(labels::text),min(state) FROM github_actions_runner_jobs`).Scan(&jobs, &tenantID, &labels, &state); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 || tenantID != "tenant_actions_admission" || string(labels) != `["self-hosted", "hako"]` || state != "admitted" {
		t.Fatalf("admitted Actions job count=%d tenant=%q labels=%s state=%q", jobs, tenantID, labels, state)
	}
	for _, rejected := range []githubwebhook.VerifiedDelivery{optedOut, preOptIn, missingAttempt} {
		var status string
		if err := pool.QueryRow(ctx, `SELECT processing_status FROM github_webhook_deliveries WHERE delivery_id=$1`, rejected.DeliveryID).Scan(&status); err != nil || status != "failed" {
			t.Fatalf("ineligible repository delivery %s status=%q error=%v", rejected.DeliveryID, status, err)
		}
	}
	var uncorrelatableJobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM github_actions_runner_jobs WHERE run_attempt=0`).Scan(&uncorrelatableJobs); err != nil || uncorrelatableJobs != 0 {
		t.Fatalf("admitted uncorrelatable zero-attempt runner jobs: count=%d error=%v", uncorrelatableJobs, err)
	}
	var queuedWorkloadRuns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_runs`).Scan(&queuedWorkloadRuns); err != nil || queuedWorkloadRuns != 0 {
		t.Fatalf("admission unexpectedly created Workload/MicroVM request: count=%d error=%v", queuedWorkloadRuns, err)
	}
	claimTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := ClaimNextActionsRunnerJob(ctx, claimTx, "controller-one", now.Add(2*time.Second), 30*time.Second)
	if err != nil || !ok || claimed.State != "claimed" || claimed.ClaimID != "controller-one" || claimed.ClaimAttempts != 1 || claimed.JobID != 9901 || len(claimed.Labels) != 2 {
		_ = claimTx.Rollback(ctx)
		t.Fatalf("claim admitted job=%+v ok=%v error=%v", claimed, ok, err)
	}
	if err := claimTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	secondClaimTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ClaimNextActionsRunnerJob(ctx, secondClaimTx, "controller-two", now.Add(3*time.Second), 30*time.Second); err != nil || ok {
		_ = secondClaimTx.Rollback(ctx)
		t.Fatalf("live job lease was claimed twice: ok=%v error=%v", ok, err)
	}
	if err := secondClaimTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	recoveryTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recovered, ok, err := ClaimNextActionsRunnerJob(ctx, recoveryTx, "controller-recovery", now.Add(40*time.Second), 30*time.Second)
	if err != nil || !ok || recovered.ClaimID != "controller-recovery" || recovered.ClaimAttempts != 2 {
		_ = recoveryTx.Rollback(ctx)
		t.Fatalf("expired lease recovery=%+v ok=%v error=%v", recovered, ok, err)
	}
	if err := recoveryTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err = ProcessBatchWithActionsRunnerAdmissions(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(2*time.Second), nil)
	if err != nil || stats != (BatchStats{}) {
		t.Fatalf("replayed admission batch = %+v, error=%v", stats, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE github_actions_runner_jobs SET state='starting',claim_id='runner-controller',lease_expires_at=$2,updated_at=$1
		WHERE integration_id='shared' AND workflow_job_id=9901 AND run_attempt=1`, now.Add(41*time.Second), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	makeStatusDelivery := func(id, action, status, conclusion, sha string) githubwebhook.VerifiedDelivery {
		payload := fmt.Sprintf(`{"action":%q,"installation":{"id":8801},"repository":{"id":8802,"name":"enabled","owner":{"login":"acme"}},"workflow_job":{"id":9901,"run_id":9902,"run_attempt":1,"workflow_name":"CI","name":"test","head_branch":"feature/task","head_sha":%q,"status":%q,"conclusion":%q,"labels":["self-hosted","hako"],"runner_id":9810,"runner_name":"hako-runner-1"}}`, action, sha, status, conclusion)
		return githubwebhook.VerifiedDelivery{DeliveryID: id, Event: "workflow_job", Action: action, Payload: []byte(payload)}
	}
	wrongSHA := makeStatusDelivery("e3b2c3d4-e5f6-4789-8abc-def012345678", "in_progress", "in_progress", "", strings.Repeat("b", 40))
	if inserted, err := inbox.Record(ctx, wrongSHA, now.Add(42*time.Second)); err != nil || !inserted {
		t.Fatalf("record mismatched Actions assignment: inserted=%v error=%v", inserted, err)
	}
	stats, err = ProcessBatchWithActionsRunnerAdmissions(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(43*time.Second), nil)
	if err != nil || stats.Processed != 1 {
		t.Fatalf("process mismatched Actions assignment = %+v error=%v", stats, err)
	}
	var runnerState string
	var runnerID *int64
	if err := pool.QueryRow(ctx, `SELECT state,runner_id FROM github_actions_runner_jobs WHERE integration_id='shared' AND workflow_job_id=9901 AND run_attempt=1`).Scan(&runnerState, &runnerID); err != nil || runnerState != "starting" || runnerID != nil {
		t.Fatalf("mismatched SHA changed Actions assignment: state=%q runner=%v error=%v", runnerState, runnerID, err)
	}
	inProgress := makeStatusDelivery("f3b2c3d4-e5f6-4789-8abc-def012345678", "in_progress", "in_progress", "", strings.Repeat("a", 40))
	if inserted, err := inbox.Record(ctx, inProgress, now.Add(44*time.Second)); err != nil || !inserted {
		t.Fatalf("record Actions in_progress: inserted=%v error=%v", inserted, err)
	}
	stats, err = ProcessBatchWithActionsRunnerAdmissions(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(45*time.Second), nil)
	if err != nil || stats.Processed != 1 {
		t.Fatalf("process Actions in_progress = %+v error=%v", stats, err)
	}
	if err := pool.QueryRow(ctx, `SELECT state,runner_id FROM github_actions_runner_jobs WHERE integration_id='shared' AND workflow_job_id=9901 AND run_attempt=1`).Scan(&runnerState, &runnerID); err != nil || runnerState != "running" || runnerID == nil || *runnerID != 9810 {
		t.Fatalf("Actions runner assignment = state=%q runner=%v error=%v", runnerState, runnerID, err)
	}
	completed := makeStatusDelivery("043b2c3d-4e5f-4789-8abc-def012345678", "completed", "completed", "success", strings.Repeat("a", 40))
	if inserted, err := inbox.Record(ctx, completed, now.Add(46*time.Second)); err != nil || !inserted {
		t.Fatalf("record Actions completed: inserted=%v error=%v", inserted, err)
	}
	stats, err = ProcessBatchWithActionsRunnerAdmissions(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(47*time.Second), nil)
	if err != nil || stats.Processed != 1 {
		t.Fatalf("process Actions completed = %+v error=%v", stats, err)
	}
	var conclusion string
	var completedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT state,runner_id,conclusion,completed_at FROM github_actions_runner_jobs WHERE integration_id='shared' AND workflow_job_id=9901 AND run_attempt=1`).Scan(&runnerState, &runnerID, &conclusion, &completedAt); err != nil || runnerState != "completed" || runnerID == nil || *runnerID != 9810 || conclusion != "success" || completedAt == nil {
		t.Fatalf("Actions completion = state=%q runner=%v conclusion=%q at=%v error=%v", runnerState, runnerID, conclusion, completedAt, err)
	}
	reordered := makeStatusDelivery("143b2c3d-4e5f-4789-8abc-def012345678", "in_progress", "in_progress", "", strings.Repeat("a", 40))
	if inserted, err := inbox.Record(ctx, reordered, now.Add(48*time.Second)); err != nil || !inserted {
		t.Fatalf("record reordered Actions in_progress: inserted=%v error=%v", inserted, err)
	}
	stats, err = ProcessBatchWithActionsRunnerAdmissions(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(49*time.Second), nil)
	if err != nil || stats.Processed != 1 {
		t.Fatalf("process reordered Actions in_progress = %+v error=%v", stats, err)
	}
	if err := pool.QueryRow(ctx, `SELECT state,runner_id,conclusion FROM github_actions_runner_jobs WHERE integration_id='shared' AND workflow_job_id=9901 AND run_attempt=1`).Scan(&runnerState, &runnerID, &conclusion); err != nil || runnerState != "completed" || runnerID == nil || *runnerID != 9810 || conclusion != "success" {
		t.Fatalf("reordered status reopened completed job: state=%q runner=%v conclusion=%q error=%v", runnerState, runnerID, conclusion, err)
	}
	missingStatusAttempt := makeStatusDelivery("243b2c3d-4e5f-4789-8abc-def012345678", "in_progress", "in_progress", "", strings.Repeat("a", 40))
	missingStatusAttempt.Payload = []byte(strings.Replace(string(missingStatusAttempt.Payload), `,"run_attempt":1`, "", 1))
	if inserted, err := inbox.Record(ctx, missingStatusAttempt, now.Add(50*time.Second)); err != nil || !inserted {
		t.Fatalf("record Actions status without attempt: inserted=%v error=%v", inserted, err)
	}
	stats, err = ProcessBatchWithActionsRunnerAdmissions(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(51*time.Second), nil)
	if err != nil || stats.Processed != 1 {
		t.Fatalf("process uncorrelatable Actions status = %+v error=%v", stats, err)
	}
	if err := pool.QueryRow(ctx, `SELECT state,runner_id,conclusion FROM github_actions_runner_jobs WHERE integration_id='shared' AND workflow_job_id=9901 AND run_attempt=1`).Scan(&runnerState, &runnerID, &conclusion); err != nil || runnerState != "completed" || runnerID == nil || *runnerID != 9810 || conclusion != "success" {
		t.Fatalf("uncorrelatable status changed completed job: state=%q runner=%v conclusion=%q error=%v", runnerState, runnerID, conclusion, err)
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

func TestTenantWebhookDeliveryGenerationIsRevalidatedBeforeQueueProcessing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	statements := []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_hook_generation','Webhook generation',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_hook_generation','hook-generation-sub','',now())`,
		`INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_hook_generation',9401,'acme','active','usr_hook_generation',now(),now())`,
		`INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at) VALUES (9401,'tenant_hook_generation',now())`,
		`INSERT INTO github_app_registrations (id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_hook_generation',9402,'tenant_owned','tenant_hook_generation','hook-generation','client',now())`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at) VALUES ('binding_hook_generation','tenant_hook_generation',9402,'approved','private-ref','oauth-ref','webhook-ref',now())`,
		`INSERT INTO tenant_github_integrations (id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_hook_generation','tenant_hook_generation','registration_hook_generation',9402,'binding_hook_generation','active',2,6,'hook-generation-key','hook-generation-hash','usr_hook_generation',now(),now())`,
		`INSERT INTO tenant_github_app_installation_bindings (tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_hook_generation','integration_hook_generation',9401,now())`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	inbox := Inbox{Pool: pool, Policy: transaction.DefaultPolicy()}
	makeDelivery := func(id string, generation int64, repositoryID int64) githubwebhook.VerifiedDelivery {
		payload := []byte(fmt.Sprintf(`{"action":"added","installation":{"id":9401},"repositories_added":[{"id":%d,"name":"repo-%d","default_branch":"main","owner":{"login":"acme"}}]}`, repositoryID, repositoryID))
		return githubwebhook.VerifiedDelivery{IntegrationID: "integration_hook_generation", CredentialGeneration: generation, DeliveryID: id, Event: "installation_repositories", Action: "added", Payload: payload}
	}
	stale := makeDelivery("f1b2c3d4-e5f6-4789-8abc-def012345678", 5, 9403)
	current := makeDelivery("f2b2c3d4-e5f6-4789-8abc-def012345678", 6, 9404)
	wrongApp := makeDelivery("f3b2c3d4-e5f6-4789-8abc-def012345678", 6, 9405)
	wrongApp.Payload = []byte(`{"action":"added","installation":{"id":9401,"app_id":9999},"repositories_added":[{"id":9405,"name":"wrong-app","owner":{"login":"acme"}}]}`)
	if inserted, err := inbox.Record(ctx, wrongApp, now); err != nil || !inserted {
		t.Fatalf("record wrong-App delivery: inserted=%t err=%v", inserted, err)
	}
	if inserted, err := inbox.Record(ctx, stale, now); err != nil || !inserted {
		t.Fatalf("record stale delivery: inserted=%t err=%v", inserted, err)
	}
	if inserted, err := inbox.Record(ctx, current, now); err != nil || !inserted {
		t.Fatalf("record current delivery: inserted=%t err=%v", inserted, err)
	}
	stats, err := ProcessBatch(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(time.Second))
	if err != nil || stats.Processed != 1 {
		t.Fatalf("process Tenant webhook batch: %+v err=%v", stats, err)
	}
	var staleStatus, currentStatus string
	if err := pool.QueryRow(ctx, `SELECT processing_status FROM github_webhook_deliveries WHERE integration_id='integration_hook_generation' AND delivery_id=$1`, stale.DeliveryID).Scan(&staleStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT processing_status FROM github_webhook_deliveries WHERE integration_id='integration_hook_generation' AND delivery_id=$1`, current.DeliveryID).Scan(&currentStatus); err != nil {
		t.Fatal(err)
	}
	if staleStatus != "failed" || currentStatus != "processed" {
		t.Fatalf("generation fencing statuses stale=%s current=%s", staleStatus, currentStatus)
	}
	var wrongAppStatus string
	if err := pool.QueryRow(ctx, `SELECT processing_status FROM github_webhook_deliveries WHERE integration_id='integration_hook_generation' AND delivery_id=$1`, wrongApp.DeliveryID).Scan(&wrongAppStatus); err != nil {
		t.Fatal(err)
	}
	var wrongAppRepos int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tenant_github_repositories WHERE tenant_id='tenant_hook_generation' AND github_repository_id=9405`).Scan(&wrongAppRepos); err != nil {
		t.Fatal(err)
	}
	if wrongAppStatus != "failed" || wrongAppRepos != 0 {
		t.Fatalf("wrong-App delivery mutated state: status=%s repositories=%d", wrongAppStatus, wrongAppRepos)
	}
	var staleRepo, currentRepo int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tenant_github_repositories WHERE tenant_id='tenant_hook_generation' AND github_repository_id=9403`).Scan(&staleRepo); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tenant_github_repositories WHERE tenant_id='tenant_hook_generation' AND github_repository_id=9404`).Scan(&currentRepo); err != nil {
		t.Fatal(err)
	}
	if staleRepo != 0 || currentRepo != 1 {
		t.Fatalf("stale/current repository mutation counts = %d/%d", staleRepo, currentRepo)
	}
	if err := pool.QueryRow(ctx, `UPDATE github_webhook_deliveries SET processing_status='failed'
		WHERE integration_id=$1 AND delivery_id=$2 RETURNING processing_status`, stale.IntegrationID, stale.DeliveryID).Scan(&staleStatus); err != nil {
		t.Fatal(err)
	}
	if err := RequestTenantDeliveryReplay(ctx, pool, transaction.DefaultPolicy(), "tenant_hook_generation", stale.IntegrationID,
		stale.DeliveryID, "usr_hook_generation", "retry stale signed event after review", now.Add(2*time.Second)); !errors.Is(err, ErrStaleTenantWebhook) {
		t.Fatalf("stale-generation delivery replay error = %v", err)
	}
	var currentReplayCount int
	for index := 0; index < MaxWebhookReplaysPerHour; index++ {
		if _, err := pool.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='failed'
			WHERE integration_id=$1 AND delivery_id=$2`, current.IntegrationID, current.DeliveryID); err != nil {
			t.Fatal(err)
		}
		if err := RequestTenantDeliveryReplay(ctx, pool, transaction.DefaultPolicy(), "tenant_hook_generation", current.IntegrationID,
			current.DeliveryID, "usr_hook_generation", "retry reviewed transient delivery failure", now.Add(time.Duration(index+3)*time.Second)); err != nil {
			t.Fatalf("eligible Tenant delivery replay %d: %v", index+1, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='failed'
		WHERE integration_id=$1 AND delivery_id=$2`, current.IntegrationID, current.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if err := RequestTenantDeliveryReplay(ctx, pool, transaction.DefaultPolicy(), "tenant_hook_generation", current.IntegrationID,
		current.DeliveryID, "usr_hook_generation", "retry reviewed transient delivery failure", now.Add(10*time.Second)); !errors.Is(err, ErrDeliveryReplayRateLimited) {
		t.Fatalf("Tenant delivery replay rate limit error = %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT replay_count FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2`, current.IntegrationID, current.DeliveryID).Scan(&currentReplayCount); err != nil || currentReplayCount != MaxWebhookReplaysPerHour {
		t.Fatalf("Tenant delivery replay count = %d err=%v", currentReplayCount, err)
	}
	var replayAuditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id='tenant_hook_generation' AND actor_user_id='usr_hook_generation'
		AND action='github.webhook.replay_requested' AND target_id=$1`, current.DeliveryID).Scan(&replayAuditCount); err != nil || replayAuditCount != MaxWebhookReplaysPerHour {
		t.Fatalf("Tenant delivery replay audit count = %d err=%v", replayAuditCount, err)
	}
	// A database failure must leave the delivery retryable, not quarantine it
	// as if the integration had been revoked.
	retryable := makeDelivery("f4b2c3d4-e5f6-4789-8abc-def012345678", 6, 9406)
	if _, err := inbox.Record(ctx, retryable, now); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `ALTER TABLE github_credential_bindings RENAME TO unavailable_credential_bindings`); err != nil {
		t.Fatal(err)
	}
	if _, err := ProcessInstallationRepositoryDeliveryForIntegration(ctx, tx, retryable.IntegrationID, retryable.DeliveryID, now); err == nil {
		t.Fatal("integration lookup database failure was treated as revoked authorization")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var retryableStatus string
	if err := pool.QueryRow(ctx, `SELECT processing_status FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2`, retryable.IntegrationID, retryable.DeliveryID).Scan(&retryableStatus); err != nil {
		t.Fatal(err)
	}
	if retryableStatus != "received" {
		t.Fatalf("database failure quarantined retryable event: %s", retryableStatus)
	}
}
