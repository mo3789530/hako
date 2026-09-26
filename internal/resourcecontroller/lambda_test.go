package resourcecontroller

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/mo3789530/hako/internal/domain"
)

func TestLambdaHandlerReturnsOnlyFailedSQSMessageIDs(t *testing.T) {
	runtime := &testRuntime{}
	sink := &testSink{}
	controller, err := New("rp_1", runtime, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLambdaHandler(controller)
	if err != nil {
		t.Fatal(err)
	}
	valid := string(encodedCommand(t, "rp_1"))
	response, err := handler.Handle(context.Background(), events.SQSEvent{Records: []events.SQSMessage{
		{MessageId: "msg-ok", Body: valid},
		{MessageId: "msg-poison", Body: "not-json"},
		{MessageId: "msg-other-plane", Body: `{"schema_version":1,"operation_id":"op_2","tenant_id":"tenant_1","workspace_id":"ws_2","resource_plane_id":"rp_other","type":"ensure_running","created_at":"2026-09-25T00:00:00Z"}`},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.BatchItemFailures) != 2 || response.BatchItemFailures[0].ItemIdentifier != "msg-poison" || response.BatchItemFailures[1].ItemIdentifier != "msg-other-plane" {
		t.Fatalf("partial batch response = %+v", response)
	}
	if len(sink.results) != 1 || sink.results[0].OperationID != domain.OperationID("op_1") {
		t.Fatalf("successful message should be reported exactly once: %+v", sink.results)
	}
}

func TestLambdaHandlerRetriesResultSinkFailures(t *testing.T) {
	runtime := &testRuntime{}
	sink := &testSink{err: errors.New("SQS unavailable")}
	controller, err := New("rp_1", runtime, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLambdaHandler(controller)
	if err != nil {
		t.Fatal(err)
	}
	response, err := handler.Handle(context.Background(), events.SQSEvent{Records: []events.SQSMessage{{MessageId: "msg-result-failed", Body: string(encodedCommand(t, "rp_1"))}}})
	if err != nil || len(response.BatchItemFailures) != 1 || response.BatchItemFailures[0].ItemIdentifier != "msg-result-failed" {
		t.Fatalf("result sink failure must return its SQS message ID for retry: response=%+v error=%v", response, err)
	}
	if _, err := NewLambdaHandler(nil); err == nil {
		t.Fatal("nil controller should fail")
	}
}
