package previewcontroller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

func TestSQSResultSinkUsesClaimIdempotencyAndFIFODeduplication(t *testing.T) {
	client := &captureSQS{}
	sink, err := NewSQSResultSink(client, "https://sqs.us-east-1.amazonaws.com/123456789012/preview-results.fifo")
	if err != nil {
		t.Fatal(err)
	}
	result := Result{
		SchemaVersion: 1, Action: "provision", TenantID: "tenant_1", InstallationID: 17,
		RepositoryID: 23, PullRequestNumber: 4, Generation: 2,
		RuntimeWorkspaceID: "prvws_0123456789abcdef0123456789abcdef", ResourcePlaneID: "rp-test",
		ClaimID: "preview-claim-1234", ClaimAttempt: 3, LeaseExpiresAt: time.Now().Add(time.Minute),
		Status: "succeeded", Binding: &Binding{MicroVMID: "microvm-1", GatewayRouteID: "route-1"}, CompletedAt: time.Now(),
	}
	if err := sink.Report(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	if client.input == nil || client.input.MessageGroupId == nil || *client.input.MessageGroupId != result.RuntimeWorkspaceID ||
		client.input.MessageDeduplicationId == nil || len(*client.input.MessageDeduplicationId) != 64 ||
		client.input.MessageAttributes["HakoPreviewClaimID"].StringValue == nil || *client.input.MessageAttributes["HakoPreviewClaimID"].StringValue != result.ClaimID {
		t.Fatalf("missing Preview result idempotency attributes: %+v", client.input)
	}
	var stored Result
	if err := json.Unmarshal([]byte(*client.input.MessageBody), &stored); err != nil || stored.ClaimAttempt != result.ClaimAttempt || stored.Binding == nil || *stored.Binding != *result.Binding {
		t.Fatalf("result payload did not preserve generation binding: %+v err=%v", stored, err)
	}
}

func TestSQSResultSinkRejectsInvalidResultBeforeSending(t *testing.T) {
	client := &captureSQS{}
	sink, err := NewSQSResultSink(client, "queue")
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Report(context.Background(), Result{}); err == nil || client.input != nil {
		t.Fatalf("invalid result was sent: input=%+v err=%v", client.input, err)
	}
}

type captureSQS struct{ input *sqs.SendMessageInput }

func (c *captureSQS) SendMessage(_ context.Context, input *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	copy := *input
	copy.MessageAttributes = make(map[string]types.MessageAttributeValue, len(input.MessageAttributes))
	for key, value := range input.MessageAttributes {
		copy.MessageAttributes[key] = value
	}
	c.input = &copy
	return &sqs.SendMessageOutput{}, nil
}
