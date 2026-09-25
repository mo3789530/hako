package operationresults

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type resultSQSClient struct {
	receiveInput *sqs.ReceiveMessageInput
	deleteInput  *sqs.DeleteMessageInput
	messages     []types.Message
	receiveErr   error
	deleteErr    error
}

func (c *resultSQSClient) ReceiveMessage(_ context.Context, input *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	c.receiveInput = input
	return &sqs.ReceiveMessageOutput{Messages: c.messages}, c.receiveErr
}
func (c *resultSQSClient) DeleteMessage(_ context.Context, input *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	c.deleteInput = input
	return &sqs.DeleteMessageOutput{}, c.deleteErr
}

func TestSQSQueueReceiveDeleteAndFailures(t *testing.T) {
	if _, err := NewSQSQueue(nil, "queue"); err == nil {
		t.Fatal("nil client should fail")
	}
	if _, err := NewSQSQueue(&resultSQSClient{}, " "); err == nil {
		t.Fatal("empty URL should fail")
	}
	client := &resultSQSClient{messages: []types.Message{
		{Body: stringPtr("payload"), ReceiptHandle: stringPtr("receipt")},
		{Body: stringPtr("missing receipt")},
	}}
	queue, err := NewSQSQueue(client, "https://sqs.example/results")
	if err != nil {
		t.Fatal(err)
	}
	deliveries, err := queue.Receive(context.Background(), 7, 3*time.Second)
	if err != nil || len(deliveries) != 2 || string(deliveries[0].Body) != "payload" || deliveries[0].ReceiptHandle != "receipt" || deliveries[1].ReceiptHandle != "" {
		t.Fatalf("receive result = %+v, %v", deliveries, err)
	}
	if client.receiveInput.MaxNumberOfMessages != 7 || client.receiveInput.WaitTimeSeconds != 3 || *client.receiveInput.QueueUrl != "https://sqs.example/results" {
		t.Fatalf("unexpected receive request: %+v", client.receiveInput)
	}
	if err := queue.Delete(context.Background(), ""); err == nil {
		t.Fatal("empty receipt should fail")
	}
	if err := queue.Delete(context.Background(), "receipt"); err != nil || *client.deleteInput.ReceiptHandle != "receipt" {
		t.Fatalf("delete result = %v, request=%+v", err, client.deleteInput)
	}
	client.receiveErr = errors.New("receive failed")
	if _, err := queue.Receive(context.Background(), 1, 0); err == nil {
		t.Fatal("receive error should propagate")
	}
	client.deleteErr = errors.New("delete failed")
	if err := queue.Delete(context.Background(), "receipt"); err == nil {
		t.Fatal("delete error should propagate")
	}
}
