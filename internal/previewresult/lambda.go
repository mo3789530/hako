package previewresult

import (
	"context"
	"errors"
	"log/slog"

	"github.com/aws/aws-lambda-go/events"
)

type LambdaHandler struct {
	consumer Consumer
	logger   *slog.Logger
}

func NewLambdaHandler(consumer Consumer) (*LambdaHandler, error) {
	if consumer.Pool == nil {
		return nil, errors.New("Preview result consumer database pool is required")
	}
	return &LambdaHandler{consumer: consumer, logger: slog.Default()}, nil
}

func (h *LambdaHandler) Handle(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
	if h == nil || h.consumer.Pool == nil {
		return events.SQSEventResponse{}, errors.New("Preview result Lambda handler is not configured")
	}
	response := events.SQSEventResponse{BatchItemFailures: make([]events.SQSBatchItemFailure, 0)}
	for _, record := range event.Records {
		claimID := stringValueOrEmpty(record.MessageAttributes["HakoPreviewClaimID"])
		action := stringValueOrEmpty(record.MessageAttributes["HakoPreviewAction"])
		planeID := stringValueOrEmpty(record.MessageAttributes["HakoResourcePlaneID"])
		_, err := h.consumer.Handle(ctx, []byte(record.Body), claimID, action, planeID)
		if err != nil {
			logger := h.logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.ErrorContext(ctx, "Preview result processing failed", "sqs_message_id", record.MessageId,
				"failure_class", "preview_result_processing")
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
