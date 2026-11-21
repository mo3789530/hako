package workloadjobs

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

func TestResultSQSQueueReceivesAndDeletes(t *testing.T) {
	client := &fakeSQS{output: &sqs.ReceiveMessageOutput{Messages: []types.Message{
		{Body: stringPointer(`{"schema_version":1}`), ReceiptHandle: stringPointer("receipt-1")},
		{Body: stringPointer("missing receipt")},
	}}}
	queue, err := NewResultSQSQueue(client, "https://sqs.example.test/workload-results")
	if err != nil {
		t.Fatal(err)
	}
	deliveries, err := queue.Receive(context.Background(), 1, 4*time.Second)
	if err != nil || len(deliveries) != 2 || deliveries[0].ReceiptHandle != "receipt-1" || deliveries[1].ReceiptHandle != "" {
		t.Fatalf("unexpected SQS deliveries: %+v error=%v", deliveries, err)
	}
	if *client.receiveInput.QueueUrl != "https://sqs.example.test/workload-results" || client.receiveInput.MaxNumberOfMessages != 1 || client.receiveInput.WaitTimeSeconds != 4 {
		t.Fatalf("unexpected SQS receive input: %+v", client.receiveInput)
	}
	if err := queue.Delete(context.Background(), "receipt-1"); err != nil {
		t.Fatal(err)
	}
	if *client.deleteInput.QueueUrl != "https://sqs.example.test/workload-results" || *client.deleteInput.ReceiptHandle != "receipt-1" {
		t.Fatalf("unexpected SQS delete input: %+v", client.deleteInput)
	}
}
