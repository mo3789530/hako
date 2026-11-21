package workloadjobs

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
)

type fakeResultQueue struct {
	deliveries []ResultDelivery
	deleted    []string
	deleteErr  error
	limit      int
}

func (q *fakeResultQueue) Receive(_ context.Context, limit int, _ time.Duration) ([]ResultDelivery, error) {
	q.limit = limit
	if len(q.deliveries) > limit {
		return q.deliveries[:limit], nil
	}
	return q.deliveries, nil
}

func (q *fakeResultQueue) Delete(_ context.Context, receipt string) error {
	if q.deleteErr != nil {
		return q.deleteErr
	}
	q.deleted = append(q.deleted, receipt)
	return nil
}

type fakeResultApplier struct {
	outcomes []ResultOutcome
	err      error
	results  []WorkloadResult
}

func (a *fakeResultApplier) Handle(_ context.Context, result WorkloadResult) (ResultOutcome, error) {
	a.results = append(a.results, result)
	if a.err != nil {
		return "", a.err
	}
	outcome := a.outcomes[0]
	a.outcomes = a.outcomes[1:]
	return outcome, nil
}

func TestWorkloadResultConsumerDeletesOnlyDurableOutcomes(t *testing.T) {
	body, err := json.Marshal(WorkloadResult{SchemaVersion: 1, TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1",
		Generation: 1, ResourcePlaneID: "rp_1", CommitSHA: strings.Repeat("a", 40), Status: domain.WorkloadSucceeded,
		CompletedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	queue := &fakeResultQueue{deliveries: []ResultDelivery{
		{ReceiptHandle: "applied", Body: body},
		{ReceiptHandle: "duplicate", Body: body},
		{ReceiptHandle: "stale", Body: body},
		{ReceiptHandle: "transient", Body: body},
	}}
	applier := &fakeResultApplier{outcomes: []ResultOutcome{ResultApplied, ResultDuplicate, ResultStale}, err: nil}
	applierFn := &failingThirdApplier{applier: applier}
	consumer, err := NewWorkloadResultConsumer(queue, applierFn, ResultConsumerConfig{BatchSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := consumer.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "temporary database failure") {
		t.Fatalf("transient apply should be retried: %v", err)
	}
	if stats != (ResultConsumerStats{Received: 4, Applied: 1, Duplicate: 1, Stale: 1, Failed: 1}) {
		t.Fatalf("unexpected result stats: %+v", stats)
	}
	if len(queue.deleted) != 3 || queue.deleted[0] != "applied" || queue.deleted[1] != "duplicate" || queue.deleted[2] != "stale" {
		t.Fatalf("only durable outcomes should be acknowledged: %v", queue.deleted)
	}
	if queue.limit != 4 {
		t.Fatalf("configured result batch size = %d, want 4", queue.limit)
	}
}

func TestWorkloadResultConsumerDefaultsToBatchOne(t *testing.T) {
	queue := &fakeResultQueue{}
	applier := &fakeResultApplier{}
	consumer, err := NewWorkloadResultConsumer(queue, applier, ResultConsumerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := consumer.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if queue.limit != 1 {
		t.Fatalf("default result batch size = %d, want 1", queue.limit)
	}
}

func TestWorkloadResultConsumerFailureLogIncludesCorrelationWithoutRawError(t *testing.T) {
	const correlationID = "req_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	body, err := json.Marshal(WorkloadResult{SchemaVersion: 1, TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1",
		Generation: 1, ResourcePlaneID: "rp_1", CorrelationID: correlationID, CommitSHA: strings.Repeat("a", 40),
		Status: domain.WorkloadFailed, ExitCode: 1, CompletedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	queue := &fakeResultQueue{deliveries: []ResultDelivery{{ReceiptHandle: "retry", Body: body}}}
	var logs bytes.Buffer
	consumer, err := NewWorkloadResultConsumer(queue, &fakeResultApplier{err: errors.New("database secret detail")}, ResultConsumerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	consumer.logger = slog.New(slog.NewTextHandler(&logs, nil))
	if _, err := consumer.RunOnce(context.Background()); err == nil {
		t.Fatal("transient result application failure was acknowledged")
	}
	logged := logs.String()
	if !strings.Contains(logged, "correlation_id="+correlationID) || !strings.Contains(logged, "failure_class=apply_result") ||
		!strings.Contains(logged, "workload_run_id=run_1") {
		t.Fatalf("failure log lacks safe correlation fields: %s", logged)
	}
	if strings.Contains(logged, "database secret detail") {
		t.Fatalf("raw error leaked into structured log: %s", logged)
	}
}

type failingThirdApplier struct {
	applier *fakeResultApplier
	calls   int
}

func (a *failingThirdApplier) Handle(ctx context.Context, result WorkloadResult) (ResultOutcome, error) {
	a.calls++
	if a.calls == 4 {
		return "", errors.New("temporary database failure")
	}
	return a.applier.Handle(ctx, result)
}

func TestDecodeWorkloadResultRejectsMalformedAndUnknownData(t *testing.T) {
	valid, err := json.Marshal(WorkloadResult{SchemaVersion: 1, TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1",
		Generation: 1, ResourcePlaneID: "rp_1", CommitSHA: strings.Repeat("a", 40), Status: domain.WorkloadSucceeded,
		CompletedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.TrimSuffix(string(valid), "}") + `,"token":"secret"}`
	for _, payload := range [][]byte{
		{},
		append(append([]byte(nil), valid...), []byte(` {}`)...),
		[]byte(unknown),
		[]byte(strings.Repeat("a", (256<<10)+1)),
	} {
		if _, err := decodeWorkloadResult(payload); err == nil {
			t.Errorf("decodeWorkloadResult(%d bytes) unexpectedly succeeded", len(payload))
		}
	}
	if _, err := decodeWorkloadResult(valid); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}
}
