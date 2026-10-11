package githubchecks

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type fakeSQS struct {
	receive *sqs.ReceiveMessageInput
	change  *sqs.ChangeMessageVisibilityInput
	deleted *sqs.DeleteMessageInput
}

func (f *fakeSQS) ReceiveMessage(_ context.Context, input *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.receive = input
	return &sqs.ReceiveMessageOutput{Messages: []types.Message{{Body: stringPointer("payload"), ReceiptHandle: stringPointer("receipt")}}}, nil
}

func (f *fakeSQS) ChangeMessageVisibility(_ context.Context, input *sqs.ChangeMessageVisibilityInput, _ ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	f.change = input
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}

func (f *fakeSQS) DeleteMessage(_ context.Context, input *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	f.deleted = input
	return &sqs.DeleteMessageOutput{}, nil
}

func TestSQSQueueSetsBoundedReceiveVisibilityAndDeletesByReceipt(t *testing.T) {
	client := &fakeSQS{}
	queue, err := NewSQSQueue(client, "https://sqs.invalid/checks")
	if err != nil {
		t.Fatal(err)
	}
	deliveries, err := queue.Receive(context.Background(), 5, 20*time.Second)
	if err != nil || len(deliveries) != 1 || string(deliveries[0].Body) != "payload" || client.receive == nil ||
		client.receive.VisibilityTimeout != 900 {
		t.Fatalf("SQS receive = %+v input=%+v err=%v", deliveries, client.receive, err)
	}
	if err := queue.Delete(context.Background(), "receipt"); err != nil || client.deleted == nil || *client.deleted.ReceiptHandle != "receipt" {
		t.Fatalf("SQS delete input=%+v err=%v", client.deleted, err)
	}
	if err := queue.ChangeVisibility(context.Background(), "receipt", 30*time.Second); err != nil || client.change == nil || client.change.VisibilityTimeout != 30 || *client.change.ReceiptHandle != "receipt" {
		t.Fatalf("SQS visibility change input=%+v err=%v", client.change, err)
	}
}

func TestSQSQueueRejectsInvalidVisibility(t *testing.T) {
	client := &fakeSQS{}
	queue, err := NewSQSQueue(client, "https://sqs.invalid/checks")
	if err != nil {
		t.Fatal(err)
	}
	for _, visibility := range []time.Duration{0, 500 * time.Millisecond, 1500 * time.Millisecond, 12*time.Hour + time.Second} {
		if _, err := NewSQSQueueWithVisibility(client, "https://sqs.invalid/checks", visibility); err == nil {
			t.Fatalf("invalid visibility timeout %s was accepted", visibility)
		}
	}
	for _, delay := range []time.Duration{0, 500 * time.Millisecond, 1500 * time.Millisecond, 12*time.Hour + time.Second} {
		if err := queue.ChangeVisibility(context.Background(), "receipt", delay); err == nil {
			t.Fatalf("invalid visibility delay %s was accepted", delay)
		}
	}
}
