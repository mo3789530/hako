package main

import (
	"context"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

type batchHandlerFunc func(context.Context, events.SQSEvent) (events.SQSEventResponse, error)

func (f batchHandlerFunc) Handle(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
	return f(ctx, event)
}

func TestResourceQueueMuxRoutesByExactEventSourceAndPreservesPartialFailures(t *testing.T) {
	var operationIDs, previewIDs []string
	operation := batchHandlerFunc(func(_ context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
		for _, record := range event.Records {
			operationIDs = append(operationIDs, record.MessageId)
		}
		return events.SQSEventResponse{}, nil
	})
	preview := batchHandlerFunc(func(_ context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
		for _, record := range event.Records {
			previewIDs = append(previewIDs, record.MessageId)
		}
		return events.SQSEventResponse{BatchItemFailures: []events.SQSBatchItemFailure{{ItemIdentifier: "preview-failed"}}}, nil
	})
	mux := newSQSHandler(operation, preview, "arn:aws:sqs:us-east-1:123456789012:rp-preview")
	response, err := mux.Handle(context.Background(), events.SQSEvent{Records: []events.SQSMessage{
		{MessageId: "operation", EventSourceARN: "arn:aws:sqs:us-east-1:123456789012:rp-commands"},
		{MessageId: "preview-failed", EventSourceARN: "arn:aws:sqs:us-east-1:123456789012:rp-preview"},
		{MessageId: "preview-unknown", EventSourceARN: "arn:aws:sqs:us-east-1:123456789012:other",
			MessageAttributes: map[string]events.SQSMessageAttribute{"HakoEventType": {StringValue: testStringPointer("repository_preview_runtime.provision")}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(operationIDs) != 1 || operationIDs[0] != "operation" {
		t.Fatalf("operation records = %v", operationIDs)
	}
	if len(previewIDs) != 1 || previewIDs[0] != "preview-failed" {
		t.Fatalf("Preview records = %v", previewIDs)
	}
	if len(response.BatchItemFailures) != 2 || response.BatchItemFailures[0].ItemIdentifier != "preview-unknown" ||
		response.BatchItemFailures[1].ItemIdentifier != "preview-failed" {
		t.Fatalf("partial failures = %+v", response.BatchItemFailures)
	}
}

func TestResourceQueueMuxFailsClosedForPreviewWithoutExactQueueOrHandler(t *testing.T) {
	operation := batchHandlerFunc(func(_ context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
		if len(event.Records) != 0 {
			t.Fatalf("misrouted Preview reached Workspace operation handler: %+v", event.Records)
		}
		return events.SQSEventResponse{}, nil
	})
	record := events.SQSMessage{MessageId: "preview", EventSourceARN: "arn:aws:sqs:us-east-1:123456789012:wrong",
		MessageAttributes: map[string]events.SQSMessageAttribute{"HakoEventType": {StringValue: testStringPointer("repository_preview_runtime.cleanup")}}}
	for _, tc := range []struct {
		name string
		mux  *resourceQueueMux
	}{
		{name: "wrong queue", mux: newSQSHandler(operation, batchHandlerFunc(func(context.Context, events.SQSEvent) (events.SQSEventResponse, error) {
			t.Fatal("Preview handler called for a different queue")
			return events.SQSEventResponse{}, nil
		}), "arn:aws:sqs:us-east-1:123456789012:rp-preview")},
		{name: "handler missing", mux: newSQSHandler(operation, nil, "arn:aws:sqs:us-east-1:123456789012:rp-preview")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, err := tc.mux.Handle(context.Background(), events.SQSEvent{Records: []events.SQSMessage{record}})
			if err != nil || len(response.BatchItemFailures) != 1 || response.BatchItemFailures[0].ItemIdentifier != "preview" {
				t.Fatalf("response=%+v err=%v", response, err)
			}
		})
	}
}

func testStringPointer(value string) *string { return &value }
