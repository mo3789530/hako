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
	"github.com/mo3789530/hako/internal/store/githubwebhook"
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

func (s *failFirstSaveStore) Load(ctx context.Context, tenant domain.TenantID, run domain.WorkloadRunID, pipelineName string) (DesiredCheck, error) {
	return s.delegate.Load(ctx, tenant, run, pipelineName)
}

func (s *failFirstSaveStore) SaveID(ctx context.Context, tenant domain.TenantID, run domain.WorkloadRunID, pipelineName, sha string, id int64, now time.Time) error {
	if !s.failed {
		s.failed = true
		return errors.New("simulated database failure after remote Check create")
	}
	return s.delegate.SaveID(ctx, tenant, run, pipelineName, sha, id, now)
}

func (s *failFirstSaveStore) LoadActionsRunner(ctx context.Context, integrationID string, workflowJobID, runAttempt int64) (githubwebhook.ActionsRunnerCheck, error) {
	return s.delegate.LoadActionsRunner(ctx, integrationID, workflowJobID, runAttempt)
}

func (s *failFirstSaveStore) SaveActionsRunnerID(ctx context.Context, check githubwebhook.ActionsRunnerCheck, id int64, now time.Time) error {
	return s.delegate.SaveActionsRunnerID(ctx, check, id, now)
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
	annotation := domain.WorkloadCheckAnnotation{Path: "internal/api/handler.go", StartLine: 42, EndLine: 42,
		AnnotationLevel: "warning", Message: "Review this API boundary."}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		return struct{}{}, workloads.SyncGitHubPipelineCheck(ctx, tx, current, domain.WorkloadPipelineResult{
			Name: "verify", Status: "succeeded", Annotations: []domain.WorkloadCheckAnnotation{annotation},
		}, now.Add(4*time.Second))
	}); err != nil {
		t.Fatal(err)
	}
	// Leave only the named pipeline Check event for the dispatcher. The
	// preceding generic Run events and base Check revisions are not relevant
	// to this delivery/recovery scenario.
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET published_at=$1 WHERE event_type <> 'workload_run.github_check_requested'
		OR payload_json::jsonb->>'pipeline_name' IS DISTINCT FROM 'verify'`, now.Add(5*time.Second)); err != nil {
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
			if input.Name != "hako/job/verify" || len(input.Output.Annotations) != 1 || input.Output.Annotations[0].Path != annotation.Path ||
				input.Output.Annotations[0].StartLine != annotation.StartLine || input.Output.Annotations[0].Message != annotation.Message {
				t.Errorf("GitHub Check did not receive the durable pipeline annotation: %+v", input.Output.Annotations)
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
	if err := pool.QueryRow(ctx, `SELECT check_run_id,status,conclusion FROM workload_github_checks WHERE tenant_id=$1 AND workload_run_id=$2 AND pipeline_name='verify'`, run.TenantID, run.ID).
		Scan(&persisted, &status, &conclusion); err != nil || persisted != 987 || status != "completed" || conclusion != "success" {
		t.Fatalf("persisted GitHub Check = ID %d status %q conclusion %q err=%v", persisted, status, conclusion, err)
	}
}

func TestPostgresActionsRunnerCheckOutboxToGitHubEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id,name,created_at) VALUES ('tenant_runner_check_e2e','Runner Check E2E',$1)`, now); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("d", 40)
	if _, err := pool.Exec(ctx, `INSERT INTO github_actions_runner_jobs
		(integration_id,delivery_id,tenant_id,installation_id,repository_id,workflow_job_id,workflow_run_id,run_attempt,workflow_name,job_name,head_branch,head_sha,labels,state,created_at,updated_at)
		VALUES ('integration_runner_e2e','delivery_runner_e2e','tenant_runner_check_e2e',7813,7814,7815,7816,1,'CI','test','main',$1,'["self-hosted","hako"]','completed',$2,$2)`, sha, now); err != nil {
		t.Fatal(err)
	}
	externalID := "hako-actions-runner/integration_runner_e2e/7815/1"
	correlationID := "req_87654321-4321-4321-8321-cba987654321"
	if _, err := pool.Exec(ctx, `INSERT INTO github_actions_runner_checks
		(integration_id,workflow_job_id,run_attempt,tenant_id,installation_id,repository_id,workflow_run_id,commit_sha,check_name,external_id,status,conclusion,started_at,completed_at,output_title,output_summary,revision,updated_at,correlation_id)
		VALUES ('integration_runner_e2e',7815,1,'tenant_runner_check_e2e',7813,7814,7816,$1,'Hako Actions Runner',$2,'completed','success',$3,$3,'Hako Actions Runner','GitHub reported this workflow job completed with conclusion: success.',3,$3,$4)`, sha, externalID, now, correlationID); err != nil {
		t.Fatal(err)
	}
	payload := `{"schema_version":1,"integration_id":"integration_runner_e2e","workflow_job_id":7815,"run_attempt":1,"correlation_id":"` + correlationID + `","revision":3}`
	if _, err := pool.Exec(ctx, `INSERT INTO outbox_events (id,aggregate_type,aggregate_id,event_type,payload_json,created_at,published_at,attempt)
		VALUES ('evt_runner_check_e2e','github_actions_runner_job','integration_runner_e2e:7815:1','github_actions_runner_job.github_check_requested',$1,$2,NULL,0)`, payload, now); err != nil {
		t.Fatal(err)
	}
	queue := &fakeQueue{}
	d, err := dispatcher.New(dispatcher.StoreAdapter{Pool: pool, Policy: transaction.DefaultPolicy()}, outboxQueuePublisher{queue: queue},
		map[string]string{"rp-test": "https://sqs.invalid/rp"}, dispatcher.Config{GitHubChecksQueueURL: "https://sqs.invalid/checks", Now: func() time.Time { return now.Add(time.Second) }})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := d.RunOnce(ctx)
	if err != nil || stats != (dispatcher.Stats{Claimed: 1, Published: 1}) || len(queue.deliveries) != 1 {
		t.Fatalf("dispatch runner Check outbox: stats=%+v deliveries=%d err=%v", stats, len(queue.deliveries), err)
	}
	credentials := &fakeCredentials{token: fakeCheckToken{value: "synthetic-runner-check-token", expiresAt: now.Add(time.Hour)},
		repository: githubapp.RepositoryAccess{Owner: "runner-owner", Name: "runner-repo"}}
	var created githubapp.CheckRunRequest
	api, err := githubapp.NewChecksClient(&http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-runner-check-token" || !strings.Contains(r.URL.Path, "/repos/runner-owner/runner-repo/check-runs") {
			t.Errorf("unexpected Check API destination or credential: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"check_runs":[]}`))
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected GitHub API method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
			t.Errorf("decode Check request: %v", err)
		}
		if created.ExternalID != externalID || created.HeadSHA != sha || created.Status != githubapp.CheckCompleted || created.Conclusion != "success" {
			t.Errorf("published Check does not match terminal workflow job: %+v", created)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(githubapp.CheckRun{ID: 9815, Name: created.Name, ExternalID: created.ExternalID,
			HeadSHA: created.HeadSHA, Status: created.Status, Conclusion: created.Conclusion})
	})}})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := New(queue, DBStore{Pool: pool, Policy: transaction.DefaultPolicy()}, credentials, api,
		Config{BatchSize: 1, Now: func() time.Time { return now.Add(2 * time.Second) }})
	if err != nil {
		t.Fatal(err)
	}
	result, err := consumer.RunOnce(ctx)
	if err != nil || result != (Stats{Received: 1, Applied: 1}) || len(queue.deleted) != 1 {
		t.Fatalf("deliver runner Check: stats=%+v deleted=%v err=%v", result, queue.deleted, err)
	}
	var checkRunID int64
	var status, conclusion, storedCorrelationID string
	if err := pool.QueryRow(ctx, `SELECT check_run_id,status,conclusion,COALESCE(correlation_id,'') FROM github_actions_runner_checks
		WHERE integration_id='integration_runner_e2e' AND workflow_job_id=7815 AND run_attempt=1`).Scan(&checkRunID, &status, &conclusion, &storedCorrelationID); err != nil {
		t.Fatal(err)
	}
	if checkRunID != 9815 || status != "completed" || conclusion != "success" || storedCorrelationID != correlationID {
		t.Fatalf("durable runner Check = (%d,%s,%s,%s), want (9815,completed,success,%s)", checkRunID, status, conclusion, storedCorrelationID, correlationID)
	}
}
