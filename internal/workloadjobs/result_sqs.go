package workloadjobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

type ResultSQSQueue struct {
	client   sqsAPI
	queueURL string
}

func NewResultSQSQueue(client sqsAPI, queueURL string) (*ResultSQSQueue, error) {
	if client == nil || strings.TrimSpace(queueURL) == "" {
		return nil, errors.New("SQS client and Workload result queue URL are required")
	}
	return &ResultSQSQueue{client: client, queueURL: queueURL}, nil
}

func (q *ResultSQSQueue) Receive(ctx context.Context, limit int, wait time.Duration) ([]ResultDelivery, error) {
	if limit < 1 || limit > 10 || wait < 0 || wait > 20*time.Second {
		return nil, errors.New("Workload result SQS receive limit must be 1-10 and wait time 0-20 seconds")
	}
	output, err := q.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: stringPointer(q.queueURL), MaxNumberOfMessages: int32(limit), WaitTimeSeconds: int32(wait / time.Second),
	})
	if err != nil {
		return nil, fmt.Errorf("receive Workload result SQS messages: %w", err)
	}
	deliveries := make([]ResultDelivery, 0, len(output.Messages))
	for _, message := range output.Messages {
		if message.Body == nil || message.ReceiptHandle == nil {
			deliveries = append(deliveries, ResultDelivery{})
			continue
		}
		deliveries = append(deliveries, ResultDelivery{Body: []byte(*message.Body), ReceiptHandle: *message.ReceiptHandle})
	}
	return deliveries, nil
}

func (q *ResultSQSQueue) Delete(ctx context.Context, receiptHandle string) error {
	if strings.TrimSpace(receiptHandle) == "" {
		return errors.New("Workload result SQS receipt handle is required")
	}
	if _, err := q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: stringPointer(q.queueURL), ReceiptHandle: stringPointer(receiptHandle)}); err != nil {
		return fmt.Errorf("delete Workload result SQS message: %w", err)
	}
	return nil
}
