package githubchecks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/workloads"
)

type fakeQueue struct {
	deliveries []Delivery
	deleted    []string
	changed    []struct {
		receipt string
		delay   time.Duration
	}
	changeErr error
}

func (q *fakeQueue) Receive(context.Context, int, time.Duration) ([]Delivery, error) {
	return q.deliveries, nil
}
func (q *fakeQueue) Delete(_ context.Context, receipt string) error {
	q.deleted = append(q.deleted, receipt)
	return nil
}
func (q *fakeQueue) ChangeVisibility(_ context.Context, receipt string, delay time.Duration) error {
	if q.changeErr != nil {
		return q.changeErr
	}
	q.changed = append(q.changed, struct {
		receipt string
		delay   time.Duration
	}{receipt, delay})
	return nil
}

type fakeStore struct {
	desired       DesiredCheck
	saveErr       error
	savedID       int64
	saveCalls     int
	loadTenant    domain.TenantID
	loadRun       domain.WorkloadRunID
	loadPipeline  string
	savedPipeline string
	runnerCheck   githubwebhook.ActionsRunnerCheck
	runnerCheckID int64
}

func (s *fakeStore) Load(_ context.Context, tenant domain.TenantID, run domain.WorkloadRunID, pipelineName string) (DesiredCheck, error) {
	s.loadTenant, s.loadRun, s.loadPipeline = tenant, run, pipelineName
	return s.desired, nil
}
func (s *fakeStore) SaveID(_ context.Context, tenant domain.TenantID, run domain.WorkloadRunID, pipelineName, sha string, id int64, _ time.Time) error {
	s.saveCalls++
	if s.saveErr != nil {
		return s.saveErr
	}
	if tenant != s.desired.Check.TenantID || run != s.desired.Check.WorkloadRunID || pipelineName != s.desired.Check.PipelineName || sha != s.desired.Check.CommitSHA {
		return errors.New("identity mismatch")
	}
	s.savedID = id
	s.savedPipeline = pipelineName
	s.desired.Check.CheckRunID = id
	return nil
}

type fakeCredentials struct {
	token       fakeCheckToken
	repository  githubapp.RepositoryAccess
	err         error
	run         domain.WorkloadRun
	calls       int
	runnerCalls int
}

type fakeCheckToken struct {
	value     string
	expiresAt time.Time
}

func (t fakeCheckToken) Value() string        { return t.value }
func (t fakeCheckToken) ExpiresAt() time.Time { return t.expiresAt }

func (p *fakeCredentials) ChecksRunToken(_ context.Context, run domain.WorkloadRun) (githubapp.CheckToken, githubapp.RepositoryAccess, error) {
	p.calls++
	p.run = run
	return p.token, p.repository, p.err
}
func (p *fakeCredentials) ActionsRunnerChecksToken(context.Context, string, int64, int64) (githubapp.CheckToken, githubapp.RepositoryAccess, error) {
	p.runnerCalls++
	return p.token, p.repository, p.err
}

func (s *fakeStore) LoadActionsRunner(_ context.Context, integrationID string, jobID, attempt int64) (githubwebhook.ActionsRunnerCheck, error) {
	if s.runnerCheck.IntegrationID != integrationID || s.runnerCheck.WorkflowJobID != jobID || s.runnerCheck.RunAttempt != attempt {
		return githubwebhook.ActionsRunnerCheck{}, errors.New("Actions Runner Check identity mismatch")
	}
	return s.runnerCheck, nil
}

func (s *fakeStore) SaveActionsRunnerID(_ context.Context, check githubwebhook.ActionsRunnerCheck, id int64, _ time.Time) error {
	if check.IntegrationID != s.runnerCheck.IntegrationID || check.WorkflowJobID != s.runnerCheck.WorkflowJobID || check.RunAttempt != s.runnerCheck.RunAttempt {
		return errors.New("Actions Runner Check identity mismatch")
	}
	s.runnerCheckID = id
	s.runnerCheck.CheckRunID = id
	return nil
}

type handlerTransport struct{ handler http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, r)
	return recorder.Result(), nil
}

