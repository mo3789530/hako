package operationresults

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/resourcecontroller"
)

type testStore struct {
	applied bool
	err     error
	results []resourcecontroller.Result
}

func (s *testStore) Apply(_ context.Context, result resourcecontroller.Result) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	s.results = append(s.results, result)
	return s.applied, nil
}

type testQueue struct {
	deliveries []Delivery
	deleted    []string
}

func (q *testQueue) Receive(context.Context, int, time.Duration) ([]Delivery, error) {
	return q.deliveries, nil
}

func (q *testQueue) Delete(_ context.Context, receipt string) error {
	q.deleted = append(q.deleted, receipt)
	return nil
}

func TestConsumerAcknowledgesOnlyAfterStoreCommit(t *testing.T) {
	store := &testStore{err: errors.New("database unavailable")}
	queue := &testQueue{deliveries: []Delivery{{Body: resultJSON(t), ReceiptHandle: "receipt-1"}}}
	consumer, err := New(store, queue, Config{WaitTime: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := consumer.RunOnce(context.Background())
	if err == nil || stats.Failed != 1 || len(queue.deleted) != 0 {
		t.Fatalf("failed DB commit must leave result for redelivery: stats=%+v deleted=%v err=%v", stats, queue.deleted, err)
	}

	store.err = nil
	store.applied = true
	stats, err = consumer.RunOnce(context.Background())
	if err != nil || stats.Applied != 1 || len(queue.deleted) != 1 {
		t.Fatalf("committed result should be acknowledged: stats=%+v deleted=%v err=%v", stats, queue.deleted, err)
	}
}

func TestConsumerAcknowledgesDuplicateTerminalResult(t *testing.T) {
	store := &testStore{applied: false}
	queue := &testQueue{deliveries: []Delivery{{Body: resultJSON(t), ReceiptHandle: "receipt-1"}}}
	consumer, err := New(store, queue, Config{WaitTime: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := consumer.RunOnce(context.Background())
	if err != nil || stats.Duplicate != 1 || len(queue.deleted) != 1 {
		t.Fatalf("duplicate result should be acknowledged as no-op: stats=%+v deleted=%v err=%v", stats, queue.deleted, err)
	}
}

func resultJSON(t *testing.T) []byte {
	t.Helper()
	encoded, err := json.Marshal(resourcecontroller.Result{
		SchemaVersion: 1, OperationID: "op_1", TenantID: "tenant_1", WorkspaceID: "ws_1",
		ResourcePlaneID: "rp_1", Type: domain.OperationEnsureRunning, Status: domain.OperationSucceeded,
		ObservedState: domain.ObservedWorkspaceRunning, CompletedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
