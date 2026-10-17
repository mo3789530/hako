package actionsrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
)

const maxRunnerStartupMessageBytes = 4096

var runnerQueuePathPattern = regexp.MustCompile(`^/[0-9]{12}/[A-Za-z0-9_-]+(?:\.fifo)?$`)

type runnerStartupSQSAPI interface {
	SendMessage(context.Context, *sqs.SendMessageInput, ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

type RunnerStartupSQSPublisher struct {
	client   runnerStartupSQSAPI
	queueURL string
	fifo     bool
}

func NewRunnerStartupSQSPublisher(client runnerStartupSQSAPI, queueURL string) (*RunnerStartupSQSPublisher, error) {
	queueURL = strings.TrimSpace(queueURL)
	parsed, err := url.Parse(queueURL)
	if client == nil || err != nil || parsed == nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Port() != "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || !validAWSRunnerSQSHost(parsed.Hostname()) || !runnerQueuePathPattern.MatchString(parsed.EscapedPath()) || len(queueURL) > 2048 {
		return nil, errors.New("SQS client and HTTPS runner startup queue URL are required")
	}
	return &RunnerStartupSQSPublisher{client: client, queueURL: queueURL, fifo: strings.HasSuffix(parsed.Path, ".fifo")}, nil
}

func validAWSRunnerSQSHost(host string) bool {
	if host != strings.ToLower(host) || !strings.HasPrefix(host, "sqs.") {
		return false
	}
	suffix := ".amazonaws.com"
	if strings.HasSuffix(host, ".amazonaws.com.cn") {
		suffix = ".amazonaws.com.cn"
	}
	region := strings.TrimSuffix(strings.TrimPrefix(host, "sqs."), suffix)
	if region == host || region == "" || strings.Contains(region, ".") {
		return false
	}
	for _, char := range region {
		if !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-') {
			return false
		}
	}
	return true
}

func (p *RunnerStartupSQSPublisher) Publish(ctx context.Context, job githubwebhook.ActionsRunnerJob) error {
	if p == nil || p.client == nil || ctx == nil || job.IntegrationID == "" || job.JobID <= 0 || job.RunAttempt <= 0 || !validClaimID(job.ClaimID) ||
		(job.State != "claimed" && job.State != "starting") || !job.LeaseExpires.After(time.Now().UTC()) {
		return errors.New("valid live Actions runner claim and SQS publisher are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := json.Marshal(runnerStartupMessage{SchemaVersion: 1, IntegrationID: job.IntegrationID, WorkflowJobID: job.JobID, RunAttempt: job.RunAttempt, ClaimID: job.ClaimID})
	if err != nil || len(body) == 0 || len(body) > maxRunnerStartupMessageBytes {
		return errors.New("Actions runner startup message is invalid or oversized")
	}
	input := &sqs.SendMessageInput{QueueUrl: aws.String(p.queueURL), MessageBody: aws.String(string(body))}
	if p.fifo {
		group := sha256.Sum256([]byte(job.IntegrationID))
		dedup := sha256.Sum256(body)
		input.MessageGroupId = aws.String(hex.EncodeToString(group[:]))
		input.MessageDeduplicationId = aws.String(hex.EncodeToString(dedup[:]))
	}
	output, err := p.client.SendMessage(ctx, input)
	if err != nil || output == nil || strings.TrimSpace(aws.ToString(output.MessageId)) == "" {
		return fmt.Errorf("send runner startup command to SQS")
	}
	return nil
}

var _ RunnerStartupPublisher = (*RunnerStartupSQSPublisher)(nil)
