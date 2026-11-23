package workloadcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

type agentCoreSQSWorkerFake struct {
	commands []workloadjournal.Command
	failed   map[string]bool
}

func (f *agentCoreSQSWorkerFake) Run(_ context.Context, command workloadjournal.Command) (workloadjournal.Snapshot, error) {
	f.commands = append(f.commands, command)
	if f.failed[command.AttemptID] {
		return workloadjournal.Snapshot{}, errors.New("sensitive provider detail")
	}
	return workloadjournal.Snapshot{}, nil
}

func TestAgentCoreLambdaHandlerReturnsOnlyFailedRecords(t *testing.T) {
	worker := &agentCoreSQSWorkerFake{failed: map[string]bool{"attempt_b": true}}
	handler, err := NewAgentCoreLambdaHandler(worker)
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	handler.logger = slog.New(slog.NewTextHandler(&logs, nil))
	response, err := handler.Handle(context.Background(), events.SQSEvent{Records: []events.SQSMessage{
		{MessageId: "message-a", Body: agentCoreCommandBody(t, "attempt_a")},
		{MessageId: "message-b", Body: agentCoreCommandBody(t, "attempt_b")},
	}})
	if err != nil || len(response.BatchItemFailures) != 1 || response.BatchItemFailures[0].ItemIdentifier != "message-b" || len(worker.commands) != 2 {
		t.Fatalf("partial response=%+v commands=%+v err=%v", response, worker.commands, err)
	}
	if strings.Contains(logs.String(), "sensitive provider detail") || !strings.Contains(logs.String(), "failure_class=agentcore_record_processing") {
		t.Fatalf("unsafe failure log: %s", logs.String())
	}
}

func TestDecodeAgentCoreSQSCommandStrictlyValidatesIdentity(t *testing.T) {
	valid := agentCoreCommandBody(t, "attempt_a")
	command, err := decodeAgentCoreSQSCommand([]byte(valid))
	if err != nil || command.AttemptID != "attempt_a" || command.Generation != 2 {
		t.Fatalf("valid command=%+v err=%v", command, err)
	}
	for name, body := range map[string]string{
		"duplicate": valid[:len(valid)-1] + `,"generation":2}`,
		"unknown":   valid[:len(valid)-1] + `,"runtime_arn":"arn:untrusted"}`,
		"wrong runtime provider": strings.Replace(valid, `"runtime_provider":"agentcore"`, `"runtime_provider":"lambda_microvm"`, 1),
		"trailing":  valid + `{}`,
		"oversized": strings.Repeat("x", maxAgentCoreSQSBody+1),
		"malformed": `{`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeAgentCoreSQSCommand([]byte(body)); err == nil {
				t.Fatal("invalid command accepted")
			}
		})
	}
}

func TestAgentCoreLambdaHandlerRejectsUnidentifiableBatch(t *testing.T) {
	handler, _ := NewAgentCoreLambdaHandler(&agentCoreSQSWorkerFake{})
	if _, err := handler.Handle(context.Background(), events.SQSEvent{Records: []events.SQSMessage{{Body: agentCoreCommandBody(t, "attempt_a")}}}); err == nil {
		t.Fatal("record without message ID was accepted")
	}
	tooMany := make([]events.SQSMessage, maxAgentCoreSQSRecords+1)
	if _, err := handler.Handle(context.Background(), events.SQSEvent{Records: tooMany}); err == nil {
		t.Fatal("oversized SQS batch was accepted")
	}
}

func agentCoreCommandBody(t *testing.T, attempt string) string {
	t.Helper()
	body, err := json.Marshal(agentCoreSQSCommand{TenantID: "tenant_sqs", WorkloadRunID: "run_sqs", AttemptID: attempt,
		Generation: 2, ResourcePlaneID: "rp_sqs", IdempotencyToken: "token_sqs", RuntimeProvider: "agentcore"})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
