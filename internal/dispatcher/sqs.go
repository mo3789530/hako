package dispatcher

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/mo3789530/hako/internal/store/outbox"
)

type sqsAPI interface {
	SendMessage(context.Context, *sqs.SendMessageInput, ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

type SQSPublisher struct {
	client sqsAPI
}

func NewSQSPublisher(client sqsAPI) (*SQSPublisher, error) {
	if client == nil {
		return nil, fmt.Errorf("SQS client is required")
	}
	return &SQSPublisher{client: client}, nil
}

func (p *SQSPublisher) Publish(ctx context.Context, queueURL string, event outbox.Event) error {
	attributes := map[string]types.MessageAttributeValue{
		"HakoEventID":   {DataType: stringPointer("String"), StringValue: stringPointer(event.ID)},
		"HakoEventType": {DataType: stringPointer("String"), StringValue: stringPointer(event.EventType)},
		"AggregateID":   {DataType: stringPointer("String"), StringValue: stringPointer(event.AggregateID)},
	}
	input := &sqs.SendMessageInput{
		QueueUrl:          stringPointer(queueURL),
		MessageBody:       stringPointer(string(event.Payload)),
		MessageAttributes: attributes,
	}
	if strings.HasSuffix(queueURL, ".fifo") {
		input.MessageGroupId = stringPointer(event.AggregateID)
		input.MessageDeduplicationId = stringPointer(event.ID)
	}
	if _, err := p.client.SendMessage(ctx, input); err != nil {
		return fmt.Errorf("send SQS message: %w", err)
	}
	return nil
}

func stringPointer(value string) *string { return &value }
