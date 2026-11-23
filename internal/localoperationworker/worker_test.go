package localoperationworker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/resourcecontroller"
	"github.com/mo3789530/hako/internal/store/outbox"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func testClient(server *httptest.Server) *http.Client {
	base, _ := url.Parse(server.URL)
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		clone := request.Clone(request.Context())
		clone.URL.Scheme = base.Scheme
		clone.URL.Host = base.Host
		clone.Host = base.Host
		return http.DefaultTransport.RoundTrip(clone)
	})}
}

type fakeStore struct {
	events    []outbox.Event
	published []outbox.Event
	released  []outbox.Event
	retryAt   time.Time
}

func (s *fakeStore) ClaimWorkspaceOperations(context.Context, int, time.Duration, time.Time) ([]outbox.Event, error) {
	return s.events, nil
}
func (s *fakeStore) MarkPublished(_ context.Context, e outbox.Event, _ time.Time) error {
	s.published = append(s.published, e)
	return nil
}
func (s *fakeStore) ReleaseAfterFailure(_ context.Context, e outbox.Event, at time.Time) error {
	s.released = append(s.released, e)
	s.retryAt = at
	return nil
}

type fakeResultStore struct {
	results []resourcecontroller.Result
	err     error
}

func (s *fakeResultStore) Apply(_ context.Context, result resourcecontroller.Result) (bool, error) {
	s.results = append(s.results, result)
	return true, s.err
}

func TestRunOnceExecutesThenAppliesAndAcknowledges(t *testing.T) {
	result := `{"schema_version":2,"workspace_revision":3,"operation_id":"op-1","tenant_id":"ten-1","workspace_id":"wsp-1","resource_plane_id":"rp-local","type":"ensure_running","status":"succeeded","observed_state":"running","resource_kind":"cloud_hypervisor_local","external_resource_id":"allocation-1","allocation_generation":1,"completed_at":"2026-11-22T00:00:00Z"}`
	command := `{"schema_version":2,"workspace_revision":3,"operation_id":"op-1","tenant_id":"ten-1","workspace_id":"wsp-1","resource_plane_id":"rp-local","type":"ensure_running","created_at":"2026-11-22T00:00:00Z"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/operations" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected Controller request: %s %s %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(result))
	}))
	defer server.Close()
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{events: []outbox.Event{{ID: "evt-1", AggregateType: "operation", AggregateID: "op-1", EventType: "operation.requested", Attempt: 1, Payload: []byte(command)}}}
	results := &fakeResultStore{}
	worker, err := New(store, results, testClient(server), Config{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Claimed != 1 || stats.Applied != 1 || stats.Failed != 0 || len(results.results) != 1 || len(store.published) != 1 || len(store.released) != 0 {
		t.Fatalf("unexpected worker result: stats=%+v results=%d published=%d released=%d", stats, len(results.results), len(store.published), len(store.released))
	}
}

func TestRunOnceReleasesWhenResultApplicationFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"schema_version":2,"workspace_revision":3,"operation_id":"op-1","tenant_id":"ten-1","workspace_id":"wsp-1","resource_plane_id":"rp-local","type":"ensure_running","status":"succeeded","observed_state":"running","resource_kind":"cloud_hypervisor_local","external_resource_id":"allocation-1","allocation_generation":1,"completed_at":"2026-11-22T00:00:00Z"}`))
	}))
	defer server.Close()
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{events: []outbox.Event{{ID: "evt-1", AggregateType: "operation", EventType: "operation.requested", Attempt: 3, Payload: []byte(`{"schema_version":2,"workspace_revision":3,"operation_id":"op-1","tenant_id":"ten-1","workspace_id":"wsp-1","resource_plane_id":"rp-local","type":"ensure_running","created_at":"2026-11-22T00:00:00Z"}`)}}}
	worker, err := New(store, &fakeResultStore{err: errors.New("database unavailable")}, testClient(server), Config{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := worker.RunOnce(context.Background())
	if err == nil || stats.Failed != 1 || len(store.published) != 0 || len(store.released) != 1 || !store.retryAt.Equal(now.Add(4*time.Second)) {
		t.Fatalf("failed result application must release with bounded backoff: stats=%+v err=%v store=%+v", stats, err, store)
	}
}

func TestRunOnceReleasesUnsupportedOutboxEvent(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	store := &fakeStore{events: []outbox.Event{{ID: "evt-1", AggregateType: "workload", EventType: "workload_run.schedule_requested", Attempt: 1}}}
	worker, err := New(store, &fakeResultStore{}, testClient(server), Config{})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := worker.RunOnce(context.Background())
	if err == nil || stats.Failed != 1 || len(store.released) != 1 || len(store.published) != 0 {
		t.Fatalf("unsupported event must stay retryable: stats=%+v err=%v store=%+v", stats, err, store)
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	if _, err := New(&fakeStore{}, &fakeResultStore{}, testClient(server), Config{BatchSize: 101}); err == nil {
		t.Fatal("expected invalid batch size to be rejected")
	}
}
