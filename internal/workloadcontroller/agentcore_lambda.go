package workloadcontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"

	"github.com/aws/aws-lambda-go/events"
	"github.com/mo3789530/hako/internal/workloadagentcore"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

const (
	maxAgentCoreSQSRecords = 10
	maxAgentCoreSQSBody    = 4 << 10
)

// AgentCoreSQSWorker is the durable worker contract consumed by the SQS
// adapter. Queue messages identify an attempt; the worker resolves all task,
// source, Runtime and session inputs from trusted durable state.
type AgentCoreSQSWorker interface {
	Run(context.Context, workloadjournal.Command) (workloadjournal.Snapshot, error)
}

type AgentCoreLambdaHandler struct {
	worker AgentCoreSQSWorker
	logger *slog.Logger
}

func NewAgentCoreLambdaHandler(worker AgentCoreSQSWorker) (*AgentCoreLambdaHandler, error) {
	if worker == nil {
		return nil, errors.New("AgentCore Workload worker is required")
	}
	return &AgentCoreLambdaHandler{worker: worker, logger: slog.Default()}, nil
}

// Handle applies AWS Lambda's ReportBatchItemFailures contract so a transient
// attempt failure does not replay already checkpointed messages in the batch.
func (h *AgentCoreLambdaHandler) Handle(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
	if h == nil || h.worker == nil || ctx == nil {
		return events.SQSEventResponse{}, errors.New("AgentCore SQS handler is not configured")
	}
	if len(event.Records) > maxAgentCoreSQSRecords {
		return events.SQSEventResponse{}, errors.New("AgentCore SQS event exceeds its record limit")
	}
	response := events.SQSEventResponse{BatchItemFailures: make([]events.SQSBatchItemFailure, 0)}
	for _, record := range event.Records {
		if record.MessageId == "" {
			return events.SQSEventResponse{}, errors.New("AgentCore SQS record has no message ID")
		}
		command, err := decodeAgentCoreSQSCommand([]byte(record.Body))
		if err == nil {
			_, err = h.worker.Run(ctx, command)
		}
		if err != nil {
			logger := h.logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.ErrorContext(ctx, "AgentCore Workload record processing failed",
				"sqs_message_id", record.MessageId, "failure_class", "agentcore_record_processing")
			response.BatchItemFailures = append(response.BatchItemFailures,
				events.SQSBatchItemFailure{ItemIdentifier: record.MessageId})
		}
	}
	return response, nil
}

type agentCoreSQSCommand struct {
	TenantID         string `json:"tenant_id"`
	WorkloadRunID    string `json:"workload_run_id"`
	AttemptID        string `json:"attempt_id"`
	Generation       int64  `json:"generation"`
	ResourcePlaneID  string `json:"resource_plane_id"`
	IdempotencyToken string `json:"idempotency_token"`
	RuntimeProvider  string `json:"runtime_provider"`
}

func decodeAgentCoreSQSCommand(body []byte) (workloadjournal.Command, error) {
	if len(body) == 0 || len(body) > maxAgentCoreSQSBody {
		return workloadjournal.Command{}, errors.New("invalid AgentCore SQS command")
	}
	// Inspect the top-level object first so duplicate fields cannot change the
	// identity selected by the typed decoder.
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return workloadjournal.Command{}, errors.New("invalid AgentCore SQS command")
	}
	fields := make(map[string]json.RawMessage, 7)
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok {
			return workloadjournal.Command{}, errors.New("invalid AgentCore SQS command")
		}
		if _, duplicate := fields[key]; duplicate {
			return workloadjournal.Command{}, errors.New("invalid AgentCore SQS command")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return workloadjournal.Command{}, errors.New("invalid AgentCore SQS command")
		}
		fields[key] = value
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return workloadjournal.Command{}, errors.New("invalid AgentCore SQS command")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return workloadjournal.Command{}, errors.New("invalid AgentCore SQS command")
	}
	if len(fields) != 7 {
		return workloadjournal.Command{}, errors.New("invalid AgentCore SQS command")
	}
	canonical, err := json.Marshal(fields)
	if err != nil {
		return workloadjournal.Command{}, errors.New("invalid AgentCore SQS command")
	}
	var wire agentCoreSQSCommand
	typed := json.NewDecoder(bytes.NewReader(canonical))
	typed.DisallowUnknownFields()
	if typed.Decode(&wire) != nil || typed.Decode(new(any)) != io.EOF || wire.RuntimeProvider != "agentcore" {
		return workloadjournal.Command{}, errors.New("invalid AgentCore SQS command")
	}
	command := workloadjournal.Command{TenantID: wire.TenantID, WorkloadRunID: wire.WorkloadRunID,
		AttemptID: wire.AttemptID, Generation: wire.Generation, ResourcePlaneID: wire.ResourcePlaneID,
		IdempotencyToken: wire.IdempotencyToken}
	if _, err := workloadagentcore.SessionID(command); err != nil {
		return workloadjournal.Command{}, errors.New("invalid AgentCore SQS command")
	}
	return command, nil
}
