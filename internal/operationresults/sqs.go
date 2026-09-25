package operationresults

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

type sqsAPI interface {
	ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

type SQSQueue struct {
	client   sqsAPI
	queueURL string
}

func NewSQSQueue(client sqsAPI, queueURL string) (*SQSQueue, error) {
	if client == nil || strings.TrimSpace(queueURL) == "" {
		return nil, errors.New("SQS client and result queue URL are required")
	}
	return &SQSQueue{client: client, queueURL: queueURL}, nil
}

func (q *SQSQueue) Receive(ctx context.Context, limit int, wait time.Duration) ([]Delivery, error) {
	output, err := q.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: stringPtr(q.queueURL), MaxNumberOfMessages: int32(limit), WaitTimeSeconds: int32(wait / time.Second),
	})
	if err != nil {
		return nil, fmt.Errorf("receive result SQS messages: %w", err)
	}
	deliveries := make([]Delivery, 0, len(output.Messages))
	for _, message := range output.Messages {
		if message.Body == nil || message.ReceiptHandle == nil {
			deliveries = append(deliveries, Delivery{})
			continue
		}
		deliveries = append(deliveries, Delivery{Body: []byte(*message.Body), ReceiptHandle: *message.ReceiptHandle})
	}
	return deliveries, nil
}

func (q *SQSQueue) Delete(ctx context.Context, receiptHandle string) error {
	if receiptHandle == "" {
		return errors.New("SQS result receipt handle is required")
	}
	if _, err := q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: stringPtr(q.queueURL), ReceiptHandle: stringPtr(receiptHandle)}); err != nil {
		return fmt.Errorf("delete result SQS message: %w", err)
	}
	return nil
}

func stringPtr(value string) *string { return &value }
