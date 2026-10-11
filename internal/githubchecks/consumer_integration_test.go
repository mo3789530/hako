//go:build integration

package githubchecks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/dispatcher"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/outbox"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/testutil"
)

type outboxQueuePublisher struct{ queue *fakeQueue }

func (p outboxQueuePublisher) Publish(_ context.Context, _ string, event outbox.Event) error {
	p.queue.deliveries = append(p.queue.deliveries, Delivery{Body: append([]byte(nil), event.Payload...), ReceiptHandle: event.ID})
	return nil
}

type failFirstSaveStore struct {
	delegate Store
	failed   bool
}

func (s *failFirstSaveStore) Load(ctx context.Context, tenant domain.TenantID, run domain.WorkloadRunID) (DesiredCheck, error) {
	return s.delegate.Load(ctx, tenant, run)
}

func (s *failFirstSaveStore) SaveID(ctx context.Context, tenant domain.TenantID, run domain.WorkloadRunID, sha string, id int64, now time.Time) error {
	if !s.failed {
		s.failed = true
		return errors.New("simulated database failure after remote Check create")
	}
	return s.delegate.SaveID(ctx, tenant, run, sha, id, now)
}

func TestPostgresOutboxDispatcherConsumerPersistsAndRecoversCheckDelivery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, statement := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_check_e2e','Checks E2E',$1)`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_check_e2e','checks-e2e','',$1)`,
		`INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_check_e2e',7811,'checks-owner','active','usr_check_e2e',$1,$1)`,
		`INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at) VALUES (7811,'tenant_check_e2e',$1)`,
		`INSERT INTO tenant_github_repositories (tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at) VALUES ('tenant_check_e2e',7811,7812,'checks-owner','checks-repo','main',$1)`,
	} {
		if _, err := pool.Exec(ctx, statement, now); err != nil {
			t.Fatal(err)
		}
	}
	run := domain.WorkloadRun{ID: "run_check_e2e", TenantID: "tenant_check_e2e", Kind: domain.WorkloadJob,
		RequestedBy: "usr_check_e2e", GitHubInstallation: 7811, GitHubRepository: 7812, CommitSHA: strings.Repeat("c", 40),
		RuntimeClass: "standard", TrustLevel: domain.WorkloadTrusted, IdempotencyKey: "checks-e2e-run", CreatedAt: now, TimeoutAt: now.Add(time.Minute)}
	created, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		stored, _, err := workloads.CreateOrGet(ctx, tx, run)
		return stored, err
	})
	if err != nil {
		t.Fatal(err)
	}
	current := created
	for index, next := range []domain.WorkloadRunState{domain.WorkloadProvisioning, domain.WorkloadRunning, domain.WorkloadSucceeded} {
		current, err = transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
			return workloads.Transition(ctx, tx, workloads.TransitionInput{TenantID: current.TenantID, RunID: current.ID,
				ExpectedRevision: current.Revision, NextState: next, OccurredAt: now.Add(time.Duration(index+1) * time.Second)})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Leave only the latest desired Check revision for the dispatcher. The
	// preceding generic Run events and earlier Check revisions are not relevant
	// to this delivery/recovery scenario.
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET published_at=$1 WHERE event_type <> 'workload_run.github_check_requested'
		OR (event_type = 'workload_run.github_check_requested' AND payload_json::jsonb->>'revision' <> '4')`, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	queue := &fakeQueue{}
	d, err := dispatcher.New(dispatcher.StoreAdapter{Pool: pool, Policy: transaction.DefaultPolicy()}, outboxQueuePublisher{queue: queue}, map[string]string{"rp-test": "https://sqs.invalid/rp"},
		dispatcher.Config{GitHubChecksQueueURL: "https://sqs.invalid/checks", Now: func() time.Time { return now.Add(4 * time.Second) }})
	if err != nil {
		t.Fatal(err)
	}
	dispatchStats, err := d.RunOnce(ctx)
	if err != nil || dispatchStats != (dispatcher.Stats{Claimed: 1, Published: 1}) || len(queue.deliveries) != 1 {
		t.Fatalf("dispatch latest Check event: stats=%+v queued=%d err=%v", dispatchStats, len(queue.deliveries), err)
	}
	store := &failFirstSaveStore{delegate: DBStore{Pool: pool, Policy: transaction.DefaultPolicy()}}
	credentials := &fakeCredentials{token: fakeCheckToken{value: "synthetic-check-e2e-token", expiresAt: now.Add(time.Hour)},
		repository: githubapp.RepositoryAccess{Owner: "checks-owner", Name: "checks-repo"}}
	remote := map[int64]githubapp.CheckRun{}
	api, err := githubapp.NewChecksClient(&http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-check-e2e-token" || !strings.Contains(r.URL.Path, "/repos/checks-owner/checks-repo/check-runs") {
			t.Errorf("unexpected GitHub Check destination or credential: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			var matching []githubapp.CheckRun
			for _, check := range remote {
				if check.HeadSHA == r.URL.Query().Get("head_sha") {
					matching = append(matching, check)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": matching})
		case http.MethodPost, http.MethodPatch:
			var input githubapp.CheckRunRequest
			if json.NewDecoder(r.Body).Decode(&input) != nil || input.HeadSHA != strings.Repeat("c", 40) || input.Status != githubapp.CheckCompleted || input.Conclusion != githubapp.CheckRunConclusion("success") {
				t.Error("GitHub Check request did not contain the exact terminal Run result")
			}
			id := int64(987)
			if r.Method == http.MethodPatch {
				parsed, _ := url.PathUnescape(r.URL.Path)
				if !strings.HasSuffix(parsed, "/987") {
					t.Errorf("redelivery did not update the recovered Check ID: %s", parsed)
				}
			}
			result := githubapp.CheckRun{ID: id, Name: input.Name, ExternalID: input.ExternalID, HeadSHA: input.HeadSHA, Status: input.Status, Conclusion: input.Conclusion}
			remote[id] = result
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusCreated)
			}
			_ = json.NewEncoder(w).Encode(result)
		default:
			t.Errorf("unexpected GitHub API method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})}})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := New(queue, store, credentials, api, Config{BatchSize: 1, Now: func() time.Time { return now.Add(5 * time.Second) }})
	if err != nil {
		t.Fatal(err)
	}
	first, firstErr := consumer.RunOnce(ctx)
	if firstErr == nil || first != (Stats{Received: 1, Failed: 1}) || len(queue.deleted) != 0 || len(remote) != 1 {
		t.Fatalf("DB failure after remote Check create must remain retryable: stats=%+v err=%v deleted=%v remote=%d", first, firstErr, queue.deleted, len(remote))
	}
	second, secondErr := consumer.RunOnce(ctx)
	if secondErr != nil || second != (Stats{Received: 1, Applied: 1}) || len(queue.deleted) != 1 || len(remote) != 1 {
		t.Fatalf("redelivery should recover and persist the original Check: stats=%+v err=%v deleted=%v remote=%d", second, secondErr, queue.deleted, len(remote))
	}
	var persisted int64
	var status, conclusion string
	if err := pool.QueryRow(ctx, `SELECT check_run_id,status,conclusion FROM workload_github_checks WHERE tenant_id=$1 AND workload_run_id=$2`, run.TenantID, run.ID).
		Scan(&persisted, &status, &conclusion); err != nil || persisted != 987 || status != "completed" || conclusion != "success" {
		t.Fatalf("persisted GitHub Check = ID %d status %q conclusion %q err=%v", persisted, status, conclusion, err)
	}
}
