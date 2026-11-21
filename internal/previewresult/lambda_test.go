package previewresult

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/mo3789530/hako/internal/previewprotocol"
)

func TestLambdaHandlerRejectsMissingConfiguration(t *testing.T) {
	if _, err := NewLambdaHandler(Consumer{}); err == nil {
		t.Fatal("NewLambdaHandler accepted a missing database pool")
	}
	var handler *LambdaHandler
	if _, err := handler.Handle(context.Background(), events.SQSEvent{}); err == nil {
		t.Fatal("nil LambdaHandler accepted an event")
	}
}

func TestLambdaHandlerRetriesInvalidRecordsWithoutLoggingPayload(t *testing.T) {
	var logs bytes.Buffer
	handler, err := NewLambdaHandler(Consumer{Pool: previewResultNeverBeginPool{}})
	if err != nil {
		t.Fatal(err)
	}
	handler.logger = slog.New(slog.NewTextHandler(&logs, nil))
	futureResult := previewprotocol.Result{
		SchemaVersion: 1, Action: previewprotocol.ActionProvision, TenantID: "tenant_test", InstallationID: 17,
		RepositoryID: 23, PullRequestNumber: 4, Generation: 2,
		RuntimeWorkspaceID: "prvws_0123456789abcdef0123456789abcdef", ResourcePlaneID: "rp-test",
		ClaimID: "preview-claim-1234", ClaimAttempt: 1, LeaseExpiresAt: time.Now().Add(time.Minute),
		Status: "succeeded", Binding: &previewprotocol.RuntimeBinding{MicroVMID: "microvm-1", GatewayRouteID: "route-1"},
		CompletedAt: time.Now().Add(time.Hour),
	}
	body, err := previewprotocol.EncodeResult(futureResult)
	if err != nil {
		t.Fatal(err)
	}
	str := func(value string) *string { return &value }
	event := events.SQSEvent{Records: []events.SQSMessage{
		{MessageId: "msg-invalid-json", Body: "provider-secret-must-not-be-logged", MessageAttributes: map[string]events.SQSMessageAttribute{
			"HakoPreviewClaimID":  {DataType: "String", StringValue: str("preview-claim-1234")},
			"HakoPreviewAction":   {DataType: "String", StringValue: str(previewprotocol.ActionProvision)},
			"HakoResourcePlaneID": {DataType: "String", StringValue: str("rp-test")},
		}},
		{MessageId: "msg-missing-route-attributes", Body: string(body)},
	}}
	response, err := handler.Handle(context.Background(), event)
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if len(response.BatchItemFailures) != 2 || response.BatchItemFailures[0].ItemIdentifier != "msg-invalid-json" ||
		response.BatchItemFailures[1].ItemIdentifier != "msg-missing-route-attributes" {
		t.Fatalf("batch failures = %+v, want both invalid messages retried", response.BatchItemFailures)
	}
	if strings.Contains(logs.String(), "provider-secret-must-not-be-logged") || !strings.Contains(logs.String(), "preview_result_processing") {
		t.Fatalf("failure log leaked message content or omitted its fixed class: %q", logs.String())
	}
}
