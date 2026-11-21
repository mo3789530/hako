package dispatcher

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/mo3789530/hako/internal/store/outbox"
)

type SQSAPI interface {
	SendMessage(context.Context, *sqs.SendMessageInput, ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

type SQSPublisher struct {
	client            SQSAPI
	clientsByQueueURL map[string]SQSAPI
}

func NewSQSPublisher(client SQSAPI) (*SQSPublisher, error) {
	if client == nil {
		return nil, fmt.Errorf("SQS client is required")
	}
	return &SQSPublisher{client: client}, nil
}

// NewRegionalSQSPublisher uses a queue-specific SQS client when supplied,
// allowing one Control Plane Dispatcher to sign requests for multiple AWS
// Regions while retaining a default client for legacy URL-map configuration.
func NewRegionalSQSPublisher(defaultClient SQSAPI, clientsByQueueURL map[string]SQSAPI) (*SQSPublisher, error) {
	if defaultClient == nil {
		return nil, fmt.Errorf("default SQS client is required")
	}
	clients := make(map[string]SQSAPI, len(clientsByQueueURL))
	for queueURL, client := range clientsByQueueURL {
		if strings.TrimSpace(queueURL) == "" || client == nil {
			return nil, fmt.Errorf("queue-specific SQS clients require non-empty queue URLs and clients")
		}
		clients[queueURL] = client
	}
	return &SQSPublisher{client: defaultClient, clientsByQueueURL: clients}, nil
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
	client := p.client
	if regional, ok := p.clientsByQueueURL[queueURL]; ok {
		client = regional
	}
	if _, err := client.SendMessage(ctx, input); err != nil {
		return fmt.Errorf("send SQS message: %w", err)
	}
	return nil
}

func stringPointer(value string) *string { return &value }
