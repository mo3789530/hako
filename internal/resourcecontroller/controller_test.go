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
	t.Helper()
	command := Command{
		SchemaVersion: 1, OperationID: "op_1", TenantID: "tenant_1", WorkspaceID: "ws_1",
		ResourcePlaneID: resourcePlaneID, Type: domain.OperationEnsureRunning, CreatedAt: time.Now().UTC(),
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
	controller, err := New("rp_1", runtime, sink, func() time.Time { return time.Unix(100, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	command := encodedCommand(t, "rp_1")
	if err := controller.Handle(context.Background(), command); err != nil {
		t.Fatalf("handle command: %v", err)
	}
	if err := controller.Handle(context.Background(), command); err != nil {
		t.Fatalf("handle duplicate command: %v", err)
	}
	if runtime.calls != 1 || len(sink.results) != 2 {
		t.Fatalf("duplicate delivery should be applied once but report may be repeated: calls=%d results=%d", runtime.calls, len(sink.results))
	}
	if sink.results[0].OperationID != sink.results[1].OperationID || sink.results[0].ObservedState != domain.ObservedWorkspaceRunning || sink.results[0].Status != domain.OperationSucceeded {
		t.Fatalf("unexpected reported result: %+v %+v", sink.results[0], sink.results[1])
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