func deliveryBody(t *testing.T, tenant string, run string, revision int64) []byte {
	t.Helper()
	body, err := json.Marshal(request{SchemaVersion: 1, TenantID: tenant, WorkloadRunID: run,
		CorrelationID: "req_12121212-1212-4212-8212-121212121212", Revision: revision})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func actionsRunnerDeliveryBody(t *testing.T, integration string, jobID, attempt, revision int64) []byte {
	t.Helper()
	body, err := json.Marshal(request{SchemaVersion: 1, IntegrationID: integration, WorkflowJobID: jobID, RunAttempt: attempt, Revision: revision})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestConsumerPublishesDurableActionsRunnerTerminalCheck(t *testing.T) {
	now := time.Now().UTC()
	completedAt := now
	check := githubwebhook.ActionsRunnerCheck{IntegrationID: "integration_runner", TenantID: "tenant_runner", Installation: 77, Repository: 88,
		WorkflowJobID: 420, WorkflowRunID: 430, RunAttempt: 2, CommitSHA: strings.Repeat("a", 40), Name: "Hako Actions Runner",
		ExternalID: "hako-actions-runner/integration_runner/420/2", Status: "completed", Conclusion: "failure", StartedAt: now.Add(-time.Minute),
		CompletedAt: &completedAt, OutputTitle: "Hako Actions Runner", OutputSummary: "GitHub reported this workflow job completed with conclusion: failure.", Revision: 3}
	store := &fakeStore{runnerCheck: check}
	queue := &fakeQueue{deliveries: []Delivery{{Body: actionsRunnerDeliveryBody(t, check.IntegrationID, check.WorkflowJobID, check.RunAttempt, check.Revision), ReceiptHandle: "runner-check"}}}
	creds := &fakeCredentials{token: fakeCheckToken{value: "synthetic-runner-check-token", expiresAt: time.Now().Add(time.Hour)}, repository: githubapp.RepositoryAccess{Owner: "acme", Name: "api"}}
	var requests int
	consumer, _ := newTestConsumer(t, store, queue, creds, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-runner-check-token" || !strings.Contains(r.URL.Path, "/repos/acme/api/check-runs") {
			t.Errorf("Runner Check used wrong token or repository: %s %s", r.Method, r.URL)
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"check_runs":[]}`))
			return
		}
		var body githubapp.CheckRunRequest
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.ExternalID != check.ExternalID || body.HeadSHA != check.CommitSHA ||
			body.Status != githubapp.CheckCompleted || body.Conclusion != "failure" || body.CompletedAt == nil {
			t.Errorf("unexpected exact-run Check request: %+v", body)
		}
		requests++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(githubapp.CheckRun{ID: 987, Name: body.Name, ExternalID: body.ExternalID, HeadSHA: body.HeadSHA,
			Status: body.Status, Conclusion: body.Conclusion})
	})
	stats, err := consumer.RunOnce(context.Background())
	if err != nil || stats != (Stats{Received: 1, Applied: 1}) || requests != 1 || creds.runnerCalls != 1 || store.runnerCheckID != 987 || len(queue.deleted) != 1 {
		t.Fatalf("durable Actions Runner Check not applied: stats=%+v err=%v requests=%d credentials=%d saved=%d deleted=%v", stats, err, requests, creds.runnerCalls, store.runnerCheckID, queue.deleted)
	}
}

func testDesiredCheck(now time.Time, id int64) DesiredCheck {
	started := now.Add(-time.Minute)
	check := workloads.GitHubCheck{TenantID: "tenant_check", WorkloadRunID: "run_check", GitHubInstallationID: 77,
		GitHubRepositoryID: 88, CommitSHA: strings.Repeat("a", 40), Name: "hako/job", ExternalID: "run_check",
		CheckRunID: id, Status: "completed", Conclusion: "failure", StartedAt: started,
		OutputTitle: "Hako job", OutputSummary: "Hako Workload Run is failed.", Revision: 2}
	completed := now
	check.CompletedAt = &completed
	run := domain.WorkloadRun{ID: "run_check", TenantID: "tenant_check", Kind: domain.WorkloadJob,
		GitHubInstallation: 77, GitHubRepository: 88, CommitSHA: strings.Repeat("a", 40),
		CorrelationID: "req_12121212-1212-4212-8212-121212121212",
		State:         domain.WorkloadFailed, GitHubIntegrationID: "integration_check", GitHubConfigRevision: 4,
		GitHubCredentialGeneration: 5}
	return DesiredCheck{Check: check, Run: run}
}

func newTestConsumer(t *testing.T, store *fakeStore, queue *fakeQueue, creds CredentialProvider, gh http.HandlerFunc) (*Consumer, *time.Time) {
	t.Helper()
	api, err := githubapp.NewChecksClient(&http.Client{Transport: handlerTransport{handler: gh}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	consumer, err := New(queue, store, creds, api, Config{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return consumer, &now
}

func TestConsumerPublishesLatestDurableStateAndPersistsCheckID(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{desired: testDesiredCheck(now, 0)}
	queue := &fakeQueue{deliveries: []Delivery{{Body: deliveryBody(t, "tenant_check", "run_check", 1), ReceiptHandle: "receipt-1"}}}
	creds := &fakeCredentials{token: fakeCheckToken{value: "synthetic-check-token", expiresAt: time.Now().Add(time.Hour)}, repository: githubapp.RepositoryAccess{Owner: "acme", Name: "api"}}
	var created int
	consumer, _ := newTestConsumer(t, store, queue, creds, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-check-token" {
			t.Error("Checks request did not use the purpose-specific token")
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"check_runs":[]}`))
			return
		}
		var body githubapp.CheckRunRequest
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.HeadSHA != strings.Repeat("a", 40) || body.Conclusion != "failure" || body.Status != githubapp.CheckCompleted {
			t.Error("consumer did not publish the exact durable state")
		}
		created++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(githubapp.CheckRun{ID: 456, Name: body.Name, ExternalID: body.ExternalID, HeadSHA: body.HeadSHA, Status: body.Status, Conclusion: body.Conclusion})
	})
	stats, err := consumer.RunOnce(context.Background())
	if err != nil || stats != (Stats{Received: 1, Applied: 1}) || len(queue.deleted) != 1 || queue.deleted[0] != "receipt-1" {
		t.Fatalf("Check delivery = %+v err=%v deleted=%v", stats, err, queue.deleted)
	}
	if created != 1 || store.savedID != 456 || creds.calls != 1 || creds.run.GitHubConfigRevision != 4 ||
		store.loadTenant != "tenant_check" || store.loadRun != "run_check" {
		t.Fatalf("durable Check identity was not respected: creates=%d saved=%d provider=%+v store=%+v", created, store.savedID, creds, store)
	}
}

