package resourcecontroller

import (
	"context"
	"errors"
	"log/slog"

	"github.com/aws/aws-lambda-go/events"
)

// LambdaHandler adapts the Resource Controller to an SQS-triggered Lambda.
// Individual failures use ReportBatchItemFailures so a result publication
// failure does not cause already-reported records in the same batch to replay.
type LambdaHandler struct {
	controller *Controller
	logger     *slog.Logger
}

func NewLambdaHandler(controller *Controller) (*LambdaHandler, error) {
	if controller == nil {
		return nil, errors.New("Resource Controller is required")
	}
	return &LambdaHandler{controller: controller, logger: slog.Default()}, nil
}

func (h *LambdaHandler) Handle(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
	if h == nil || h.controller == nil {
		return events.SQSEventResponse{}, errors.New("Resource Controller Lambda handler is not configured")
	}
	response := events.SQSEventResponse{BatchItemFailures: make([]events.SQSBatchItemFailure, 0)}
	for _, record := range event.Records {
		if err := h.controller.Handle(ctx, []byte(record.Body)); err != nil {
			command, decodeErr := decodeCommand([]byte(record.Body))
			attrs := []any{"sqs_message_id", record.MessageId, "failure_class", "operation_record_processing"}
			if decodeErr == nil {
				attrs = append(attrs, "operation_id", command.OperationID, "resource_plane_id", command.ResourcePlaneID,
					"workspace_id", command.WorkspaceID)
				if command.CorrelationID != "" {
					attrs = append(attrs, "correlation_id", command.CorrelationID)
				}
			}
			logger := h.logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.ErrorContext(ctx, "Resource Plane Operation processing failed", attrs...)
			response.BatchItemFailures = append(response.BatchItemFailures, events.SQSBatchItemFailure{ItemIdentifier: record.MessageId})
		}
	}
	return response, nil
}
