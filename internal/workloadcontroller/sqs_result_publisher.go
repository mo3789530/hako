package workloadcontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/mo3789530/hako/internal/workloadjobs"
)

const maxSQSWorkloadResultBytes = 256 << 10

type workloadResultMessageSender interface {
	SendMessage(context.Context, *sqs.SendMessageInput, ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// SQSResultPublisher publishes validated, metadata-only Workload results to a
// dedicated Control Plane result queue. It does not share the Workspace
// Operation result queue because that consumer uses a different envelope.
type SQSResultPublisher struct {
	client   workloadResultMessageSender
	queueURL string
	fifo     bool
}

func NewSQSResultPublisher(client workloadResultMessageSender, queueURL string) (*SQSResultPublisher, error) {
	queueURL = strings.TrimSpace(queueURL)
	parsed, err := url.Parse(queueURL)
	if client == nil || err != nil || parsed == nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path == "" || len(queueURL) > 2048 {
		return nil, errors.New("SQS client and HTTPS Workload result queue URL are required")
	}
	return &SQSResultPublisher{client: client, queueURL: queueURL, fifo: strings.HasSuffix(parsed.Path, ".fifo")}, nil
}

func (p *SQSResultPublisher) Publish(ctx context.Context, result workloadjobs.WorkloadResult) error {
	if p == nil || p.client == nil || ctx == nil || workloadjobs.ValidateResult(result) != nil {
		return errors.New("valid Workload result and SQS publisher are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := json.Marshal(result)
	if err != nil || len(body) == 0 || len(body) > maxSQSWorkloadResultBytes {
		return errors.New("Workload result exceeds the SQS message limit")
	}
	input := &sqs.SendMessageInput{QueueUrl: &p.queueURL, MessageBody: awsString(string(body))}
	if p.fifo {
		groupKey := sha256.Sum256([]byte(string(result.TenantID) + "/" + string(result.WorkloadRunID)))
		dedupKey := sha256.Sum256(body)
		groupID, dedupID := hex.EncodeToString(groupKey[:]), hex.EncodeToString(dedupKey[:])
		input.MessageGroupId, input.MessageDeduplicationId = &groupID, &dedupID
	}
	if _, err := p.client.SendMessage(ctx, input); err != nil {
		return fmt.Errorf("send Workload result to Resource Plane result queue: %w", err)
	}
	return nil
}

func awsString(value string) *string { return &value }

var _ ResultPublisher = (*SQSResultPublisher)(nil)
