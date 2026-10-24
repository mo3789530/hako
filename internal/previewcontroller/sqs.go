package previewcontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/mo3789530/hako/internal/previewprotocol"
)

type sqsAPI interface {
	SendMessage(context.Context, *sqs.SendMessageInput, ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

type SQSResultSink struct {
	client   sqsAPI
	queueURL string
}

func NewSQSResultSink(client sqsAPI, queueURL string) (*SQSResultSink, error) {
	if client == nil || strings.TrimSpace(queueURL) == "" {
		return nil, errors.New("SQS client and Preview result queue URL are required")
	}
	return &SQSResultSink{client: client, queueURL: queueURL}, nil
}

func (s *SQSResultSink) Report(ctx context.Context, result Result) error {
	if s == nil || s.client == nil || strings.TrimSpace(s.queueURL) == "" || ctx == nil || !validResult(result) {
		return errors.New("invalid Preview runtime result")
	}
	body, err := previewprotocol.EncodeResult(result)
	if err != nil {
		return fmt.Errorf("encode Preview runtime result: %w", err)
	}
	attributes := map[string]types.MessageAttributeValue{
		"HakoPreviewClaimID":  {DataType: stringPointer("String"), StringValue: stringPointer(result.ClaimID)},
		"HakoPreviewAction":   {DataType: stringPointer("String"), StringValue: stringPointer(result.Action)},
		"HakoResourcePlaneID": {DataType: stringPointer("String"), StringValue: stringPointer(result.ResourcePlaneID)},
	}
	input := &sqs.SendMessageInput{QueueUrl: stringPointer(s.queueURL), MessageBody: stringPointer(string(body)), MessageAttributes: attributes}
	if strings.HasSuffix(s.queueURL, ".fifo") {
		input.MessageGroupId = stringPointer(result.RuntimeWorkspaceID)
		digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%d", result.Action, result.RuntimeWorkspaceID, result.Generation, result.ClaimID, result.ClaimAttempt)))
		input.MessageDeduplicationId = stringPointer(hex.EncodeToString(digest[:]))
	}
	if _, err := s.client.SendMessage(ctx, input); err != nil {
		return fmt.Errorf("send Preview runtime result to SQS: %w", err)
	}
	return nil
}

func stringPointer(value string) *string { return &value }
