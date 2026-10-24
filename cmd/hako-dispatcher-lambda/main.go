package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/actionsrunner"
	"github.com/mo3789530/hako/internal/dispatcher"
	"github.com/mo3789530/hako/internal/resourceplane"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/outbox"
	"github.com/mo3789530/hako/internal/store/transaction"
)

func main() {
	ctx := context.Background()
	handler, err := buildHandler(ctx)
	if err != nil {
		log.Fatalf("configure Hako Outbox Dispatcher Lambda: %v", err)
	}
	lambda.Start(handler)
}

type runOnce func(context.Context) (dispatcher.Stats, error)

type pendingMetricsRead func(context.Context, time.Time) (outbox.PendingMetrics, error)

type runnerDispatchOnce func(context.Context) (actionsrunner.RunnerStartupDispatchStats, error)

func runWithActionsRunner(outboxRun runOnce, runnerRun runnerDispatchOnce) runOnce {
	return func(ctx context.Context) (dispatcher.Stats, error) {
		stats, err := outboxRun(ctx)
		if err != nil {
			return stats, err
		}
		if runnerRun == nil {
			return stats, nil
		}
		runnerStats, err := runnerRun(ctx)
		log.Printf("Actions runner dispatch complete (claimed=%d published=%d failed=%d)", runnerStats.Claimed, runnerStats.Published, runnerStats.Failed)
		if err != nil {
			return stats, fmt.Errorf("dispatch Actions runner startup jobs: %w", err)
		}
		return stats, nil
	}
}

func handlerFor(run runOnce, readPendingMetrics ...pendingMetricsRead) func(context.Context, events.CloudWatchEvent) error {
	return func(ctx context.Context, _ events.CloudWatchEvent) error {
		stats, err := run(ctx)
		if len(readPendingMetrics) > 0 && readPendingMetrics[0] != nil {
			metrics, metricErr := readPendingMetrics[0](ctx, time.Now().UTC())
			if metricErr != nil {
				log.Print("Outbox lag metric unavailable")
			} else if payload, marshalErr := marshalOutboxMetric(metrics, time.Now().UTC()); marshalErr == nil {
				log.Print(string(payload))
			}
		}
		if err != nil {
			log.Printf("Outbox dispatch batch failed (failure_class=dispatch_batch claimed=%d published=%d failed=%d)", stats.Claimed, stats.Published, stats.Failed)
			return errors.New("Outbox dispatch batch failed")
		}
		log.Printf("Outbox batch complete (claimed=%d published=%d failed=%d)", stats.Claimed, stats.Published, stats.Failed)
		return nil
	}
}

func marshalOutboxMetric(metrics outbox.PendingMetrics, timestamp time.Time) ([]byte, error) {
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}
	var present float64
	if metrics.HasUnpublished {
		present = 1
	}
	return json.Marshal(map[string]any{
		"_aws": map[string]any{
			"Timestamp": timestamp.UnixMilli(),
			"CloudWatchMetrics": []any{map[string]any{
				"Namespace":  "Hako/ControlPlane",
				"Dimensions": [][]string{{"Service"}},
				"Metrics": []any{
					map[string]any{"Name": "OutboxOldestUnpublishedAgeSeconds", "Unit": "Seconds"},
					map[string]any{"Name": "OutboxHasUnpublishedEvents", "Unit": "Count"},
				},
			}},
		},
		"Service":                           "OutboxDispatcher",
		"OutboxOldestUnpublishedAgeSeconds": metrics.OldestUnpublishedAgeSeconds,
		"OutboxHasUnpublishedEvents":        present,
	})
}

