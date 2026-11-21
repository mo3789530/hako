package resourcecontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type sqsAPI interface {
	ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	ChangeMessageVisibility(context.Context, *sqs.ChangeMessageVisibilityInput, ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
	SendMessage(context.Context, *sqs.SendMessageInput, ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

type SQSQueue struct {
	client   sqsAPI
	queueURL string
}

func NewSQSQueue(client sqsAPI, queueURL string) (*SQSQueue, error) {
	if client == nil || strings.TrimSpace(queueURL) == "" {
		return nil, errors.New("SQS client and command queue URL are required")
	}
	return &SQSQueue{client: client, queueURL: queueURL}, nil
}

func (q *SQSQueue) Receive(ctx context.Context, maxMessages int, wait, visibility time.Duration) ([]Delivery, error) {
	output, err := q.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:                    stringPointer(q.queueURL),
		MaxNumberOfMessages:         int32(maxMessages),
		WaitTimeSeconds:             int32(wait / time.Second),
		VisibilityTimeout:           int32(visibility / time.Second),
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount},
	})
	if err != nil {
		return nil, fmt.Errorf("receive SQS message: %w", err)
	}
	deliveries := make([]Delivery, 0, len(output.Messages))
	for _, message := range output.Messages {
		if message.Body == nil || message.ReceiptHandle == nil {
			deliveries = append(deliveries, Delivery{})
			continue
		}
		receiveCount := 1
		if raw := message.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)]; raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				receiveCount = parsed
			}
		}
		deliveries = append(deliveries, Delivery{Body: []byte(*message.Body), ReceiptHandle: *message.ReceiptHandle, ReceiveCount: receiveCount})
	}
	return deliveries, nil
}

func (q *SQSQueue) Delete(ctx context.Context, receiptHandle string) error {
	if receiptHandle == "" {
		return errors.New("SQS receipt handle is required")
	}
	if _, err := q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: stringPointer(q.queueURL), ReceiptHandle: stringPointer(receiptHandle)}); err != nil {
		return fmt.Errorf("delete SQS message: %w", err)
	}
	return nil
}

func (q *SQSQueue) ChangeVisibility(ctx context.Context, receiptHandle string, visibility time.Duration) error {
	if receiptHandle == "" || visibility < time.Second || visibility%time.Second != 0 || visibility > 12*time.Hour {
		return errors.New("SQS receipt handle and visibility timeout (1s-12h, whole seconds) are required")
	}
	_, err := q.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: stringPointer(q.queueURL), ReceiptHandle: stringPointer(receiptHandle),
		VisibilityTimeout: int32(visibility / time.Second),
	})
	if err != nil {
		return fmt.Errorf("change SQS message visibility: %w", err)
	}
	return nil
}

type SQSResultSink struct {
	client   sqsAPI
	queueURL string
}

func NewSQSResultSink(client sqsAPI, queueURL string) (*SQSResultSink, error) {
	if client == nil || strings.TrimSpace(queueURL) == "" {
		return nil, errors.New("SQS client and result queue URL are required")
	}
	return &SQSResultSink{client: client, queueURL: queueURL}, nil
}

func (s *SQSResultSink) Report(ctx context.Context, result Result) error {
	body, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode Operation result: %w", err)
	}
	input := &sqs.SendMessageInput{
		QueueUrl:    stringPointer(s.queueURL),
		MessageBody: stringPointer(string(body)),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"HakoOperationID":  {DataType: stringPointer("String"), StringValue: stringPointer(string(result.OperationID))},
			"HakoResultStatus": {DataType: stringPointer("String"), StringValue: stringPointer(string(result.Status))},
		},
	}
	if strings.HasSuffix(s.queueURL, ".fifo") {
		input.MessageGroupId = stringPointer(string(result.OperationID))
		input.MessageDeduplicationId = stringPointer(string(result.OperationID) + ":" + string(result.Status))
	}
	if _, err := s.client.SendMessage(ctx, input); err != nil {
		return fmt.Errorf("send Operation result to SQS: %w", err)
	}
	return nil
}

func stringPointer(value string) *string { return &value }