func TestConsumerPublishesNamedPipelineCheckIndependently(t *testing.T) {
	now := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	desired := testDesiredCheck(now, 0)
	desired.Check.PipelineName = "verify"
	desired.Check.Name = "hako/job/verify"
	desired.Check.Conclusion = "success"
	desired.Check.OutputTitle = "Hako pipeline verify"
	desired.Check.OutputSummary = "Pipeline verify completed with status succeeded and exit code 0."
	desired.Check.Annotations = []domain.WorkloadCheckAnnotation{{Path: "internal/api/handler.go", StartLine: 42,
		EndLine: 42, AnnotationLevel: "warning", Message: "Review this API boundary."}}
	desired.Run.State = domain.WorkloadSucceeded
	store := &fakeStore{desired: desired}
	body, err := json.Marshal(request{SchemaVersion: 1, TenantID: string(desired.Run.TenantID),
		WorkloadRunID: string(desired.Run.ID), PipelineName: "verify", CorrelationID: desired.Run.CorrelationID, Revision: desired.Check.Revision})
	if err != nil {
		t.Fatal(err)
	}
	queue := &fakeQueue{deliveries: []Delivery{{Body: body, ReceiptHandle: "pipeline-check"}}}
	creds := &fakeCredentials{token: fakeCheckToken{value: "synthetic-check-token", expiresAt: time.Now().Add(time.Hour)},
		repository: githubapp.RepositoryAccess{Owner: "acme", Name: "api"}}
	var requestName string
	consumer, _ := newTestConsumer(t, store, queue, creds, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"check_runs":[]}`))
			return
		}
		var check githubapp.CheckRunRequest
		if err := json.NewDecoder(r.Body).Decode(&check); err != nil {
			t.Errorf("decode pipeline Check: %v", err)
		}
		if len(check.Output.Annotations) != 1 || check.Output.Annotations[0].Path != "internal/api/handler.go" ||
			check.Output.Annotations[0].StartLine != 42 || check.Output.Annotations[0].Message != "Review this API boundary." {
			t.Errorf("pipeline Check annotations were not delivered: %+v", check.Output.Annotations)
		}
		requestName = check.Name
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(githubapp.CheckRun{ID: 919, Name: check.Name, ExternalID: check.ExternalID,
			HeadSHA: check.HeadSHA, Status: check.Status, Conclusion: check.Conclusion})
	})
	stats, err := consumer.RunOnce(context.Background())
	if err != nil || stats != (Stats{Received: 1, Applied: 1}) || requestName != "hako/job/verify" ||
		store.loadPipeline != "verify" || store.savedPipeline != "verify" || store.savedID != 919 {
		t.Fatalf("pipeline Check was not delivered independently: stats=%+v name=%q store=%+v err=%v", stats, requestName, store, err)
	}
}

func TestConsumerLeavesMessageForRetryWhenIDPersistenceFails(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{desired: testDesiredCheck(now, 0), saveErr: errors.New("database unavailable")}
	queue := &fakeQueue{deliveries: []Delivery{{Body: deliveryBody(t, "tenant_check", "run_check", 2), ReceiptHandle: "receipt-retry"}}}
	creds := &fakeCredentials{token: fakeCheckToken{value: "synthetic-check-token", expiresAt: time.Now().Add(time.Hour)}, repository: githubapp.RepositoryAccess{Owner: "acme", Name: "api"}}
	var creates, updates, lookups int
	consumer, _ := newTestConsumer(t, store, queue, creds, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			lookups++
			if lookups == 1 {
				_, _ = w.Write([]byte(`{"check_runs":[]}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []githubapp.CheckRun{{ID: 789, Name: "hako/job", ExternalID: "run_check", HeadSHA: strings.Repeat("a", 40), Status: githubapp.CheckCompleted, Conclusion: "failure"}}})
			return
		}
		if r.Method == http.MethodPost {
			creates++
		} else if r.Method == http.MethodPatch {
			updates++
		} else {
			t.Errorf("unexpected Check request method %s", r.Method)
		}
		var body githubapp.CheckRunRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(githubapp.CheckRun{ID: 789, Name: body.Name, ExternalID: body.ExternalID, HeadSHA: body.HeadSHA, Status: body.Status, Conclusion: body.Conclusion})
	})
	stats, err := consumer.RunOnce(context.Background())
	if err == nil || stats != (Stats{Received: 1, Failed: 1}) || len(queue.deleted) != 0 || store.saveCalls != 1 {
		t.Fatalf("failed ID persistence must leave delivery retryable: stats=%+v err=%v deleted=%v", stats, err, queue.deleted)
	}
	// The remote create may have succeeded before DSQL failed. On redelivery the
	// API client finds the same external ID and updates that Check, not a new one.
	store.saveErr = nil
	queue.deliveries = []Delivery{{Body: deliveryBody(t, "tenant_check", "run_check", 2), ReceiptHandle: "receipt-retry-2"}}
	stats, err = consumer.RunOnce(context.Background())
	if err != nil || stats != (Stats{Received: 1, Applied: 1}) || len(queue.deleted) != 1 || creates != 1 || updates != 1 || lookups != 2 || store.savedID != 789 {
		t.Fatalf("Check retry did not recover original remote result: stats=%+v err=%v creates=%d updates=%d lookups=%d saved=%d", stats, err, creates, updates, lookups, store.savedID)
	}
}

