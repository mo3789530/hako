package resourcecontroller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/mo3789530/hako/internal/domain"
)

type resourceSQSClient struct {
	receiveInput    *sqs.ReceiveMessageInput
	deleteInput     *sqs.DeleteMessageInput
	visibilityInput *sqs.ChangeMessageVisibilityInput
	sendInput       *sqs.SendMessageInput
	messages        []types.Message
	err             error
}

func (c *resourceSQSClient) ReceiveMessage(_ context.Context, input *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	c.receiveInput = input
	return &sqs.ReceiveMessageOutput{Messages: c.messages}, c.err
}
func (c *resourceSQSClient) DeleteMessage(_ context.Context, input *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	c.deleteInput = input
	return &sqs.DeleteMessageOutput{}, c.err
}
func (c *resourceSQSClient) ChangeMessageVisibility(_ context.Context, input *sqs.ChangeMessageVisibilityInput, _ ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	c.visibilityInput = input
	return &sqs.ChangeMessageVisibilityOutput{}, c.err
}
func (c *resourceSQSClient) SendMessage(_ context.Context, input *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	c.sendInput = input
	return &sqs.SendMessageOutput{}, c.err
}

func TestSQSQueueReceivesMessagesWithRetryCountAndValidatesOperations(t *testing.T) {
	if _, err := NewSQSQueue(nil, "queue"); err == nil {
		t.Fatal("nil client should fail")
	}
	if _, err := NewSQSQueue(&resourceSQSClient{}, " "); err == nil {
		t.Fatal("empty queue URL should fail")
	}
	client := &resourceSQSClient{messages: []types.Message{
		{Body: stringPointer("valid"), ReceiptHandle: stringPointer("r1"), Attributes: map[string]string{string(types.MessageSystemAttributeNameApproximateReceiveCount): "4"}},
		{Body: stringPointer("bad count"), ReceiptHandle: stringPointer("r2"), Attributes: map[string]string{string(types.MessageSystemAttributeNameApproximateReceiveCount): "nope"}},
		{Body: stringPointer("missing receipt")},
	}}
	queue, err := NewSQSQueue(client, "https://sqs.example/commands")
	if err != nil {
		t.Fatal(err)
	}
	deliveries, err := queue.Receive(context.Background(), 4, 5*time.Second, 30*time.Second)
	if err != nil || len(deliveries) != 3 || deliveries[0].ReceiveCount != 4 || deliveries[1].ReceiveCount != 1 || deliveries[2].ReceiptHandle != "" {
		t.Fatalf("receive deliveries = %+v, %v", deliveries, err)
	}
	if client.receiveInput.MaxNumberOfMessages != 4 || client.receiveInput.WaitTimeSeconds != 5 || client.receiveInput.VisibilityTimeout != 30 {
		t.Fatalf("unexpected receive settings: %+v", client.receiveInput)
	}
	if err := queue.Delete(context.Background(), ""); err == nil {
		t.Fatal("empty receipt should fail")
	}
	if err := queue.Delete(context.Background(), "r1"); err != nil || *client.deleteInput.ReceiptHandle != "r1" {
		t.Fatalf("delete = %v request=%+v", err, client.deleteInput)
	}
	for _, timeout := range []time.Duration{0, 500 * time.Millisecond, 1500 * time.Millisecond, 13 * time.Hour} {
		if err := queue.ChangeVisibility(context.Background(), "r1", timeout); err == nil {
			t.Errorf("invalid visibility %s should fail", timeout)
		}
	}
	if err := queue.ChangeVisibility(context.Background(), "", time.Second); err == nil {
		t.Fatal("empty receipt should fail visibility change")
	}
	if err := queue.ChangeVisibility(context.Background(), "r1", 60*time.Second); err != nil || client.visibilityInput.VisibilityTimeout != 60 {
		t.Fatalf("change visibility = %v request=%+v", err, client.visibilityInput)
	}
	client.err = errors.New("SQS unavailable")
	if _, err := queue.Receive(context.Background(), 1, 0, time.Second); err == nil {
		t.Fatal("receive service error should propagate")
	}
	if err := queue.Delete(context.Background(), "r1"); err == nil {
		t.Fatal("delete service error should propagate")
	}
	if err := queue.ChangeVisibility(context.Background(), "r1", time.Second); err == nil {
		t.Fatal("visibility service error should propagate")
	}
}

func TestSQSResultSinkFIFOMetadataAndSendErrors(t *testing.T) {
	if _, err := NewSQSResultSink(nil, "queue"); err == nil {
		t.Fatal("nil client should fail")
	}
	if _, err := NewSQSResultSink(&resourceSQSClient{}, ""); err == nil {
		t.Fatal("empty result URL should fail")
	}
	client := &resourceSQSClient{}
	sink, err := NewSQSResultSink(client, "https://sqs.example/results.fifo")
	if err != nil {
		t.Fatal(err)
	}
	result := Result{SchemaVersion: 1, OperationID: "op_1", Type: domain.OperationEnsureRunning, Status: domain.OperationSucceeded}
	if err := sink.Report(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	if client.sendInput.MessageGroupId == nil || *client.sendInput.MessageGroupId != "op_1" || *client.sendInput.MessageDeduplicationId != "op_1:succeeded" || client.sendInput.MessageAttributes["HakoOperationID"].StringValue == nil {
		t.Fatalf("unexpected FIFO result message: %+v", client.sendInput)
	}
	client.err = errors.New("send failed")
	if err := sink.Report(context.Background(), result); err == nil {
		t.Fatal("send failure should propagate")
	}
}
