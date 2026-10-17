package actionsrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/aws/aws-lambda-go/events"
	"github.com/mo3789530/hako/internal/githubbroker"
)

var ErrInvalidRunnerStartupEvent = errors.New("invalid GitHub Actions runner startup event")

type RunnerStartupHandler struct {
	worker *RunnerStartupWorker
}

type runnerStartupMessage struct {
	SchemaVersion int    `json:"schema_version"`
	IntegrationID string `json:"integration_id"`
	WorkflowJobID int64  `json:"workflow_job_id"`
	RunAttempt    int64  `json:"run_attempt"`
	ClaimID       string `json:"claim_id"`
}

func NewRunnerStartupHandler(worker *RunnerStartupWorker) (*RunnerStartupHandler, error) {
	if worker == nil {
		return nil, ErrInvalidRunnerLaunch
	}
	return &RunnerStartupHandler{worker: worker}, nil
}

// Handle processes each SQS record independently and returns partial batch
// failures so a poison message cannot cause successful startup work to replay.
func (h *RunnerStartupHandler) Handle(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
	if h == nil || h.worker == nil || ctx == nil {
		return events.SQSEventResponse{}, ErrInvalidRunnerLaunch
	}
	response := events.SQSEventResponse{BatchItemFailures: make([]events.SQSBatchItemFailure, 0)}
	for _, record := range event.Records {
		request, err := decodeRunnerStartupRequest(record.Body)
		if err == nil {
			err = h.worker.Start(ctx, request)
		}
		if err != nil {
			response.BatchItemFailures = append(response.BatchItemFailures, events.SQSBatchItemFailure{ItemIdentifier: record.MessageId})
		}
	}
	return response, nil
}

func decodeRunnerStartupRequest(body string) (githubbroker.ActionsRunnerRegistrationRequest, error) {
	if len(body) == 0 || len(body) > 2048 {
		return githubbroker.ActionsRunnerRegistrationRequest{}, ErrInvalidRunnerStartupEvent
	}
	decoder := json.NewDecoder(io.LimitReader(bytes.NewBufferString(body), 2049))
	decoder.DisallowUnknownFields()
	var message runnerStartupMessage
	if decoder.Decode(&message) != nil || message.SchemaVersion != 1 {
		return githubbroker.ActionsRunnerRegistrationRequest{}, ErrInvalidRunnerStartupEvent
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return githubbroker.ActionsRunnerRegistrationRequest{}, ErrInvalidRunnerStartupEvent
	}
	request := githubbroker.ActionsRunnerRegistrationRequest{IntegrationID: message.IntegrationID, WorkflowJobID: message.WorkflowJobID,
		RunAttempt: message.RunAttempt, ClaimID: message.ClaimID}
	if !validActionsRunnerBootstrapRequest(request) {
		return githubbroker.ActionsRunnerRegistrationRequest{}, ErrInvalidRunnerStartupEvent
	}
	return request, nil
}
