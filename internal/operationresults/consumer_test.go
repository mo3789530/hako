package operationresults

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
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

func TestConsumerFailureLogIncludesCorrelationAndOmitsRawError(t *testing.T) {
	store := &testStore{err: errors.New("database password detail")}
	queue := &testQueue{deliveries: []Delivery{{Body: resultJSON(t), ReceiptHandle: "receipt-1"}}}
	consumer, err := New(store, queue, Config{WaitTime: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	consumer.logger = slog.New(slog.NewTextHandler(&logs, nil))
	if _, err := consumer.RunOnce(context.Background()); err == nil {
		t.Fatal("failed result should remain retryable")
	}
	logged := logs.String()
	if !strings.Contains(logged, "correlation_id=req_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa") ||
		!strings.Contains(logged, "failure_class=apply_result") || strings.Contains(logged, "database password detail") {
		t.Fatalf("unsafe or incomplete structured failure log: %s", logged)
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

func TestDecodeRejectsUnsupportedOrInconsistentResult(t *testing.T) {
	valid := resourcecontroller.Result{
		SchemaVersion: 1, OperationID: "op_1", TenantID: "tenant_1", WorkspaceID: "ws_1",
		ResourcePlaneID: "rp_1", CorrelationID: "req_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Type: domain.OperationEnsureRunning, Status: domain.OperationSucceeded,
		ObservedState: domain.ObservedWorkspaceRunning, CompletedAt: time.Now().UTC(),
	}

	versioned := valid
	versioned.SchemaVersion = 2
	versioned.WorkspaceRevision = 7
	encodedVersioned, err := json.Marshal(versioned)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := decode(encodedVersioned); err != nil || decoded.WorkspaceRevision != 7 {
		t.Fatalf("valid versioned result should be accepted: %+v error=%v", decoded, err)
	}
	tests := []struct {
		name   string
		mutate func(*resourcecontroller.Result)
	}{
		{name: "invalid correlation id", mutate: func(result *resourcecontroller.Result) { result.CorrelationID = "client-supplied" }},
		{name: "unsupported type", mutate: func(result *resourcecontroller.Result) { result.Type = "unknown" }},
		{name: "wrong successful state", mutate: func(result *resourcecontroller.Result) { result.ObservedState = domain.ObservedWorkspaceSuspended }},
		{name: "failed without error code", mutate: func(result *resourcecontroller.Result) {
			result.Status = domain.OperationFailed
			result.ObservedState = domain.ObservedWorkspaceFailed
		}},
		{name: "failed with non-failed observed state", mutate: func(result *resourcecontroller.Result) {
			result.Status = domain.OperationFailed
			result.ErrorCode = "runtime_error"
		}},
		{name: "version 2 missing revision", mutate: func(result *resourcecontroller.Result) {
			result.SchemaVersion = 2
		}},
		{name: "version 1 unexpectedly sets revision", mutate: func(result *resourcecontroller.Result) {
			result.WorkspaceRevision = 1
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := valid
			test.mutate(&result)
			payload, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decode(payload); err == nil {
				t.Fatal("expected malformed or inconsistent result to be rejected")
			}
		})
	}
}

func resultJSON(t *testing.T) []byte {
	t.Helper()
	encoded, err := json.Marshal(resourcecontroller.Result{
		SchemaVersion: 1, OperationID: "op_1", TenantID: "tenant_1", WorkspaceID: "ws_1",
		ResourcePlaneID: "rp_1", CorrelationID: "req_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Type: domain.OperationEnsureRunning, Status: domain.OperationSucceeded,
		ObservedState: domain.ObservedWorkspaceRunning, CompletedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
