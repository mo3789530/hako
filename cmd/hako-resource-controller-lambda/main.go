package main

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/mo3789530/hako/internal/domain"
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
	lambda.Start(handler.Handle)
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
