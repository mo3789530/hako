package workloadjobs

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
	ChangeMessageVisibility(context.Context, *sqs.ChangeMessageVisibilityInput, ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
}

type SQSQueue struct {
	client   sqsAPI
	queueURL string
}

func NewSQSQueue(client sqsAPI, queueURL string) (*SQSQueue, error) {
	if client == nil || strings.TrimSpace(queueURL) == "" {
		return nil, errors.New("SQS client and Workload scheduler queue URL are required")
	}
	return &SQSQueue{client: client, queueURL: queueURL}, nil
}

func (q *SQSQueue) Receive(ctx context.Context, limit int, wait time.Duration) ([]ScheduleDelivery, error) {
	if limit < 1 || limit > 10 || wait < 0 || wait > 20*time.Second {
		return nil, errors.New("SQS receive limit must be 1-10 and wait time 0-20 seconds")
	}
	output, err := q.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: stringPointer(q.queueURL), MaxNumberOfMessages: int32(limit), WaitTimeSeconds: int32(wait / time.Second),
	})
	if err != nil {
		return nil, fmt.Errorf("receive Workload schedule messages: %w", err)
	}
	deliveries := make([]ScheduleDelivery, 0, len(output.Messages))
	for _, message := range output.Messages {
		if message.Body == nil || message.ReceiptHandle == nil {
			deliveries = append(deliveries, ScheduleDelivery{})
			continue
		}
		deliveries = append(deliveries, ScheduleDelivery{Body: []byte(*message.Body), ReceiptHandle: *message.ReceiptHandle})
	}
	return deliveries, nil
}

func (q *SQSQueue) Delete(ctx context.Context, receiptHandle string) error {
	if strings.TrimSpace(receiptHandle) == "" {
		return errors.New("Workload SQS receipt handle is required")
	}
	if _, err := q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: stringPointer(q.queueURL), ReceiptHandle: stringPointer(receiptHandle)}); err != nil {
		return fmt.Errorf("delete Workload schedule message: %w", err)
	}
	return nil
}

func (q *SQSQueue) ChangeVisibility(ctx context.Context, receiptHandle string, timeout time.Duration) error {
	if strings.TrimSpace(receiptHandle) == "" || timeout < time.Second || timeout > 12*time.Hour || timeout%time.Second != 0 {
		return errors.New("Workload SQS receipt handle and whole-second visibility timeout (1s-12h) are required")
	}
	_, err := q.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: stringPointer(q.queueURL), ReceiptHandle: stringPointer(receiptHandle), VisibilityTimeout: int32(timeout / time.Second),
	})
	if err != nil {
		return fmt.Errorf("change Workload message visibility: %w", err)
	}
	return nil
}

func stringPointer(value string) *string { return &value }
