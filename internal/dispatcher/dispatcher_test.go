package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/store/outbox"
)

type fakeStore struct {
	events     []outbox.Event
	published  []string
	retryTimes []time.Time
}

func (s *fakeStore) Claim(context.Context, int, time.Duration, time.Time) ([]outbox.Event, error) {
	return s.events, nil
}
func (*fakeStore) ReadPendingMetrics(context.Context, time.Time) (outbox.PendingMetrics, error) {
	return outbox.PendingMetrics{}, nil
}
func (s *fakeStore) MarkPublished(_ context.Context, event outbox.Event, _ time.Time) error {
	s.published = append(s.published, event.ID)
	return nil
}
func (s *fakeStore) ReleaseAfterFailure(_ context.Context, _ outbox.Event, at time.Time) error {
	s.retryTimes = append(s.retryTimes, at)
	return nil
}

type fakePublisher struct {
	queueURLs []string
	err       error
}

func (p *fakePublisher) Publish(_ context.Context, queueURL string, _ outbox.Event) error {
	p.queueURLs = append(p.queueURLs, queueURL)
	return p.err
}

func TestRunOnceRoutesAndAcknowledgesCommittedCommand(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{events: []outbox.Event{{
		ID: "evt_1", AggregateType: "operation", AggregateID: "op_1", EventType: "operation.requested", Attempt: 1,
		Payload: []byte(`{"schema_version":1,"operation_id":"op_1","resource_plane_id":"rp_tokyo"}`),
	}}}
	publisher := &fakePublisher{}
	dispatcher, err := New(store, publisher, map[string]string{"rp_tokyo": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/hako-rp-tokyo"}, Config{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := dispatcher.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run dispatcher: %v", err)
	}
	if stats != (Stats{Claimed: 1, Published: 1}) || len(store.published) != 1 || store.published[0] != "evt_1" || len(publisher.queueURLs) != 1 {
		t.Fatalf("unexpected dispatch outcome: stats=%+v published=%v queues=%v", stats, store.published, publisher.queueURLs)
	}
}