func TestConsumerRejectsCrossTenantAndFutureRevisionBeforeCredentialLookup(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{desired: testDesiredCheck(now, 0)}
	queue := &fakeQueue{deliveries: []Delivery{{Body: deliveryBody(t, "tenant_other", "run_check", 1), ReceiptHandle: "receipt"}}}
	creds := &fakeCredentials{}
	consumer, _ := newTestConsumer(t, store, queue, creds, func(http.ResponseWriter, *http.Request) { t.Error("unauthorized Check reached GitHub") })
	stats, err := consumer.RunOnce(context.Background())
	if err == nil || stats.Failed != 1 || creds.calls != 0 || len(queue.deleted) != 0 {
		t.Fatalf("cross-Tenant Check was not rejected: stats=%+v err=%v", stats, err)
	}
	queue.deliveries = []Delivery{{Body: deliveryBody(t, "tenant_check", "run_check", 3), ReceiptHandle: "receipt-future"}}
	stats, err = consumer.RunOnce(context.Background())
	if err == nil || stats.Failed != 1 || creds.calls != 0 || len(queue.deleted) != 0 {
		t.Fatalf("future Check revision was not rejected: stats=%+v err=%v", stats, err)
	}
}

func TestConsumerLeavesRateLimitedCheckDeliveryForSQSRetry(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	store := &fakeStore{desired: testDesiredCheck(now, 0)}
	queue := &fakeQueue{deliveries: []Delivery{{Body: deliveryBody(t, "tenant_check", "run_check", 2), ReceiptHandle: "receipt-rate-limit"}}}
	creds := &fakeCredentials{token: fakeCheckToken{value: "synthetic-check-token", expiresAt: now.Add(time.Hour)}, repository: githubapp.RepositoryAccess{Owner: "acme", Name: "api"}}
	var reject bool = true
	consumer, _ := newTestConsumer(t, store, queue, creds, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"check_runs":[]}`))
			return
		}
		if reject {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("upstream details must not leak"))
			return
		}
		var body githubapp.CheckRunRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(githubapp.CheckRun{ID: 612, Name: body.Name, ExternalID: body.ExternalID, HeadSHA: body.HeadSHA, Status: body.Status, Conclusion: body.Conclusion})
	})
	stats, err := consumer.RunOnce(context.Background())
	if err == nil || stats != (Stats{Received: 1, Failed: 1, Deferred: 1}) || len(queue.deleted) != 0 || len(queue.changed) != 1 || queue.changed[0].receipt != "receipt-rate-limit" || queue.changed[0].delay < 30*time.Second || queue.changed[0].delay > 31*time.Second || !strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "upstream details") {
		t.Fatalf("rate-limited Check should remain queued with a safe error: stats=%+v err=%v deleted=%v", stats, err, queue.deleted)
	}
	reject = false
	queue.deliveries = []Delivery{{Body: deliveryBody(t, "tenant_check", "run_check", 2), ReceiptHandle: "receipt-retry"}}
	stats, err = consumer.RunOnce(context.Background())
	if err != nil || stats != (Stats{Received: 1, Applied: 1}) || len(queue.deleted) != 1 || store.savedID != 612 {
		t.Fatalf("rate-limited Check retry failed: stats=%+v err=%v deleted=%v ID=%d", stats, err, queue.deleted, store.savedID)
	}
	reject = true
	queue.changeErr = errors.New("synthetic SQS failure")
	queue.deliveries = []Delivery{{Body: deliveryBody(t, "tenant_check", "run_check", 2), ReceiptHandle: "receipt-delay-failure"}}
	stats, err = consumer.RunOnce(context.Background())
	if err == nil || stats != (Stats{Received: 1, Failed: 1}) || len(queue.deleted) != 1 || !strings.Contains(err.Error(), "defer GitHub Check delivery") {
		t.Fatalf("failed visibility update acknowledged or hid failure: stats=%+v err=%v deleted=%v", stats, err, queue.deleted)
	}
}

func TestRetryVisibilityBoundsOnlyRetryableSafeHints(t *testing.T) {
	now := time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		err     error
		want    time.Duration
		deferIt bool
	}{
		{name: "bounded retry after", err: &githubapp.CredentialError{Retryable: true, RetryAt: now.Add(30 * time.Second)}, want: 30 * time.Second, deferIt: true},
		{name: "round up to avoid early retry", err: &githubapp.CredentialError{Retryable: true, RetryAt: now.Add(1100 * time.Millisecond)}, want: 2 * time.Second, deferIt: true},
		{name: "minimum delay", err: &githubapp.CredentialError{Retryable: true, RetryAt: now.Add(-time.Second)}, want: time.Second, deferIt: true},
		{name: "maximum delay", err: &githubapp.CredentialError{Retryable: true, RetryAt: now.Add(24 * time.Hour)}, want: 12 * time.Hour, deferIt: true},
		{name: "nonretryable", err: &githubapp.CredentialError{Retryable: false, RetryAt: now.Add(time.Minute)}},
		{name: "no safe retry time", err: &githubapp.CredentialError{Retryable: true}},
		{name: "untyped error", err: errors.New("temporary error")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := retryVisibility(test.err, now)
			if ok != test.deferIt || got != test.want {
				t.Fatalf("retry visibility = %s,%v; want %s,%v", got, ok, test.want, test.deferIt)
			}
		})
	}
}
