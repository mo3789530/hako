package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/aws/aws-lambda-go/events"
)

type commandEventHandler func(context.Context, events.SQSEvent) (events.SQSEventResponse, error)
type recoveryEventHandler func(context.Context, events.CloudWatchEvent) (any, error)
type artifactCleanupEventHandler func(context.Context) (any, error)

// newEventMux lets the dedicated command queue and bounded recovery schedule
// share one Lambda binary and role. It accepts only the two AWS event shapes
// configured by this deployment; ambiguous or unknown event types fail closed.
func newEventMux(command commandEventHandler, recovery recoveryEventHandler, cleanup ...artifactCleanupEventHandler) func(context.Context, json.RawMessage) (any, error) {
	return func(ctx context.Context, payload json.RawMessage) (any, error) {
		if ctx == nil || len(payload) == 0 || !json.Valid(payload) || command == nil || recovery == nil || len(cleanup) > 1 {
			return nil, errors.New("invalid Workload Lambda event")
		}
		var envelope struct {
			Records  json.RawMessage `json:"Records"`
			Source   string          `json:"source"`
			HakoTask string          `json:"hako_task"`
		}
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return nil, errors.New("invalid Workload Lambda event")
		}
		hasRecords := len(envelope.Records) > 0 && string(envelope.Records) != "null"
		if hasRecords && envelope.Source == "" {
			var event events.SQSEvent
			if err := json.Unmarshal(payload, &event); err != nil || event.Records == nil {
				return nil, errors.New("invalid Workload command event")
			}
			return command(ctx, event)
		}
		if !hasRecords && envelope.Source == "aws.events" {
			if envelope.HakoTask != "" {
				if envelope.HakoTask != "artifact-cleanup" || len(cleanup) != 1 || cleanup[0] == nil {
					return nil, errors.New("unsupported scheduled Workload task")
				}
				return cleanup[0](ctx)
			}
			var event events.CloudWatchEvent
			if err := json.Unmarshal(payload, &event); err != nil || event.Source != "aws.events" || event.DetailType != "Scheduled Event" {
				return nil, errors.New("invalid Workload recovery event")
			}
			return recovery(ctx, event)
		}
		return nil, errors.New("unsupported Workload Lambda event")
	}
}
