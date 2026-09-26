package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	fakeruntime "github.com/mo3789530/hako/internal/runtime/fake"
)

func main() {
	resourcePlaneID := domain.ResourcePlaneID(strings.TrimSpace(os.Getenv("HAKO_RESOURCE_PLANE_ID")))
	commandQueueURL := strings.TrimSpace(os.Getenv("HAKO_RESOURCE_PLANE_QUEUE_URL"))
	resultQueueURL := strings.TrimSpace(os.Getenv("HAKO_OPERATION_RESULT_QUEUE_URL"))
	if resourcePlaneID == "" || commandQueueURL == "" || resultQueueURL == "" {
		log.Fatal("HAKO_RESOURCE_PLANE_ID, HAKO_RESOURCE_PLANE_QUEUE_URL, and HAKO_OPERATION_RESULT_QUEUE_URL are required")
	}
	executionTimeout := 15 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("HAKO_OPERATION_TIMEOUT")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			log.Fatal("HAKO_OPERATION_TIMEOUT must be a positive Go duration, such as 15m")
		}
		executionTimeout = parsed
	}
	maxCommandAge, err := parseCommandMaxAge(os.Getenv("HAKO_COMMAND_MAX_AGE"))
	if err != nil {
		log.Fatal(err)
	}
	visibilityTimeout := 60 * time.Second
	if raw := strings.TrimSpace(os.Getenv("HAKO_SQS_VISIBILITY_TIMEOUT")); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 1 || seconds > 43200 {
			log.Fatal("HAKO_SQS_VISIBILITY_TIMEOUT must be an integer from 1 to 43200 seconds")
		}
		visibilityTimeout = time.Duration(seconds) * time.Second
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("load AWS SDK configuration: %v", err)
	}
	sqsClient := sqs.NewFromConfig(awsCfg)
	queue, err := resourcecontroller.NewSQSQueue(sqsClient, commandQueueURL)
	if err != nil {
		log.Fatalf("configure Resource Plane command queue: %v", err)
	}
	resultSink, err := resourcecontroller.NewSQSResultSink(sqsClient, resultQueueURL)
	if err != nil {
		log.Fatalf("configure Operation result queue: %v", err)
	}
	controller, err := resourcecontroller.NewWithExecutionTimeoutAndMaxCommandAge(resourcePlaneID, fakeruntime.New(), resultSink, nil, executionTimeout, maxCommandAge)
	if err != nil {
		log.Fatalf("configure fake Resource Controller: %v", err)
	}
	worker, err := resourcecontroller.NewWorker(queue, controller, resourcecontroller.WorkerConfig{
		MaxMessages: 10, WaitTime: 20 * time.Second, VisibilityTimeout: visibilityTimeout,
	})
	if err != nil {
		log.Fatalf("configure Resource Plane SQS worker: %v", err)
	}
	log.Printf("Fake Resource Controller started for %s (operation timeout=%s, max command age=%s, SQS visibility=%s)", resourcePlaneID, executionTimeout, maxCommandAge, visibilityTimeout)
	for {
		stats, err := worker.RunOnce(ctx)
		if err != nil {
			log.Printf("process Resource Plane command batch: %v (received=%d handled=%d failed=%d)", err, stats.Received, stats.Handled, stats.Failed)
		} else if stats.Received > 0 {
			log.Printf("processed Resource Plane command batch (received=%d handled=%d)", stats.Received, stats.Handled)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func parseCommandMaxAge(raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return 30 * time.Minute, nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || parsed <= 0 {
		return 0, errors.New("HAKO_COMMAND_MAX_AGE must be a positive Go duration, such as 30m")
	}
	return parsed, nil
}
