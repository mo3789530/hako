package workloadjobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeScheduleQueue struct {
	deliveries []ScheduleDelivery
	deleted    []string
	visibility []time.Duration
	deleteErr  error
}

func (q *fakeScheduleQueue) Receive(_ context.Context, limit int, wait time.Duration) ([]ScheduleDelivery, error) {
	if limit != 10 || wait != 20*time.Second {
		return nil, errors.New("unexpected receive configuration")
	}
	return q.deliveries, nil
}

func (q *fakeScheduleQueue) Delete(_ context.Context, receipt string) error {
	if q.deleteErr != nil {
		return q.deleteErr
	}
	q.deleted = append(q.deleted, receipt)
	return nil
}

func (q *fakeScheduleQueue) ChangeVisibility(_ context.Context, receipt string, timeout time.Duration) error {
	q.visibility = append(q.visibility, timeout)
	return nil
}

func TestSchedulerConsumerDeletesOnlyAfterHandlerSucceeds(t *testing.T) {
	queue := &fakeScheduleQueue{deliveries: []ScheduleDelivery{
		{ReceiptHandle: "receipt-1", Body: []byte(`{"schema_version":1,"tenant_id":"tenant_1","workload_run_id":"run_1"}`)},
		{ReceiptHandle: "receipt-2", Body: []byte(`{"schema_version":1,"tenant_id":"tenant_1","workload_run_id":"run_2"}`)},
	}}
	var handled []ScheduleRequest
	handler := ScheduleHandlerFunc(func(_ context.Context, request ScheduleRequest) error {
		handled = append(handled, request)
		if request.WorkloadRunID == "run_2" {
			return errors.New("temporary database failure")
		}
		return nil
	})
	consumer, err := NewSchedulerConsumer(queue, handler, ConsumerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := consumer.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "temporary database failure") {
		t.Fatalf("expected retryable handler error, got %v", err)
	}
	if stats != (ConsumerStats{Received: 2, Handled: 1, Failed: 1}) || len(handled) != 2 || len(queue.deleted) != 1 || queue.deleted[0] != "receipt-1" || len(queue.visibility) != 2 {
		t.Fatalf("unexpected scheduler handling outcome: stats=%+v handled=%v deleted=%v visibility=%v", stats, handled, queue.deleted, queue.visibility)
	}
}

func TestSchedulerConsumerRenewsVisibilityDuringLongHandler(t *testing.T) {
	queue := &fakeScheduleQueue{deliveries: []ScheduleDelivery{{ReceiptHandle: "receipt-1", Body: []byte(`{"schema_version":1,"tenant_id":"tenant_1","workload_run_id":"run_1"}`)}}}
	handler := ScheduleHandlerFunc(func(context.Context, ScheduleRequest) error {
		time.Sleep(35 * time.Millisecond)
		return nil
	})
	consumer, err := NewSchedulerConsumer(queue, handler, ConsumerConfig{
		VisibilityTimeout: time.Second, VisibilityHeartbeat: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := consumer.RunOnce(context.Background())
	if err != nil || stats.Handled != 1 || len(queue.visibility) < 2 {
		t.Fatalf("long-running handler did not renew SQS visibility: stats=%+v renewals=%d error=%v", stats, len(queue.visibility), err)
	}
}

func TestSchedulerConsumerLeavesMalformedAndReceiptlessMessagesOnQueue(t *testing.T) {
	queue := &fakeScheduleQueue{deliveries: []ScheduleDelivery{
		{ReceiptHandle: "bad-json", Body: []byte(`{"schema_version":1,"tenant_id":"tenant_1","workload_run_id":"run_1"} {}`)},
		{ReceiptHandle: "unknown-field", Body: []byte(`{"schema_version":1,"tenant_id":"tenant_1","workload_run_id":"run_1","token":"secret"}`)},
		{Body: []byte(`{"schema_version":1,"tenant_id":"tenant_1","workload_run_id":"run_1"}`)},
	}}
	handler := ScheduleHandlerFunc(func(context.Context, ScheduleRequest) error {
		t.Fatal("malformed messages must not reach the handler")
		return nil
	})
	consumer, err := NewSchedulerConsumer(queue, handler, ConsumerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := consumer.RunOnce(context.Background())
	if err == nil || stats != (ConsumerStats{Received: 3, Failed: 3}) || len(queue.deleted) != 0 {
		t.Fatalf("malformed deliveries must be left for retry/DLQ: stats=%+v deleted=%v err=%v", stats, queue.deleted, err)
	}
}

func TestDecodeScheduleRequestRequiresVersionedTenantAndRun(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"schema_version":2,"tenant_id":"tenant_1","workload_run_id":"run_1"}`),
		[]byte(`{"schema_version":1,"workload_run_id":"run_1"}`),
		[]byte(`{"schema_version":1,"tenant_id":"tenant_1"}`),
	} {
		if _, err := decodeScheduleRequest(body); err == nil {
			t.Errorf("decodeScheduleRequest(%s) unexpectedly succeeded", body)
		}
	}
}
