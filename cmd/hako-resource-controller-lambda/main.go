package main

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/previewcontroller"
	"github.com/mo3789530/hako/internal/resourcecontroller"
)

func main() {
	resourcePlaneID := domain.ResourcePlaneID(strings.TrimSpace(os.Getenv("HAKO_RESOURCE_PLANE_ID")))
	resultQueueURL := strings.TrimSpace(os.Getenv("HAKO_OPERATION_RESULT_QUEUE_URL"))
	if resourcePlaneID == "" || resultQueueURL == "" {
		log.Fatal("HAKO_RESOURCE_PLANE_ID and HAKO_OPERATION_RESULT_QUEUE_URL are required")
	}
	executionTimeout, err := parseExecutionTimeout(os.Getenv("HAKO_OPERATION_TIMEOUT"))
	if err != nil {
		log.Fatal("invalid operation timeout (failure_class=configuration)")
	}
	maxCommandAge, err := parseMaxCommandAge(os.Getenv("HAKO_COMMAND_MAX_AGE"))
	if err != nil {
		log.Fatal("invalid command age (failure_class=configuration)")
	}
	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatal("load AWS SDK configuration failed (failure_class=aws_configuration)")
	}
	runtime, err := configuredRuntime(os.Getenv("HAKO_RUNTIME_IMPLEMENTATION"), awsCfg, os.Getenv)
	if err != nil {
		log.Fatal("configure Resource Controller runtime failed (failure_class=runtime_configuration)")
	}
	sink, err := resourcecontroller.NewSQSResultSink(sqs.NewFromConfig(awsCfg), resultQueueURL)
	if err != nil {
		log.Fatal("configure Operation result queue failed (failure_class=queue_configuration)")
	}
	controller, err := resourcecontroller.NewWithExecutionTimeoutAndMaxCommandAge(resourcePlaneID, runtime, sink, nil, executionTimeout, maxCommandAge)
	if err != nil {
		log.Fatal("configure Resource Controller failed (failure_class=controller_configuration)")
	}
	handler, err := resourcecontroller.NewLambdaHandler(controller)
	if err != nil {
		log.Fatal("configure SQS Lambda handler failed (failure_class=handler_configuration)")
	}
	var previewHandler sqsBatchHandler
	previewQueueARN := strings.TrimSpace(os.Getenv("HAKO_PREVIEW_RUNTIME_COMMAND_QUEUE_ARN"))
	previewResultURL := strings.TrimSpace(os.Getenv("HAKO_PREVIEW_RUNTIME_RESULT_QUEUE_URL"))
	if (previewQueueARN == "") != (previewResultURL == "") {
		log.Fatal("Preview command queue ARN and result queue URL must be configured together (failure_class=configuration)")
	}
	if previewQueueARN != "" {
		previewRuntime, ok := runtime.(previewcontroller.WorkspaceRuntime)
		if !ok {
			log.Fatal("Preview runtime requires the durable Workspace runtime binding reporter (failure_class=runtime_configuration)")
		}
		previewSourceConfig, err := configuredPreviewSource(os.Getenv)
		if err != nil {
			log.Fatal("configure Preview source dependencies failed (failure_class=source_configuration)")
		}
		materializer, installer, err := composePreviewSource(ctx, awsCfg, resourcePlaneID, previewSourceConfig)
		if err != nil {
			log.Fatal("compose Preview source dependencies failed (failure_class=source_configuration)")
		}
		adapter, err := previewcontroller.NewWorkspaceRuntimeAdapterWithSource(resourcePlaneID, previewRuntime, materializer, installer, nil)
		if err != nil {
			log.Fatal("configure Preview Workspace runtime source adapter failed (failure_class=runtime_configuration)")
		}
		previewSink, err := previewcontroller.NewSQSResultSink(sqs.NewFromConfig(awsCfg), previewResultURL)
		if err != nil {
			log.Fatal("configure Preview result queue failed (failure_class=queue_configuration)")
		}
		preview, err := previewcontroller.New(string(resourcePlaneID), adapter, previewSink, nil, executionTimeout)
		if err != nil {
			log.Fatal("configure Preview controller failed (failure_class=controller_configuration)")
		}
		previewHandler, err = previewcontroller.NewLambdaHandler(preview)
		if err != nil {
			log.Fatal("configure Preview command handler failed (failure_class=handler_configuration)")
		}
	}
	lambda.Start(newSQSHandler(handler, previewHandler, previewQueueARN).Handle)
}

type sqsBatchHandler interface {
	Handle(context.Context, events.SQSEvent) (events.SQSEventResponse, error)
}

type resourceQueueMux struct {
	operations      sqsBatchHandler
	previews        sqsBatchHandler
	previewQueueARN string
}

func newSQSHandler(operations, previews sqsBatchHandler, previewQueueARN string) *resourceQueueMux {
	return &resourceQueueMux{operations: operations, previews: previews, previewQueueARN: strings.TrimSpace(previewQueueARN)}
}

// Handle routes each record by its AWS source ARN, not by untrusted body
// fields. A configured Preview queue fails closed if it is misrouted or its
// handler is absent; it can never fall through to the Workspace operation
// parser.
func (m *resourceQueueMux) Handle(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
	if m == nil || m.operations == nil {
		return events.SQSEventResponse{}, errors.New("Resource Plane queue handlers are not configured")
	}
	var operationRecords, previewRecords []events.SQSMessage
	var failures []events.SQSBatchItemFailure
	for _, record := range event.Records {
		if m.previewQueueARN != "" && record.EventSourceARN == m.previewQueueARN {
			if m.previews == nil {
				failures = append(failures, events.SQSBatchItemFailure{ItemIdentifier: record.MessageId})
			} else {
				previewRecords = append(previewRecords, record)
			}
			continue
		}
		// Preview envelopes must only enter through the exact configured queue.
		if record.EventSourceARN == "" || record.MessageAttributes["HakoEventType"].StringValue != nil &&
			strings.HasPrefix(*record.MessageAttributes["HakoEventType"].StringValue, "repository_preview_runtime.") {
			failures = append(failures, events.SQSBatchItemFailure{ItemIdentifier: record.MessageId})
			continue
		}
		operationRecords = append(operationRecords, record)
	}
	for _, dispatch := range []struct {
		handler sqsBatchHandler
		records []events.SQSMessage
	}{{m.operations, operationRecords}, {m.previews, previewRecords}} {
		if len(dispatch.records) == 0 {
			continue
		}
		if dispatch.handler == nil {
			for _, record := range dispatch.records {
				failures = append(failures, events.SQSBatchItemFailure{ItemIdentifier: record.MessageId})
			}
			continue
		}
		response, err := dispatch.handler.Handle(ctx, events.SQSEvent{Records: dispatch.records})
		if err != nil {
			return events.SQSEventResponse{}, err
		}
		failures = append(failures, response.BatchItemFailures...)
	}
	return events.SQSEventResponse{BatchItemFailures: failures}, nil
}

func parseMaxCommandAge(raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return 30 * time.Minute, nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || parsed <= 0 {
		return 0, errors.New("HAKO_COMMAND_MAX_AGE must be a positive Go duration, such as 30m")
	}
	return parsed, nil
}

func parseExecutionTimeout(raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return 45 * time.Second, nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || parsed <= 0 {
		return 0, errors.New("HAKO_OPERATION_TIMEOUT must be a positive Go duration, such as 45s")
	}
	return parsed, nil
}
