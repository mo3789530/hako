package workloadcontroller

import (
	"context"
	"errors"
	"log/slog"

	"github.com/aws/aws-lambda-go/events"
)

// LambdaHandler adapts the Resource Plane workload controller to an SQS event
// source. A record is acknowledged only after the controller has persisted its
// command checkpoint and completed the requested reconcile step.
type LambdaHandler struct {
	controller *Controller
	logger     *slog.Logger
}

func NewLambdaHandler(controller *Controller) (*LambdaHandler, error) {
	if controller == nil {
		return nil, errors.New("Workload Controller is required")
	}
	return &LambdaHandler{controller: controller, logger: slog.Default()}, nil
}

// Handle returns partial batch failures so one transient provider/journal
// failure does not replay unrelated commands already checkpointed in this
// invocation. The event type is taken only from the trusted Dispatcher
// message attribute, never from repository-controlled JSON.
func (h *LambdaHandler) Handle(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
	if h == nil || h.controller == nil || ctx == nil {
		return events.SQSEventResponse{}, errors.New("context and Workload Controller are required")
	}
	response := events.SQSEventResponse{BatchItemFailures: make([]events.SQSBatchItemFailure, 0)}
	for _, record := range event.Records {
		if record.MessageId == "" {
			return events.SQSEventResponse{}, errors.New("SQS Workload record has no message ID")
		}
		eventType := ""
		if attribute, ok := record.MessageAttributes["HakoEventType"]; ok && attribute.DataType == "String" && attribute.StringValue != nil {
			eventType = *attribute.StringValue
		}
		if _, err := h.controller.HandleEvent(ctx, eventType, []byte(record.Body)); err != nil {
			command, decodeErr := Decode([]byte(record.Body))
			logger := h.logger
			if logger == nil {
				logger = slog.Default()
			}
			attrs := []any{"sqs_message_id", record.MessageId, "event_type", eventType, "failure_class", "workload_record_processing"}
			if decodeErr == nil {
				attrs = append(attrs, "correlation_id", command.CorrelationID, "workload_run_id", command.WorkloadRunID,
					"attempt_id", command.AttemptID, "resource_plane_id", command.ResourcePlaneID)
			}
			logger.ErrorContext(ctx, "Workload command processing failed", attrs...)
			response.BatchItemFailures = append(response.BatchItemFailures, events.SQSBatchItemFailure{ItemIdentifier: record.MessageId})
		}
	}
	return response, nil
}
