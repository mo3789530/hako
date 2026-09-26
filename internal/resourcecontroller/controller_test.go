package resourcecontroller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

type testRuntime struct {
	calls   int
	results map[domain.OperationID]domain.ObservedWorkspaceState
}

type blockingRuntime struct{}

func (blockingRuntime) Execute(ctx context.Context, _ Command) (domain.ObservedWorkspaceState, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

type delayedRuntime struct{ delay time.Duration }

func (r delayedRuntime) Execute(ctx context.Context, _ Command) (domain.ObservedWorkspaceState, error) {
	timer := time.NewTimer(r.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
		return domain.ObservedWorkspaceRunning, nil
	}
}

func (r *testRuntime) Execute(_ context.Context, command Command) (domain.ObservedWorkspaceState, error) {
	if r.results == nil {
		r.results = make(map[domain.OperationID]domain.ObservedWorkspaceState)
	}
	if prior, exists := r.results[command.OperationID]; exists {
		return prior, nil
	}
	r.calls++
	var state domain.ObservedWorkspaceState
	if command.Type == domain.OperationDelete {
		state = domain.ObservedWorkspaceDeleted
	} else {
		state = domain.ObservedWorkspaceRunning
	}
	r.results[command.OperationID] = state
	return state, nil
}

type testSink struct {
	results []Result
	err     error
}

func (s *testSink) Report(_ context.Context, result Result) error {
	if s.err != nil {
		return s.err
	}
	s.results = append(s.results, result)
	return nil
}

func encodedCommand(t *testing.T, resourcePlaneID domain.ResourcePlaneID) []byte {
	return encodedCommandAt(t, resourcePlaneID, time.Now().UTC())
}

func encodedCommandAt(t *testing.T, resourcePlaneID domain.ResourcePlaneID, createdAt time.Time) []byte {
	t.Helper()
	command := Command{
		SchemaVersion: 2, WorkspaceRevision: 1, OperationID: "op_1", TenantID: "tenant_1", WorkspaceID: "ws_1",
		ResourcePlaneID: resourcePlaneID, Type: domain.OperationEnsureRunning, CreatedAt: createdAt,
	}
	encoded, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestControllerHandlesDuplicateAndReportsResultAtLeastOnce(t *testing.T) {
	runtime := &testRuntime{}
	sink := &testSink{}
	now := time.Now().UTC()
	controller, err := New("rp_1", runtime, sink, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	command := encodedCommandAt(t, "rp_1", now)
	if err := controller.Handle(context.Background(), command); err != nil {
		t.Fatalf("handle command: %v", err)
	}
	if err := controller.Handle(context.Background(), command); err != nil {
		t.Fatalf("handle duplicate command: %v", err)
	}
	if runtime.calls != 1 || len(sink.results) != 2 {
		t.Fatalf("duplicate delivery should be applied once but report may be repeated: calls=%d results=%d", runtime.calls, len(sink.results))
	}
	if sink.results[0].OperationID != sink.results[1].OperationID || sink.results[0].WorkspaceRevision != 1 || sink.results[0].ObservedState != domain.ObservedWorkspaceRunning || sink.results[0].Status != domain.OperationSucceeded {
		t.Fatalf("unexpected reported result: %+v %+v", sink.results[0], sink.results[1])
	}
}

func TestControllerRejectsExpiredCommandsWithoutExecutingRuntime(t *testing.T) {
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	runtime := &testRuntime{}
	sink := &testSink{}
	controller, err := NewWithExecutionTimeoutAndMaxCommandAge("rp_1", runtime, sink, func() time.Time { return now }, time.Minute, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	command := encodedCommandAt(t, "rp_1", now.Add(-10*time.Minute-time.Second))
	if err := controller.Handle(context.Background(), command); err != nil {
		t.Fatalf("expired command should be reported and acknowledged: %v", err)
	}
	if runtime.calls != 0 {
		t.Fatalf("expired command reached Runtime %d times", runtime.calls)
	}
	if len(sink.results) != 1 || sink.results[0].Status != domain.OperationFailed || sink.results[0].ErrorCode != "operation_expired" || sink.results[0].ObservedState != domain.ObservedWorkspaceFailed {
		t.Fatalf("expired command result = %+v", sink.results)
	}
}

func TestControllerRejectsFarFutureCommands(t *testing.T) {
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	runtime := &testRuntime{}
	sink := &testSink{}
	controller, err := New("rp_1", runtime, sink, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Handle(context.Background(), encodedCommandAt(t, "rp_1", now.Add(5*time.Minute+time.Second))); err == nil {
		t.Fatal("far-future command should be rejected for queue redrive/DLQ")
	}
	if runtime.calls != 0 || len(sink.results) != 0 {
		t.Fatalf("far-future command was applied: runtime=%d results=%d", runtime.calls, len(sink.results))
	}
}

func TestControllerRejectsWrongResourcePlaneAndInvalidEnvelope(t *testing.T) {
	runtime := &testRuntime{}
	sink := &testSink{}
	controller, err := New("rp_1", runtime, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Handle(context.Background(), encodedCommand(t, "rp_other")); err == nil {
		t.Fatal("expected command for another Resource Plane to be rejected")
	}
	if err := controller.Handle(context.Background(), []byte(`{"schema_version":2}`)); err == nil {
		t.Fatal("expected incomplete command to be rejected")
	}
	if runtime.calls != 0 || len(sink.results) != 0 {
		t.Fatalf("invalid commands must not reach Runtime or result sink: runtime=%d results=%d", runtime.calls, len(sink.results))
	}
}

func TestControllerRequiresRevisionForVersionTwoCommandsAndPreservesLegacyVersionOne(t *testing.T) {
	now := time.Now().UTC()
	runtime := &testRuntime{}
	sink := &testSink{}
	controller, err := New("rp_1", runtime, sink, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	missingRevision, err := json.Marshal(Command{
		SchemaVersion: 2, OperationID: "op_missing_revision", TenantID: "tenant_1", WorkspaceID: "ws_1",
		ResourcePlaneID: "rp_1", Type: domain.OperationEnsureRunning, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Handle(context.Background(), missingRevision); err == nil {
		t.Fatal("schema version 2 command without a revision should be rejected")
	}
	legacy, err := json.Marshal(Command{
		SchemaVersion: 1, OperationID: "op_legacy", TenantID: "tenant_1", WorkspaceID: "ws_legacy",
		ResourcePlaneID: "rp_1", Type: domain.OperationEnsureRunning, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Handle(context.Background(), legacy); err != nil {
		t.Fatalf("legacy schema version 1 command should remain readable during rollout: %v", err)
	}
	if len(sink.results) != 1 || sink.results[0].SchemaVersion != 1 || sink.results[0].WorkspaceRevision != 0 {
		t.Fatalf("legacy result should preserve version and unversioned semantics: %+v", sink.results)
	}
}

func TestControllerReportsExecutionTimeout(t *testing.T) {
	sink := &testSink{}
	controller, err := NewWithExecutionTimeout("rp_1", blockingRuntime{}, sink, nil, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Handle(context.Background(), encodedCommand(t, "rp_1")); err != nil {
		t.Fatal(err)
	}
	if len(sink.results) != 1 || sink.results[0].Status != domain.OperationFailed || sink.results[0].ErrorCode != "operation_timeout" {
		t.Fatalf("expected timeout result, got %+v", sink.results)
	}
}

func TestWorkerDoesNotDeleteUntilResultIsReported(t *testing.T) {
	sink := &testSink{err: errors.New("result queue unavailable")}
	runtime := &testRuntime{}
	controller, err := New("rp_1", runtime, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	queue := &testQueue{deliveries: []Delivery{{Body: encodedCommand(t, "rp_1"), ReceiptHandle: "receipt_1"}}}
	worker, err := NewWorker(queue, controller, WorkerConfig{WaitTime: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := worker.RunOnce(context.Background())
	if err == nil || stats.Failed != 1 || len(queue.deleted) != 0 {
		t.Fatalf("failed result publication must leave command visible: stats=%+v err=%v deleted=%v", stats, err, queue.deleted)
	}
	sink.err = nil
	stats, err = worker.RunOnce(context.Background())
	if err != nil || stats.Handled != 1 || len(queue.deleted) != 1 {
		t.Fatalf("successful result publication should delete command: stats=%+v err=%v deleted=%v", stats, err, queue.deleted)
	}
}

func TestWorkerExtendsVisibilityWhileCommandRuns(t *testing.T) {
	sink := &testSink{}
	controller, err := New("rp_1", delayedRuntime{delay: 700 * time.Millisecond}, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	queue := &testQueue{deliveries: []Delivery{{Body: encodedCommand(t, "rp_1"), ReceiptHandle: "receipt_1"}}}
	worker, err := NewWorker(queue, controller, WorkerConfig{WaitTime: time.Second, VisibilityTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := worker.RunOnce(context.Background())
	if err != nil || stats.Handled != 1 || queue.visibilityChanges == 0 {
		t.Fatalf("long-running command should renew visibility: stats=%+v changes=%d err=%v", stats, queue.visibilityChanges, err)
	}
}

func TestWorkerRetryBackoffIsExponentialAndBounded(t *testing.T) {
	worker, err := NewWorker(&testQueue{}, &Controller{}, WorkerConfig{
		WaitTime: time.Second, RetryInitialBackoff: 2 * time.Second, RetryMaxBackoff: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		receive int
		want    time.Duration
	}{{1, 2 * time.Second}, {2, 4 * time.Second}, {3, 5 * time.Second}, {9, 5 * time.Second}} {
		if got := worker.retryBackoff(test.receive); got != test.want {
			t.Errorf("receive count %d: got %s, want %s", test.receive, got, test.want)
		}
	}
}

type testQueue struct {
	deliveries        []Delivery
	deleted           []string
	visibilityChanges int
}

func (q *testQueue) Receive(context.Context, int, time.Duration, time.Duration) ([]Delivery, error) {
	return q.deliveries, nil
}
func (q *testQueue) ChangeVisibility(context.Context, string, time.Duration) error {
	q.visibilityChanges++
	return nil
}
func (q *testQueue) Delete(_ context.Context, receipt string) error {
	q.deleted = append(q.deleted, receipt)
	return nil
}
