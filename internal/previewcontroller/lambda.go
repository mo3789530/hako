package previewcontroller

import (
	"context"
	"errors"
	"log/slog"

	"github.com/aws/aws-lambda-go/events"
)

const (
	eventTypeAttribute   = "HakoEventType"
	aggregateIDAttribute = "AggregateID"
)

type LambdaHandler struct {
	controller *Controller
	logger     *slog.Logger
}

func NewLambdaHandler(controller *Controller) (*LambdaHandler, error) {
	if controller == nil {
		return nil, errors.New("Preview Resource Plane controller is required")
	}
	return &LambdaHandler{controller: controller, logger: slog.Default()}, nil
}

func (h *LambdaHandler) Handle(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
	if h == nil || h.controller == nil {
		return events.SQSEventResponse{}, errors.New("Preview Resource Plane Lambda handler is not configured")
	}
	response := events.SQSEventResponse{BatchItemFailures: make([]events.SQSBatchItemFailure, 0)}
	for _, record := range event.Records {
		eventType := stringValueOrEmpty(record.MessageAttributes[eventTypeAttribute])
		aggregateID := stringValueOrEmpty(record.MessageAttributes[aggregateIDAttribute])
		if err := h.controller.Handle(ctx, []byte(record.Body), eventType, aggregateID); err != nil {
			logger := h.logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.ErrorContext(ctx, "Preview Resource Plane command processing failed",
				"sqs_message_id", record.MessageId, "failure_class", "preview_command_processing")
			response.BatchItemFailures = append(response.BatchItemFailures, events.SQSBatchItemFailure{ItemIdentifier: record.MessageId})
		}
	}
	return response, nil
}

func stringValueOrEmpty(attribute events.SQSMessageAttribute) string {
	if attribute.StringValue == nil {
		return ""
	}
	return *attribute.StringValue
}
