package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func TestEventMuxRoutesOnlyCommandRecoveryAndArtifactCleanupEnvelopes(t *testing.T) {
	commandCalls, recoveryCalls, cleanupCalls := 0, 0, 0
	mux := newEventMux(func(_ context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
		commandCalls++
		if len(event.Records) != 1 || event.Records[0].MessageId != "message-1" {
			t.Fatalf("unexpected command event: %+v", event)
		}
		return events.SQSEventResponse{BatchItemFailures: []events.SQSBatchItemFailure{}}, nil
	}, func(_ context.Context, event events.CloudWatchEvent) (any, error) {
		recoveryCalls++
		if event.Source != "aws.events" || event.ID != "schedule-1" {
			t.Fatalf("unexpected recovery event: %+v", event)
		}
		return map[string]string{"status": "ok"}, nil
	}, func(_ context.Context) (any, error) {
		cleanupCalls++
		return map[string]string{"status": "cleaned"}, nil
	})

	commandPayload := json.RawMessage(`{"Records":[{"messageId":"message-1","body":"{}"}]}`)
	result, err := mux(context.Background(), commandPayload)
	if err != nil {
		t.Fatalf("route SQS command event: %v", err)
	}
	if _, ok := result.(events.SQSEventResponse); !ok {
		t.Fatalf("command event returned unexpected result type %T", result)
	}

	recoveryPayload := json.RawMessage(`{"version":"0","id":"schedule-1","source":"aws.events","detail-type":"Scheduled Event","detail":{}}`)
	result, err = mux(context.Background(), recoveryPayload)
	if err != nil {
		t.Fatalf("route scheduled recovery event: %v", err)
	}
	if _, ok := result.(map[string]string); !ok {
		t.Fatalf("recovery event returned unexpected result type %T", result)
	}
	result, err = mux(context.Background(), json.RawMessage(`{"source":"aws.events","hako_task":"artifact-cleanup"}`))
	if err != nil {
		t.Fatalf("route artifact cleanup event: %v", err)
	}
	if _, ok := result.(map[string]string); !ok {
		t.Fatalf("cleanup event returned unexpected result type %T", result)
	}
	if commandCalls != 1 || recoveryCalls != 1 || cleanupCalls != 1 {
		t.Fatalf("event handlers called command=%d recovery=%d cleanup=%d", commandCalls, recoveryCalls, cleanupCalls)
	}
}

func TestEventMuxRejectsInvalidAmbiguousAndUnknownEvents(t *testing.T) {
	noopCommand := func(context.Context, events.SQSEvent) (events.SQSEventResponse, error) {
		return events.SQSEventResponse{}, nil
	}
	noopRecovery := func(context.Context, events.CloudWatchEvent) (any, error) { return nil, nil }
	mux := newEventMux(noopCommand, noopRecovery)
	tests := []struct {
		name    string
		ctx     context.Context
		payload json.RawMessage
	}{
		{name: "nil context", payload: json.RawMessage(`{"source":"aws.events"}`)},
		{name: "empty", ctx: context.Background()},
		{name: "malformed", ctx: context.Background(), payload: json.RawMessage(`{`)},
		{name: "unknown", ctx: context.Background(), payload: json.RawMessage(`{"source":"aws.other"}`)},
		{name: "ambiguous", ctx: context.Background(), payload: json.RawMessage(`{"source":"aws.events","Records":[]}`)},
		{name: "null records", ctx: context.Background(), payload: json.RawMessage(`{"Records":null}`)},
		{name: "non-scheduled EventBridge event", ctx: context.Background(), payload: json.RawMessage(`{"source":"aws.events","detail-type":"AWS API Call via CloudTrail"}`)},
		{name: "unknown scheduled task", ctx: context.Background(), payload: json.RawMessage(`{"source":"aws.events","hako_task":"delete-everything"}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := mux(test.ctx, test.payload); err == nil {
				t.Fatal("invalid Lambda event was accepted")
			}
		})
	}
}
