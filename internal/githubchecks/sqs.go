package githubchecks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

type SQSAPI interface {
	ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	ChangeMessageVisibility(context.Context, *sqs.ChangeMessageVisibilityInput, ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
	DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

type SQSQueue struct {
	client            SQSAPI
	queueURL          string
	visibilityTimeout time.Duration
}

func NewSQSQueue(client SQSAPI, queueURL string) (*SQSQueue, error) {
	return NewSQSQueueWithVisibility(client, queueURL, 15*time.Minute)
}

func NewSQSQueueWithVisibility(client SQSAPI, queueURL string, visibilityTimeout time.Duration) (*SQSQueue, error) {
	if client == nil || strings.TrimSpace(queueURL) == "" {
		return nil, errors.New("SQS client and GitHub Checks queue URL are required")
	}
	if visibilityTimeout < time.Second || visibilityTimeout > 12*time.Hour || visibilityTimeout%time.Second != 0 {
		return nil, errors.New("GitHub Checks SQS visibility timeout must be whole seconds between 1 and 43200")
	}
	return &SQSQueue{client: client, queueURL: strings.TrimSpace(queueURL), visibilityTimeout: visibilityTimeout}, nil
}

func (q *SQSQueue) Receive(ctx context.Context, limit int, wait time.Duration) ([]Delivery, error) {
	if limit < 1 || limit > 10 || wait < 0 || wait > 20*time.Second {
		return nil, errors.New("GitHub Checks SQS receive limit must be 1-10 and wait time 0-20 seconds")
	}
	output, err := q.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: stringPointer(q.queueURL), MaxNumberOfMessages: int32(limit), WaitTimeSeconds: int32(wait / time.Second),
		VisibilityTimeout: int32(q.visibilityTimeout / time.Second),
	})
	if err != nil {
		return nil, fmt.Errorf("receive GitHub Checks SQS messages: %w", err)
	}
	if output == nil {
		return nil, errors.New("receive GitHub Checks SQS messages returned an empty response")
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
	if strings.TrimSpace(receiptHandle) == "" {
		return errors.New("GitHub Checks SQS receipt handle is required")
	}
	if _, err := q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: stringPointer(q.queueURL), ReceiptHandle: stringPointer(receiptHandle)}); err != nil {
		return fmt.Errorf("delete GitHub Checks SQS message: %w", err)
	}
	return nil
}

func (q *SQSQueue) ChangeVisibility(ctx context.Context, receiptHandle string, delay time.Duration) error {
	if strings.TrimSpace(receiptHandle) == "" || delay < time.Second || delay > 12*time.Hour || delay%time.Second != 0 {
		return errors.New("GitHub Checks SQS receipt and whole-second visibility delay between 1 and 43200 seconds are required")
	}
	if _, err := q.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: stringPointer(q.queueURL), ReceiptHandle: stringPointer(receiptHandle), VisibilityTimeout: int32(delay / time.Second),
	}); err != nil {
		return fmt.Errorf("change GitHub Checks SQS message visibility: %w", err)
	}
	return nil
}

func stringPointer(value string) *string { return &value }
