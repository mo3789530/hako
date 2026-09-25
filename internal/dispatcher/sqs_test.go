package dispatcher

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/mo3789530/hako/internal/store/outbox"
)

type captureSQS struct {
	input *sqs.SendMessageInput
	err   error
}

func (c *captureSQS) SendMessage(_ context.Context, input *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	c.input = input
	return &sqs.SendMessageOutput{}, c.err
}

func TestSQSPublisherStandardQueueAndFailures(t *testing.T) {
	if _, err := NewSQSPublisher(nil); err == nil {
		t.Fatal("nil SQS client should fail")
	}
	client := &captureSQS{}
	publisher, err := NewSQSPublisher(client)
	if err != nil {
		t.Fatal(err)
	}
	event := outbox.Event{ID: "evt_2", AggregateID: "op_2", EventType: "operation.requested", Payload: []byte(`{}`)}
	if err := publisher.Publish(context.Background(), "https://sqs.example/standard", event); err != nil {
		t.Fatalf("publish standard queue event: %v", err)
	}
	if client.input.MessageGroupId != nil || client.input.MessageDeduplicationId != nil {
		t.Fatalf("standard queue should not have FIFO attributes: %+v", client.input)
	}
	client.err = errors.New("network unavailable")
	if err := publisher.Publish(context.Background(), "https://sqs.example/standard", event); err == nil {
		t.Fatal("SQS send failure should be returned")
	}
}

func TestSQSPublisherAddsStableEventAttributesAndFIFOKeys(t *testing.T) {
	client := &captureSQS{}
	publisher, err := NewSQSPublisher(client)
	if err != nil {
		t.Fatal(err)
	}
	event := outbox.Event{ID: "evt_1", AggregateID: "op_1", EventType: "operation.requested", Payload: []byte(`{"schema_version":1}`)}
	queueURL := "https://sqs.ap-northeast-1.amazonaws.com/123456789012/hako.fifo"
	if err := publisher.Publish(context.Background(), queueURL, event); err != nil {
		t.Fatalf("publish FIFO event: %v", err)
	}
	if awsString(client.input.QueueUrl) != queueURL || awsString(client.input.MessageBody) != string(event.Payload) || awsString(client.input.MessageGroupId) != event.AggregateID || awsString(client.input.MessageDeduplicationId) != event.ID {
		t.Fatalf("unexpected FIFO SQS message: %+v", client.input)
	}
	if awsString(client.input.MessageAttributes["HakoEventID"].StringValue) != event.ID || awsString(client.input.MessageAttributes["HakoEventType"].StringValue) != event.EventType {
		t.Fatalf("missing event metadata attributes: %+v", client.input.MessageAttributes)
	}
}

func awsString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