func buildHandler(ctx context.Context) (func(context.Context, events.CloudWatchEvent) error, error) {
	pool, err := openDatabase(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect Hako dispatcher database: %w", err)
	}
	queues, err := configuredQueues()
	if err != nil {
		pool.Close()
		return nil, err
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("load AWS SDK configuration: %w", err)
	}
	queueURLs := make(map[string]string, len(queues))
	workloadQueueURLs := make(map[string]string, len(queues))
	regionalClients := make(map[string]dispatcher.SQSAPI, len(queues))
	for resourcePlaneID, queue := range queues {
		queueURLs[resourcePlaneID] = queue.URL
		if queue.WorkloadURL != "" {
			workloadQueueURLs[resourcePlaneID] = queue.WorkloadURL
		}
		if queue.Region != "" {
			regionalConfig := awsCfg
			regionalConfig.Region = queue.Region
			regionalClients[queue.URL] = sqs.NewFromConfig(regionalConfig)
			if queue.WorkloadURL != "" {
				regionalClients[queue.WorkloadURL] = sqs.NewFromConfig(regionalConfig)
			}
		}
	}
	publisher, err := dispatcher.NewRegionalSQSPublisher(sqs.NewFromConfig(awsCfg), regionalClients)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("configure SQS publisher: %w", err)
	}
	engine, err := dispatcher.New(dispatcher.StoreAdapter{Pool: pool, Policy: transaction.DefaultPolicy()}, publisher, queueURLs, dispatcher.Config{
		BatchSize: 10, LeaseDuration: 2 * time.Minute, BaseBackoff: time.Second, MaxBackoff: time.Minute,
		WorkloadSchedulerQueueURL: strings.TrimSpace(os.Getenv("HAKO_WORKLOAD_SCHEDULER_QUEUE_URL")),
		WorkloadCommandQueueURLs:  workloadQueueURLs,
		GitHubChecksQueueURL:      strings.TrimSpace(os.Getenv("HAKO_GITHUB_CHECKS_QUEUE_URL")),
	})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("configure Outbox Dispatcher: %w", err)
	}
	run := runWithActionsRunner(engine.RunOnce, nil)
	runnerQueue, hasRunnerQueue, err := configuredActionsRunnerQueue()
	if err != nil {
		pool.Close()
		return nil, err
	}
	if hasRunnerQueue {
		runnerAWSConfig := awsCfg
		runnerAWSConfig.Region = runnerQueue.Region
		runnerPublisher, err := actionsrunner.NewRunnerStartupSQSPublisher(sqs.NewFromConfig(runnerAWSConfig), runnerQueue.ActionsRunnerStartupQueueURL)
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("configure Actions runner startup SQS publisher: %w", err)
		}
		runnerDispatcher, err := actionsrunner.NewRunnerStartupDispatcher(githubwebhook.ActionsRunnerJobClaimer{Pool: pool, Policy: transaction.DefaultPolicy()},
			runnerPublisher, actionsrunner.RunnerStartupDispatcherConfig{BatchSize: 10, LeaseDuration: 10 * time.Minute})
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("configure Actions runner startup dispatcher: %w", err)
		}
		run = runWithActionsRunner(engine.RunOnce, runnerDispatcher.DispatchOnce)
	}
	store := dispatcher.StoreAdapter{Pool: pool, Policy: transaction.DefaultPolicy()}
	return handlerFor(run, store.ReadPendingMetrics), nil
}

func configuredActionsRunnerQueue() (resourceplane.Registration, bool, error) {
	manifestJSON := strings.TrimSpace(os.Getenv("HAKO_RESOURCE_PLANE_MANIFEST_JSON"))
	if manifestJSON == "" {
		return resourceplane.Registration{}, false, errors.New("HAKO_RESOURCE_PLANE_MANIFEST_JSON is required")
	}
	manifest, err := resourceplane.Parse([]byte(manifestJSON))
	if err != nil {
		return resourceplane.Registration{}, false, fmt.Errorf("parse Resource Plane manifest for Actions runner queue: %w", err)
	}
	return manifest.ActionsRunnerStartupQueue()
}

type queueConfiguration struct {
	URL         string
	WorkloadURL string
	Region      string
}

func configuredQueues() (map[string]queueConfiguration, error) {
	manifestJSON := strings.TrimSpace(os.Getenv("HAKO_RESOURCE_PLANE_MANIFEST_JSON"))
	if manifestJSON == "" {
		return nil, errors.New("HAKO_RESOURCE_PLANE_MANIFEST_JSON is required")
	}
	manifest, err := resourceplane.Parse([]byte(manifestJSON))
	if err != nil {
		return nil, fmt.Errorf("parse Resource Plane manifest: %w", err)
	}
	queues := make(map[string]queueConfiguration, len(manifest.ResourcePlanes))
	for _, registration := range manifest.ResourcePlanes {
		queues[registration.ID] = queueConfiguration{URL: registration.CommandQueueURL, WorkloadURL: registration.WorkloadCommandQueueURL, Region: registration.Region}
	}
	return queues, nil
}

func openDatabase(ctx context.Context) (*pgxpool.Pool, error) {
	postgresURL := strings.TrimSpace(os.Getenv("HAKO_DATABASE_URL"))
	dsqlHost := strings.TrimSpace(os.Getenv("HAKO_DSQL_HOST"))
	if postgresURL != "" && dsqlHost != "" {
		return nil, errors.New("set either HAKO_DATABASE_URL or HAKO_DSQL_HOST, not both")
	}
	if postgresURL != "" {
		return dsql.OpenPostgres(ctx, postgresURL)
	}
	config := dsql.Config{Host: dsqlHost, Region: os.Getenv("AWS_REGION"), User: strings.TrimSpace(os.Getenv("HAKO_DSQL_USER")), Database: os.Getenv("HAKO_DSQL_DATABASE")}
	if config.User == "" {
		config.User = "admin"
	}
	return dsql.Open(ctx, config)
}
