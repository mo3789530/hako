package workloadjobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type fakeSQS struct {
	receiveInput    *sqs.ReceiveMessageInput
	deleteInput     *sqs.DeleteMessageInput
	visibilityInput *sqs.ChangeMessageVisibilityInput
	output          *sqs.ReceiveMessageOutput
	err             error
}

func (f *fakeSQS) ChangeMessageVisibility(_ context.Context, input *sqs.ChangeMessageVisibilityInput, _ ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	f.visibilityInput = input
	if f.err != nil {
		return nil, f.err
	}
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}

func (f *fakeSQS) ReceiveMessage(_ context.Context, input *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.receiveInput = input
	if f.err != nil {
		return nil, f.err
	}
	return f.output, nil
}

func (f *fakeSQS) DeleteMessage(_ context.Context, input *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	f.deleteInput = input
	if f.err != nil {
		return nil, f.err
	}
	return &sqs.DeleteMessageOutput{}, nil
}

func TestSQSQueueReceivesAndDeletesScheduleMessages(t *testing.T) {
	client := &fakeSQS{output: &sqs.ReceiveMessageOutput{Messages: []types.Message{
		{Body: stringPointer(`{"schema_version":1,"tenant_id":"tenant_1","workload_run_id":"run_1"}`), ReceiptHandle: stringPointer("receipt-1")},
		{Body: stringPointer("missing receipt")},
	}}}
	queue, err := NewSQSQueue(client, "https://sqs.example.test/scheduler")
	if err != nil {
		t.Fatal(err)
	}
	deliveries, err := queue.Receive(context.Background(), 5, 3*time.Second)
	if err != nil || len(deliveries) != 2 || deliveries[0].ReceiptHandle != "receipt-1" || deliveries[1].ReceiptHandle != "" {
		t.Fatalf("unexpected SQS deliveries: %+v error=%v", deliveries, err)
	}
	if *client.receiveInput.QueueUrl != "https://sqs.example.test/scheduler" || client.receiveInput.MaxNumberOfMessages != 5 || client.receiveInput.WaitTimeSeconds != 3 {
		t.Fatalf("unexpected SQS receive input: %+v", client.receiveInput)
	}
	if err := queue.Delete(context.Background(), "receipt-1"); err != nil {
		t.Fatal(err)
	}
	if *client.deleteInput.QueueUrl != "https://sqs.example.test/scheduler" || *client.deleteInput.ReceiptHandle != "receipt-1" {
		t.Fatalf("unexpected SQS delete input: %+v", client.deleteInput)
	}
	if err := queue.ChangeVisibility(context.Background(), "receipt-1", 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if *client.visibilityInput.ReceiptHandle != "receipt-1" || client.visibilityInput.VisibilityTimeout != 300 {
		t.Fatalf("unexpected SQS visibility input: %+v", client.visibilityInput)
	}
}

func TestSQSQueueValidatesConfigurationAndPropagatesFailures(t *testing.T) {
	client := &fakeSQS{err: errors.New("SQS unavailable")}
	if _, err := NewSQSQueue(nil, "queue"); err == nil {
		t.Fatal("nil SQS client should be rejected")
	}
	if _, err := NewSQSQueue(client, " "); err == nil {
		t.Fatal("empty queue URL should be rejected")
	}
	queue, err := NewSQSQueue(client, "queue")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Receive(context.Background(), 11, 0); err == nil {
		t.Fatal("excessive receive batch should be rejected")
	}
	if _, err := queue.Receive(context.Background(), 1, 0); err == nil {
		t.Fatal("SQS receive error should be returned")
	}
	if err := queue.Delete(context.Background(), "receipt"); err == nil {
		t.Fatal("SQS delete error should be returned")
	}
	if err := queue.Delete(context.Background(), " "); err == nil {
		t.Fatal("empty receipt handle should be rejected")
	}
	if err := queue.ChangeVisibility(context.Background(), "receipt", 13*time.Hour); err == nil {
		t.Fatal("visibility exceeding SQS maximum should be rejected")
	}
	if err := queue.ChangeVisibility(context.Background(), "receipt", time.Second); err == nil {
		t.Fatal("SQS visibility error should be returned")
	}
}
