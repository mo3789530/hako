package actionsrunner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
)

type runnerClaimerStub struct {
	jobs     []githubwebhook.ActionsRunnerJob
	claimIDs []string
	lease    time.Duration
	claimErr error
}

func (s *runnerClaimerStub) ClaimNext(_ context.Context, claimID string, now time.Time, lease time.Duration) (githubwebhook.ActionsRunnerJob, bool, error) {
	s.claimIDs = append(s.claimIDs, claimID)
	s.lease = lease
	if s.claimErr != nil {
		return githubwebhook.ActionsRunnerJob{}, false, s.claimErr
	}
	if len(s.jobs) == 0 {
		return githubwebhook.ActionsRunnerJob{}, false, nil
	}
	job := s.jobs[0]
	s.jobs = s.jobs[1:]
	job.ClaimID, job.State, job.LeaseExpires = claimID, "claimed", now.Add(lease)
	return job, true, nil
}

type runnerPublisherStub struct {
	jobs []githubwebhook.ActionsRunnerJob
	err  error
}

func (s *runnerPublisherStub) Publish(_ context.Context, job githubwebhook.ActionsRunnerJob) error {
	s.jobs = append(s.jobs, job)
	return s.err
}

type runnerStartupSQSStub struct {
	input *sqs.SendMessageInput
	err   error
}

func (s *runnerStartupSQSStub) SendMessage(_ context.Context, input *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	s.input = input
	if s.err != nil {
		return nil, s.err
	}
	return &sqs.SendMessageOutput{MessageId: aws.String("message-1")}, nil
}

func TestRunnerStartupDispatcherClaimsAndPublishesMinimalKeys(t *testing.T) {
	now := time.Now().UTC()
	claimer := &runnerClaimerStub{jobs: []githubwebhook.ActionsRunnerJob{{IntegrationID: "integration_runner", JobID: 123, RunAttempt: 2,
		TenantID: "tenant-secret-not-for-queue", Repository: 456, HeadSHA: strings.Repeat("a", 40)}}}
	publisher := &runnerPublisherStub{}
	claimIDs := []string{"runner-claim-0001", "runner-claim-0002"}
	claimIndex := 0
	dispatcher, err := NewRunnerStartupDispatcher(claimer, publisher, RunnerStartupDispatcherConfig{BatchSize: 2, Now: func() time.Time { return now },
		NewClaimID: func() (string, error) {
			claimID := claimIDs[claimIndex]
			claimIndex++
			return claimID, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := dispatcher.DispatchOnce(context.Background())
	if err != nil || stats != (RunnerStartupDispatchStats{Claimed: 1, Published: 1}) || len(publisher.jobs) != 1 ||
		claimer.lease != 10*time.Minute || len(claimer.claimIDs) != 2 || claimer.claimIDs[0] != claimIDs[0] || claimer.claimIDs[1] != claimIDs[1] {
		t.Fatalf("dispatch stats=%+v published=%+v claimIDs=%v lease=%v err=%v", stats, publisher.jobs, claimer.claimIDs, claimer.lease, err)
	}
	client := &runnerStartupSQSStub{}
	sqsPublisher, err := NewRunnerStartupSQSPublisher(client, "https://sqs.ap-northeast-1.amazonaws.com/123456789012/hako-runner.fifo")
	if err != nil {
		t.Fatal(err)
	}
	if err := sqsPublisher.Publish(context.Background(), publisher.jobs[0]); err != nil {
		t.Fatal(err)
	}
	body := aws.ToString(client.input.MessageBody)
	for _, forbidden := range []string{"tenant-secret-not-for-queue", "HeadSHA", strings.Repeat("a", 40), "repository", "labels"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("startup SQS body disclosed %q: %s", forbidden, body)
		}
	}
	if client.input.MessageGroupId == nil || client.input.MessageDeduplicationId == nil || !strings.Contains(body, `"schema_version":1`) {
		t.Fatalf("FIFO/message contract = %+v body=%s", client.input, body)
	}
}

func TestRunnerStartupDispatcherStopsOnPublishFailureAndValidatesInputs(t *testing.T) {
	now := time.Now().UTC()
	claimer := &runnerClaimerStub{jobs: []githubwebhook.ActionsRunnerJob{{IntegrationID: "integration_runner", JobID: 123, RunAttempt: 1}}}
	publisher := &runnerPublisherStub{err: errors.New("SQS unavailable")}
	dispatcher, err := NewRunnerStartupDispatcher(claimer, publisher, RunnerStartupDispatcherConfig{Now: func() time.Time { return now },
		NewClaimID: func() (string, error) { return "runner-claim-0001", nil }})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := dispatcher.DispatchOnce(context.Background())
	if err == nil || stats != (RunnerStartupDispatchStats{Claimed: 1, Failed: 1}) || len(publisher.jobs) != 1 {
		t.Fatalf("publish failure stats=%+v err=%v", stats, err)
	}
	for _, queueURL := range []string{"", "http://sqs.ap-northeast-1.amazonaws.com/123456789012/queue", "https://user@sqs.ap-northeast-1.amazonaws.com/123456789012/queue",
		"https://sqs.ap-northeast-1.amazonaws.com/123456789012/queue?next=other", "https://attacker.example/123456789012/queue",
		"https://sqs.ap-northeast-1.amazonaws.com.evil/123456789012/queue", "https://sqs.ap-northeast-1.amazonaws.com:8443/123456789012/queue"} {
		if _, err := NewRunnerStartupSQSPublisher(&runnerStartupSQSStub{}, queueURL); err == nil {
			t.Fatalf("unsafe SQS URL accepted: %q", queueURL)
		}
	}
	if _, err := NewRunnerStartupSQSPublisher(&runnerStartupSQSStub{err: errors.New("network")}, "https://sqs.ap-northeast-1.amazonaws.com/123456789012/queue"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRunnerStartupDispatcher(claimer, publisher, RunnerStartupDispatcherConfig{LeaseDuration: 11 * time.Minute}); err == nil {
		t.Fatal("overlong lease accepted")
	}
	invalidClaim := &runnerClaimerStub{}
	invalidDispatcher, err := NewRunnerStartupDispatcher(invalidClaim, publisher, RunnerStartupDispatcherConfig{NewClaimID: func() (string, error) { return "bad claim", nil }})
	if err != nil {
		t.Fatal(err)
	}
	stats, err = invalidDispatcher.DispatchOnce(context.Background())
	if err == nil || stats.Failed != 1 || len(invalidClaim.claimIDs) != 0 {
		t.Fatalf("invalid claim stats=%+v calls=%v err=%v", stats, invalidClaim.claimIDs, err)
	}
	if err := (&RunnerStartupSQSPublisher{client: &runnerStartupSQSStub{}, queueURL: "https://sqs.example/q"}).Publish(context.Background(), githubwebhook.ActionsRunnerJob{}); err == nil {
		t.Fatal("invalid/unclaimed job was published")
	}
}

func TestRunnerStartupSQSPublisherRequiresAcknowledgedSend(t *testing.T) {
	publisher, err := NewRunnerStartupSQSPublisher(&runnerStartupSQSStub{}, "https://sqs.ap-northeast-1.amazonaws.com/123456789012/queue")
	if err != nil {
		t.Fatal(err)
	}
	job := githubwebhook.ActionsRunnerJob{IntegrationID: "integration_runner", JobID: 123, RunAttempt: 1, ClaimID: "runner-claim-0001",
		State: "claimed", LeaseExpires: time.Now().Add(time.Minute)}
	if err := publisher.Publish(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}
