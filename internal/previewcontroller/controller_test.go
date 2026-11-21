package previewcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/mo3789530/hako/internal/previewprotocol"
)

func TestControllerProvisionsAndReportsFencedBinding(t *testing.T) {
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	runtime := &fakeRuntime{binding: Binding{MicroVMID: "microvm-1", GatewayRouteID: "route-1"}}
	sink := &fakeSink{}
	controller := newController(t, now, runtime, sink)
	command := validCommand(now, previewprotocol.ActionProvision)
	if err := controller.Handle(context.Background(), encodeCommand(t, command), previewprotocol.EventType(command.Action), command.RuntimeWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if runtime.ensureCalls != 1 || runtime.cleanupCalls != 0 || runtime.compensateCalls != 0 || len(sink.results) != 1 {
		t.Fatalf("unexpected provision handling: runtime=%+v results=%+v", runtime, sink.results)
	}
	result := sink.results[0]
	if result.Status != "succeeded" || result.Binding == nil || *result.Binding != runtime.binding ||
		result.ClaimID != command.ClaimID || result.ClaimAttempt != command.ClaimAttempt || result.Generation != command.Generation ||
		result.ResourcePlaneID != command.ResourcePlaneID || !result.CompletedAt.Equal(now) {
		t.Fatalf("result lost claim/generation fence: %+v", result)
	}
}

func TestControllerCompensatesFailedProvisionBeforeReportingFixedError(t *testing.T) {
	now := time.Now().UTC()
	runtime := &fakeRuntime{binding: Binding{MicroVMID: "orphan-1"}, ensureErr: errors.New("sensitive provider details")}
	sink := &fakeSink{}
	controller := newController(t, now, runtime, sink)
	command := validCommand(now, previewprotocol.ActionProvision)
	if err := controller.Handle(context.Background(), encodeCommand(t, command), previewprotocol.EventType(command.Action), command.RuntimeWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if runtime.compensateCalls != 1 || len(sink.results) != 1 || sink.results[0].ErrorCode != "provider_unavailable" || sink.results[0].Status != "failed" {
		t.Fatalf("failed provision was not compensated/reported: runtime=%+v results=%+v", runtime, sink.results)
	}
	if strings.Contains(string(mustMarshal(t, sink.results[0])), "sensitive provider details") {
		t.Fatal("provider error leaked into result")
	}
}

func TestControllerDoesNotReportWhenCompensationFails(t *testing.T) {
	now := time.Now().UTC()
	runtime := &fakeRuntime{ensureErr: errors.New("provider error"), compensateErr: errors.New("cleanup error")}
	sink := &fakeSink{}
	controller := newController(t, now, runtime, sink)
	command := validCommand(now, previewprotocol.ActionProvision)
	err := controller.Handle(context.Background(), encodeCommand(t, command), previewprotocol.EventType(command.Action), command.RuntimeWorkspaceID)
	if !errors.Is(err, ErrCompensationFailed) || len(sink.results) != 0 {
		t.Fatalf("compensation failure must leave command retryable: results=%+v err=%v", sink.results, err)
	}
}

func TestControllerReportsExpiredAndRejectsWrongPlane(t *testing.T) {
	now := time.Now().UTC()
	runtime := &fakeRuntime{}
	sink := &fakeSink{}
	controller := newController(t, now, runtime, sink)
	command := validCommand(now.Add(-2*time.Minute), previewprotocol.ActionProvision)
	command.LeaseExpiresAt = now.Add(-time.Second)
	if err := controller.Handle(context.Background(), encodeCommand(t, command), previewprotocol.EventType(command.Action), command.RuntimeWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if runtime.ensureCalls != 0 || len(sink.results) != 1 || sink.results[0].ErrorCode != "claim_lease_expired" {
		t.Fatalf("expired claim was executed: runtime=%+v results=%+v", runtime, sink.results)
	}
	command = validCommand(now, previewprotocol.ActionCleanup)
	command.ResourcePlaneID = "rp-other"
	err := controller.Handle(context.Background(), encodeCommand(t, command), previewprotocol.EventType(command.Action), command.RuntimeWorkspaceID)
	if err == nil || runtime.cleanupCalls != 0 {
		t.Fatalf("wrong-Plane command was executed: runtime=%+v err=%v", runtime, err)
	}
}

func TestControllerCleanupReportsOnlyAfterRuntimeConfirmation(t *testing.T) {
	now := time.Now().UTC()
	runtime := &fakeRuntime{cleanupErr: errors.New("provider detail")}
	sink := &fakeSink{}
	controller := newController(t, now, runtime, sink)
	command := validCommand(now, previewprotocol.ActionCleanup)
	if err := controller.Handle(context.Background(), encodeCommand(t, command), previewprotocol.EventType(command.Action), command.RuntimeWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if len(sink.results) != 1 || sink.results[0].ErrorCode != "provider_unavailable" || sink.results[0].Binding != nil {
		t.Fatalf("cleanup result was not safely represented: %+v", sink.results)
	}
}

func TestLambdaHandlerValidatesDispatcherAttributesAndReturnsPartialFailures(t *testing.T) {
	now := time.Now().UTC()
	runtime := &fakeRuntime{}
	sink := &fakeSink{}
	controller := newController(t, now, runtime, sink)
	handler, err := NewLambdaHandler(controller)
	if err != nil {
		t.Fatal(err)
	}
	command := validCommand(now, previewprotocol.ActionCleanup)
	body := encodeCommand(t, command)
	goodType, goodAggregate := previewprotocol.EventType(command.Action), command.RuntimeWorkspaceID
	event := events.SQSEvent{Records: []events.SQSMessage{
		{MessageId: "good", Body: string(body), MessageAttributes: map[string]events.SQSMessageAttribute{
			eventTypeAttribute: {StringValue: &goodType}, aggregateIDAttribute: {StringValue: &goodAggregate},
		}},
		{MessageId: "bad", Body: string(body), MessageAttributes: map[string]events.SQSMessageAttribute{
			eventTypeAttribute: {StringValue: &goodType}, aggregateIDAttribute: {StringValue: ptr("prvws_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
		}},
	}}
	response, err := handler.Handle(context.Background(), event)
	if err != nil || len(response.BatchItemFailures) != 1 || response.BatchItemFailures[0].ItemIdentifier != "bad" || runtime.cleanupCalls != 1 || len(sink.results) != 1 {
		t.Fatalf("unexpected SQS handling: response=%+v runtime=%+v results=%+v err=%v", response, runtime, sink.results, err)
	}
}

type fakeRuntime struct {
	binding         Binding
	ensureErr       error
	cleanupErr      error
	compensateErr   error
	ensureCalls     int
	cleanupCalls    int
	compensateCalls int
}

func (r *fakeRuntime) Ensure(context.Context, previewprotocol.Command) (Binding, error) {
	r.ensureCalls++
	return r.binding, r.ensureErr
}
func (r *fakeRuntime) Cleanup(context.Context, previewprotocol.Command) error {
	r.cleanupCalls++
	return r.cleanupErr
}
func (r *fakeRuntime) Compensate(context.Context, previewprotocol.Command, Binding) error {
	r.compensateCalls++
	return r.compensateErr
}

type fakeSink struct {
	results []Result
	err     error
}

func (s *fakeSink) Report(_ context.Context, result Result) error {
	if s.err != nil {
		return s.err
	}
	s.results = append(s.results, result)
	return nil
}

func newController(t *testing.T, now time.Time, runtime Runtime, sink ResultSink) *Controller {
	t.Helper()
	controller, err := New("rp-test", runtime, sink, func() time.Time { return now }, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func validCommand(now time.Time, action string) previewprotocol.Command {
	command := previewprotocol.Command{
		SchemaVersion: 1, Action: action, TenantID: "tenant_1", InstallationID: 17, RepositoryID: 23,
		PullRequestNumber: 4, Generation: 2, RuntimeWorkspaceID: "prvws_0123456789abcdef0123456789abcdef",
		ResourcePlaneID: "rp-test", ClaimID: "preview-claim-1234", ClaimAttempt: 1,
		LeaseExpiresAt: now.Add(time.Minute), ExpiresAt: now.Add(time.Hour),
	}
	if action == previewprotocol.ActionProvision {
		command.HeadSHA, command.HeadRef = strings.Repeat("a", 40), "feature/preview"
	}
	return command
}

func encodeCommand(t *testing.T, command previewprotocol.Command) []byte {
	t.Helper()
	encoded, err := previewprotocol.Encode(command)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func ptr(value string) *string { return &value }
