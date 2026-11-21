package workloadcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadjobs"
)

type captureWorkloadResultSQS struct {
	input *sqs.SendMessageInput
	err   error
}

func (c *captureWorkloadResultSQS) SendMessage(_ context.Context, input *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	c.input = input
	return &sqs.SendMessageOutput{}, c.err
}

func TestSQSResultPublisherValidatesAndPublishesStandardQueueMessage(t *testing.T) {
	client := &captureWorkloadResultSQS{}
	publisher, err := NewSQSResultPublisher(client, "https://sqs.ap-northeast-1.amazonaws.com/123456789012/hako-workload-results")
	if err != nil {
		t.Fatal(err)
	}
	result := workloadjobs.WorkloadResult{SchemaVersion: 1, TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1",
		Generation: 1, ResourcePlaneID: "rp_1", CommitSHA: strings.Repeat("a", 40), Status: domain.WorkloadSucceeded,
		CompletedAt: time.Now().UTC()}
	if err := publisher.Publish(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	if client.input == nil || client.input.MessageBody == nil || client.input.QueueUrl == nil || *client.input.QueueUrl != publisher.queueURL ||
		client.input.MessageGroupId != nil || client.input.MessageDeduplicationId != nil {
		t.Fatalf("standard queue message = %+v", client.input)
	}
	var sent workloadjobs.WorkloadResult
	if err := json.Unmarshal([]byte(*client.input.MessageBody), &sent); err != nil || !reflect.DeepEqual(sent, result) {
		t.Fatalf("published result = %+v err=%v", sent, err)
	}
}

func TestSQSResultPublisherUsesStableFIFOGroupAndDeduplication(t *testing.T) {
	client := &captureWorkloadResultSQS{}
	publisher, err := NewSQSResultPublisher(client, "https://sqs.ap-northeast-1.amazonaws.com/123456789012/hako-workload-results.fifo")
	if err != nil {
		t.Fatal(err)
	}
	result := workloadjobs.WorkloadResult{SchemaVersion: 1, TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1",
		Generation: 1, ResourcePlaneID: "rp_1", CommitSHA: strings.Repeat("b", 40), Status: domain.WorkloadFailed,
		ExitCode: 1, ErrorCode: "test_failed", CompletedAt: time.Now().UTC()}
	if err := publisher.Publish(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	first := client.input
	if first.MessageGroupId == nil || first.MessageDeduplicationId == nil || len(*first.MessageGroupId) != 64 || len(*first.MessageDeduplicationId) != 64 {
		t.Fatalf("FIFO identifiers are missing or unbounded: %+v", first)
	}
	if err := publisher.Publish(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	if *client.input.MessageGroupId != *first.MessageGroupId || *client.input.MessageDeduplicationId != *first.MessageDeduplicationId {
		t.Fatal("retry did not preserve deterministic FIFO identity")
	}
}

func TestSQSResultPublisherFailsClosedBeforeSend(t *testing.T) {
	for _, queueURL := range []string{"http://sqs.example.invalid/q", "https://user:pass@sqs.example.invalid/q", "https://sqs.example.invalid/q?token=x", "https://sqs.example.invalid"} {
		if _, err := NewSQSResultPublisher(&captureWorkloadResultSQS{}, queueURL); err == nil {
			t.Fatalf("unsafe queue URL accepted: %q", queueURL)
		}
	}
	client := &captureWorkloadResultSQS{}
	publisher, err := NewSQSResultPublisher(client, "https://sqs.example.invalid/queue")
	if err != nil {
		t.Fatal(err)
	}
	invalid := workloadjobs.WorkloadResult{SchemaVersion: 1, TenantID: "tenant_1"}
	if err := publisher.Publish(context.Background(), invalid); err == nil || client.input != nil {
		t.Fatalf("invalid result reached SQS: input=%+v err=%v", client.input, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	valid := workloadjobs.WorkloadResult{SchemaVersion: 1, TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1",
		Generation: 1, ResourcePlaneID: "rp_1", CommitSHA: strings.Repeat("a", 40), Status: domain.WorkloadSucceeded,
		CompletedAt: time.Now().UTC()}
	if err := publisher.Publish(cancelled, valid); !errors.Is(err, context.Canceled) || client.input != nil {
		t.Fatalf("cancelled context reached SQS or returned wrong error: input=%+v err=%v", client.input, err)
	}
}

func TestSQSResultPublisherPropagatesSendFailure(t *testing.T) {
	client := &captureWorkloadResultSQS{err: errors.New("queue unavailable")}
	publisher, err := NewSQSResultPublisher(client, "https://sqs.example.invalid/queue")
	if err != nil {
		t.Fatal(err)
	}
	result := workloadjobs.WorkloadResult{SchemaVersion: 1, TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1",
		Generation: 1, ResourcePlaneID: "rp_1", CommitSHA: strings.Repeat("a", 40), Status: domain.WorkloadSucceeded, CompletedAt: time.Now().UTC()}
	if err := publisher.Publish(context.Background(), result); err == nil || !strings.Contains(err.Error(), "send Workload result") {
		t.Fatalf("queue error was not surfaced safely: %v", err)
	}
}