func TestRunOnceRoutesRevisionFencedSchemaV2WorkspaceCommand(t *testing.T) {
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{events: []outbox.Event{{
		ID: "evt_v2", AggregateType: "operation", AggregateID: "op_v2", EventType: "operation.requested", Attempt: 1,
		Payload: []byte(`{"schema_version":2,"workspace_revision":3,"operation_id":"op_v2","resource_plane_id":"rp_tokyo","correlation_id":"req_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`),
	}}}
	publisher := &fakePublisher{}
	dispatcher, err := New(store, publisher, map[string]string{"rp_tokyo": "queue-v2"}, Config{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := dispatcher.RunOnce(context.Background())
	if err != nil || stats != (Stats{Claimed: 1, Published: 1}) || len(publisher.queueURLs) != 1 || publisher.queueURLs[0] != "queue-v2" {
		t.Fatalf("Schema v2 operation was not routed: stats=%+v queues=%v err=%v", stats, publisher.queueURLs, err)
	}
}

func TestValidateCommandRejectsRevisionVersionMismatch(t *testing.T) {
	for _, payload := range []string{
		`{"schema_version":1,"workspace_revision":1,"operation_id":"op_1","resource_plane_id":"rp_1"}`,
		`{"schema_version":2,"operation_id":"op_1","resource_plane_id":"rp_1"}`,
		`{"schema_version":2,"workspace_revision":1,"operation_id":"op_1","resource_plane_id":"rp_1","correlation_id":"spoofed"}`,
	} {
		_, err := validateCommand(outbox.Event{AggregateType: "operation", AggregateID: "op_1", EventType: "operation.requested", Payload: []byte(payload)})
		if err == nil {
			t.Errorf("accepted mismatched command version: %s", payload)
		}
	}
}

func TestRunOnceReleasesFailedPublicationWithBoundedBackoff(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{events: []outbox.Event{{
		ID: "evt_1", AggregateType: "operation", AggregateID: "op_1", EventType: "operation.requested", Attempt: 3,
		Payload: []byte(`{"schema_version":1,"operation_id":"op_1","resource_plane_id":"rp_tokyo"}`),
	}}}
	publisher := &fakePublisher{err: errors.New("queue unavailable")}
	dispatcher, err := New(store, publisher, map[string]string{"rp_tokyo": "queue-url"}, Config{
		Now: func() time.Time { return now }, BaseBackoff: 2 * time.Second, MaxBackoff: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := dispatcher.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "queue unavailable") {
		t.Fatalf("expected queue failure, got %v", err)
	}
	if stats != (Stats{Claimed: 1, Failed: 1}) || len(store.retryTimes) != 1 || !store.retryTimes[0].Equal(now.Add(5*time.Second)) || len(store.published) != 0 {
		t.Fatalf("unexpected retry handling: stats=%+v retry=%v published=%v", stats, store.retryTimes, store.published)
	}
}

func TestRunOnceRejectsInvalidOrUnmappedCommand(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeStore{events: []outbox.Event{{
		ID: "evt_1", AggregateType: "operation", AggregateID: "op_1", EventType: "operation.requested", Attempt: 1,
		Payload: []byte(`{"schema_version":1,"operation_id":"op_1","resource_plane_id":"rp_missing"}`),
	}}}
	publisher := &fakePublisher{}
	dispatcher, err := New(store, publisher, map[string]string{"rp_tokyo": "queue-url"}, Config{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := dispatcher.RunOnce(context.Background())
	if err == nil || stats.Failed != 1 || len(publisher.queueURLs) != 0 || len(store.retryTimes) != 1 {
		t.Fatalf("unmapped Resource Plane command must be deferred: stats=%+v err=%v", stats, err)
	}
}

func TestRunOnceRoutesWorkloadScheduleRequestToDedicatedQueue(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{events: []outbox.Event{{
		ID: "evt_workload", AggregateType: "workload_run", AggregateID: "run_1", EventType: "workload_run.schedule_requested", Attempt: 1,
		Payload: []byte(`{"schema_version":1,"tenant_id":"tenant_1","workload_run_id":"run_1","correlation_id":"req_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`),
	}}}
	publisher := &fakePublisher{}
	dispatcher, err := New(store, publisher, map[string]string{"rp_tokyo": "rp-queue"}, Config{
		Now: func() time.Time { return now }, WorkloadSchedulerQueueURL: "workload-scheduler-queue",
	})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := dispatcher.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("dispatch workload schedule request: %v", err)
	}
	if stats != (Stats{Claimed: 1, Published: 1}) || len(publisher.queueURLs) != 1 || publisher.queueURLs[0] != "workload-scheduler-queue" {
		t.Fatalf("workload request was not routed to dedicated queue: stats=%+v queues=%v", stats, publisher.queueURLs)
	}
}

func TestRunOnceRoutesGitHubCheckRequestToDedicatedQueue(t *testing.T) {
	store := &fakeStore{events: []outbox.Event{{
		ID: "evt_check", AggregateType: "workload_run", AggregateID: "run_1", EventType: "workload_run.github_check_requested", Attempt: 1,
		Payload: []byte(`{"schema_version":1,"tenant_id":"tenant_1","workload_run_id":"run_1","revision":2}`),
	}}}
	publisher := &fakePublisher{}
	d, err := New(store, publisher, map[string]string{"rp_tokyo": "rp-queue"}, Config{GitHubChecksQueueURL: "checks-queue"})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := d.RunOnce(context.Background())
	if err != nil || stats != (Stats{Claimed: 1, Published: 1}) || len(publisher.queueURLs) != 1 || publisher.queueURLs[0] != "checks-queue" {
		t.Fatalf("GitHub Check request was not routed to dedicated queue: stats=%+v queues=%v err=%v", stats, publisher.queueURLs, err)
	}
}

func TestRunOnceRoutesActionsRunnerCheckRequestToDedicatedQueue(t *testing.T) {
	store := &fakeStore{events: []outbox.Event{{
		ID: "evt_runner_check", AggregateType: "github_actions_runner_job", AggregateID: "integration_1:42:2",
		EventType: "github_actions_runner_job.github_check_requested", Attempt: 1,
		Payload: []byte(`{"schema_version":1,"integration_id":"integration_1","workflow_job_id":42,"run_attempt":2,"revision":3}`),
	}}}
	publisher := &fakePublisher{}
	d, err := New(store, publisher, map[string]string{"rp_tokyo": "rp-queue"}, Config{GitHubChecksQueueURL: "checks-queue"})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := d.RunOnce(context.Background())
	if err != nil || stats != (Stats{Claimed: 1, Published: 1}) || len(publisher.queueURLs) != 1 || publisher.queueURLs[0] != "checks-queue" {
		t.Fatalf("Actions Runner Check request was not routed to dedicated queue: stats=%+v queues=%v err=%v", stats, publisher.queueURLs, err)
	}
}

func TestRunOnceRejectsInvalidGitHubCheckEnvelope(t *testing.T) {
	store := &fakeStore{events: []outbox.Event{{
		ID: "evt_check", AggregateType: "workload_run", AggregateID: "run_1", EventType: "workload_run.github_check_requested", Attempt: 1,
		Payload: []byte(`{"schema_version":1,"tenant_id":"tenant_1","workload_run_id":"run_other","revision":1}`),
	}}}
	publisher := &fakePublisher{}
	d, err := New(store, publisher, map[string]string{"rp_tokyo": "rp-queue"}, Config{GitHubChecksQueueURL: "checks-queue"})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := d.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "inconsistent envelope") || stats != (Stats{Claimed: 1, Failed: 1}) || len(publisher.queueURLs) != 0 {
		t.Fatalf("invalid GitHub Check event was accepted: stats=%+v queues=%v err=%v", stats, publisher.queueURLs, err)
	}
}

func TestRunOnceRoutesWorkloadDispatchToPinnedResourcePlane(t *testing.T) {
	store := &fakeStore{events: []outbox.Event{{
		ID: "evt_attempt", AggregateType: "workload_attempt", AggregateID: "attempt_1", EventType: "workload_attempt.dispatch", Attempt: 1,
		Payload: []byte(`{"schema_version":1,"tenant_id":"tenant_1","workload_run_id":"run_1","attempt_id":"attempt_1","generation":1,"resource_plane_id":"rp_tokyo","idempotency_token":"attempt-fixed"}`),
	}}}
	publisher := &fakePublisher{}
	dispatcher, err := New(store, publisher, map[string]string{"rp_tokyo": "rp-queue"}, Config{WorkloadCommandQueueURLs: map[string]string{"rp_tokyo": "rp-workload-queue"}})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := dispatcher.RunOnce(context.Background())
	if err != nil || stats != (Stats{Claimed: 1, Published: 1}) || len(publisher.queueURLs) != 1 || publisher.queueURLs[0] != "rp-workload-queue" {
		t.Fatalf("Workload dispatch was not routed to its pinned Resource Plane: stats=%+v queues=%v err=%v", stats, publisher.queueURLs, err)
	}
}

func TestRunOnceRoutesWorkloadCancellationToPinnedResourcePlane(t *testing.T) {
	store := &fakeStore{events: []outbox.Event{{ID: "evt_cancel", AggregateType: "workload_attempt", AggregateID: "attempt_1", EventType: "workload_attempt.cancel", Attempt: 1,
		Payload: []byte(`{"schema_version":1,"tenant_id":"tenant_1","workload_run_id":"run_1","attempt_id":"attempt_1","generation":1,"resource_plane_id":"rp_tokyo","idempotency_token":"attempt-fixed"}`)}}}
	publisher := &fakePublisher{}
	d, err := New(store, publisher, map[string]string{"rp_tokyo": "rp-queue"}, Config{WorkloadCommandQueueURLs: map[string]string{"rp_tokyo": "rp-workload-queue"}})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := d.RunOnce(context.Background())
	if err != nil || stats.Published != 1 || len(publisher.queueURLs) != 1 || publisher.queueURLs[0] != "rp-workload-queue" {
		t.Fatal("cancel command was not routed", stats, err)
	}
}

func TestRunOnceRoutesWorkspaceServiceToDedicatedPinnedPlaneQueue(t *testing.T) {
	store := &fakeStore{events: []outbox.Event{{
		ID: "evt_service", AggregateType: "workspace_service", AggregateID: "svc_1",
		EventType: "workspace_service.reconcile_requested", Attempt: 1,
		Payload: []byte(`{"schema_version":1,"tenant_id":"tenant_1","workspace_id":"ws_1","resource_plane_id":"rp_tokyo","service_id":"svc_1","revision":4,"desired_state":"running","catalog_version":"postgres/1","config":{"image":"postgres:18"}}`),
	}}}
	publisher := &fakePublisher{}
	d, err := New(store, publisher, map[string]string{"rp_tokyo": "workspace-queue"}, Config{
		WorkspaceServiceQueueURLs: map[string]string{"rp_tokyo": "service-queue"},
	})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := d.RunOnce(context.Background())
	if err != nil || stats != (Stats{Claimed: 1, Published: 1}) || len(publisher.queueURLs) != 1 || publisher.queueURLs[0] != "service-queue" {
		t.Fatalf("Workspace Service command was not sent to its dedicated queue: stats=%+v queues=%v err=%v", stats, publisher.queueURLs, err)
	}
}

func TestRunOnceDefersWorkspaceServiceWhenDedicatedQueueIsNotConfigured(t *testing.T) {
	store := &fakeStore{events: []outbox.Event{{
		ID: "evt_service", AggregateType: "workspace_service", AggregateID: "svc_1",
		EventType: "workspace_service.reconcile_requested", Attempt: 1,
		Payload: []byte(`{"schema_version":1,"tenant_id":"tenant_1","workspace_id":"ws_1","resource_plane_id":"rp_tokyo","service_id":"svc_1","revision":1,"desired_state":"running","catalog_version":"postgres/1","config":{"image":"postgres:18"}}`),
	}}}
	publisher := &fakePublisher{}
	d, err := New(store, publisher, map[string]string{"rp_tokyo": "workspace-queue"}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := d.RunOnce(context.Background())
	if err == nil || stats != (Stats{Claimed: 1, Failed: 1}) || len(store.retryTimes) != 1 || len(store.published) != 0 || len(publisher.queueURLs) != 0 {
		t.Fatalf("unconfigured Service queue must leave the Outbox event pending: stats=%+v err=%v", stats, err)
	}
}

func TestRunOnceRejectsInvalidWorkspaceServiceEnvelope(t *testing.T) {
	store := &fakeStore{events: []outbox.Event{{
		ID: "evt_service", AggregateType: "workspace_service", AggregateID: "svc_1",
		EventType: "workspace_service.reconcile_requested", Attempt: 1,
		Payload: []byte(`{"schema_version":1,"tenant_id":"tenant_1","workspace_id":"ws_1","resource_plane_id":"rp_tokyo","service_id":"svc_other","revision":0,"desired_state":"running","catalog_version":"postgres/1","config":{"image":"postgres:18"}}`),
	}}}
	publisher := &fakePublisher{}
	d, err := New(store, publisher, map[string]string{"rp_tokyo": "workspace-queue"}, Config{
		WorkspaceServiceQueueURLs: map[string]string{"rp_tokyo": "service-queue"},
	})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := d.RunOnce(context.Background())
	if err == nil || stats != (Stats{Claimed: 1, Failed: 1}) || len(store.retryTimes) != 1 || len(store.published) != 0 || len(publisher.queueURLs) != 0 {
		t.Fatalf("invalid Workspace Service command must remain unpublished: stats=%+v err=%v", stats, err)
	}
}

func TestRunOnceDoesNotFallbackToWorkspaceCommandQueueForWorkload(t *testing.T) {
	store := &fakeStore{events: []outbox.Event{{ID: "evt_attempt", AggregateType: "workload_attempt", AggregateID: "attempt_1", EventType: "workload_attempt.dispatch", Attempt: 1,
		Payload: []byte(`{"schema_version":1,"tenant_id":"tenant_1","workload_run_id":"run_1","attempt_id":"attempt_1","generation":1,"resource_plane_id":"rp_tokyo","idempotency_token":"attempt-fixed"}`)}}}
	publisher := &fakePublisher{}
	d, err := New(store, publisher, map[string]string{"rp_tokyo": "workspace-commands"}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := d.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no Workload command queue configured") || stats != (Stats{Claimed: 1, Failed: 1}) {
		t.Fatalf("missing dedicated Workload queue should remain retryable: stats=%+v err=%v", stats, err)
	}
	if len(publisher.queueURLs) != 0 || len(store.published) != 0 || len(store.retryTimes) != 1 {
		t.Fatalf("Workload command fell back to Workspace queue: queues=%v published=%v retries=%v", publisher.queueURLs, store.published, store.retryTimes)
	}
}

func TestRunOnceRejectsInconsistentWorkloadDispatch(t *testing.T) {
	store := &fakeStore{events: []outbox.Event{{
		ID: "evt_attempt", AggregateType: "workload_attempt", AggregateID: "attempt_1", EventType: "workload_attempt.dispatch", Attempt: 1,
		Payload: []byte(`{"schema_version":1,"tenant_id":"tenant_1","workload_run_id":"run_1","attempt_id":"attempt_other","generation":1,"resource_plane_id":"rp_tokyo","idempotency_token":"attempt-fixed"}`),
	}}}
	publisher := &fakePublisher{}
	dispatcher, err := New(store, publisher, map[string]string{"rp_tokyo": "rp-queue"}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := dispatcher.RunOnce(context.Background())
	if err == nil || stats != (Stats{Claimed: 1, Failed: 1}) || len(publisher.queueURLs) != 0 {
		t.Fatalf("inconsistent Workload dispatch was accepted: stats=%+v queues=%v err=%v", stats, publisher.queueURLs, err)
	}
}

func TestRunOnceDefersWorkloadRequestWithoutSchedulerQueue(t *testing.T) {
	store := &fakeStore{events: []outbox.Event{{
		ID: "evt_workload", AggregateType: "workload_run", AggregateID: "run_1", EventType: "workload_run.schedule_requested", Attempt: 1,
		Payload: []byte(`{"schema_version":1,"tenant_id":"tenant_1","workload_run_id":"run_1"}`),
	}}}
	publisher := &fakePublisher{}
	dispatcher, err := New(store, publisher, map[string]string{"rp_tokyo": "rp-queue"}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := dispatcher.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Workload scheduler queue is not configured") || stats != (Stats{Claimed: 1, Failed: 1}) {
		t.Fatalf("missing scheduler queue should leave event retryable: stats=%+v err=%v", stats, err)
	}
	if len(publisher.queueURLs) != 0 || len(store.published) != 0 || len(store.retryTimes) != 1 {
		t.Fatalf("unexpected missing-queue handling: queues=%v published=%v retries=%v", publisher.queueURLs, store.published, store.retryTimes)
	}
}

func TestRunOnceRejectsInconsistentWorkloadScheduleEnvelope(t *testing.T) {
	store := &fakeStore{events: []outbox.Event{{
		ID: "evt_workload", AggregateType: "workload_run", AggregateID: "run_1", EventType: "workload_run.schedule_requested", Attempt: 1,
		Payload: []byte(`{"schema_version":1,"tenant_id":"tenant_1","workload_run_id":"run_other"}`),
	}}}
	publisher := &fakePublisher{}
	dispatcher, err := New(store, publisher, map[string]string{"rp_tokyo": "rp-queue"}, Config{WorkloadSchedulerQueueURL: "workload-scheduler-queue"})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := dispatcher.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "inconsistent envelope") || stats != (Stats{Claimed: 1, Failed: 1}) {
		t.Fatalf("inconsistent workload envelope must be rejected: stats=%+v err=%v", stats, err)
	}
	if len(publisher.queueURLs) != 0 || len(store.published) != 0 || len(store.retryTimes) != 1 {
		t.Fatalf("unexpected invalid-envelope handling: queues=%v published=%v retries=%v", publisher.queueURLs, store.published, store.retryTimes)
	}
}
