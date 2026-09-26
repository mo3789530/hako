package resourcecontroller

import (
	"context"
	"errors"

	"github.com/aws/aws-lambda-go/events"
)

// LambdaHandler adapts the Resource Controller to an SQS-triggered Lambda.
// Individual failures use ReportBatchItemFailures so a result publication
// failure does not cause already-reported records in the same batch to replay.
type LambdaHandler struct {
	controller *Controller
}

func NewLambdaHandler(controller *Controller) (*LambdaHandler, error) {
	if controller == nil {
		return nil, errors.New("Resource Controller is required")
	}
	return &LambdaHandler{controller: controller}, nil
}

func (h *LambdaHandler) Handle(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
	if h == nil || h.controller == nil {
		return events.SQSEventResponse{}, errors.New("Resource Controller Lambda handler is not configured")
	}
	response := events.SQSEventResponse{BatchItemFailures: make([]events.SQSBatchItemFailure, 0)}
	for _, record := range event.Records {
		if err := h.controller.Handle(ctx, []byte(record.Body)); err != nil {
			response.BatchItemFailures = append(response.BatchItemFailures, events.SQSBatchItemFailure{ItemIdentifier: record.MessageId})
		}
	}
	return response, nil
}
