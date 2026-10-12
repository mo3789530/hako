package workloadcontroller

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func TestLambdaHandlerReturnsOnlyUncheckpointedWorkloadRecords(t *testing.T) {
	controller, _, api, agent, _ := setupLambda(t)
	handler, err := NewLambdaHandler(controller)
	if err != nil {
		t.Fatal(err)
	}
	valid := envelope(t, nil)
	messageType := "workload_attempt.dispatch"
	response, err := handler.Handle(context.Background(), events.SQSEvent{Records: []events.SQSMessage{
		{MessageId: "valid", Body: string(valid), MessageAttributes: map[string]events.SQSMessageAttribute{
			"HakoEventType": {DataType: "String", StringValue: &messageType},
		}},
		{MessageId: "missing-event-type", Body: string(valid)},
		{MessageId: "unsupported-event-type", Body: string(valid), MessageAttributes: map[string]events.SQSMessageAttribute{
			"HakoEventType": {DataType: "String", StringValue: stringPointer("operation.requested")},
		}},
	}})
	if err != nil || len(response.BatchItemFailures) != 2 || response.BatchItemFailures[0].ItemIdentifier != "missing-event-type" || response.BatchItemFailures[1].ItemIdentifier != "unsupported-event-type" {
		t.Fatalf("partial batch response = %+v, err=%v", response, err)
	}
	if len(api.inputs) != 1 || agent.starts != 1 {
		t.Fatalf("only the trusted dispatch record should launch once: launches=%d starts=%d", len(api.inputs), agent.starts)
	}
}

func TestLambdaHandlerRequiresControllerContextAndMessageIDs(t *testing.T) {
	if _, err := NewLambdaHandler(nil); err == nil {
		t.Fatal("nil controller was accepted")
	}
	controller, _, _, _, _ := setupLambda(t)
	handler, err := NewLambdaHandler(controller)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handler.Handle(nil, events.SQSEvent{}); err == nil {
		t.Fatal("nil context was accepted")
	}
	response, err := handler.Handle(context.Background(), events.SQSEvent{Records: []events.SQSMessage{{Body: "{}"}}})
	if err == nil || len(response.BatchItemFailures) != 0 {
		t.Fatalf("message without AWS message ID must fail invocation safely: response=%+v err=%v", response, err)
	}
}

func TestLambdaHandlerDoesNotTrustEventTypeFromBody(t *testing.T) {
	controller, _, api, _, _ := setupLambda(t)
	handler, err := NewLambdaHandler(controller)
	if err != nil {
		t.Fatal(err)
	}
	command := map[string]any{"schema_version": 1, "tenant_id": "tenant_1", "workload_run_id": "run_1", "attempt_id": "attempt_1",
		"generation": 1, "resource_plane_id": "rp_1", "idempotency_token": "token_1", "event_type": "workload_attempt.dispatch"}
	body, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	response, err := handler.Handle(context.Background(), events.SQSEvent{Records: []events.SQSMessage{{MessageId: "body-type-only", Body: string(body)}}})
	if err != nil || len(response.BatchItemFailures) != 1 || response.BatchItemFailures[0].ItemIdentifier != "body-type-only" || len(api.inputs) != 0 {
		t.Fatalf("body-supplied event type affected dispatch: response=%+v launches=%d err=%v", response, len(api.inputs), err)
	}
}

func stringPointer(value string) *string { return &value }
