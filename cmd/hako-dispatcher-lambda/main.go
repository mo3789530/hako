package main

import (
	"context"
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
	"github.com/mo3789530/hako/internal/dispatcher"
	"github.com/mo3789530/hako/internal/resourceplane"
	"github.com/mo3789530/hako/internal/store/dsql"
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

func handlerFor(run runOnce) func(context.Context, events.CloudWatchEvent) error {
	return func(ctx context.Context, _ events.CloudWatchEvent) error {
		stats, err := run(ctx)
		if err != nil {
			return fmt.Errorf("dispatch Outbox batch (claimed=%d published=%d failed=%d): %w", stats.Claimed, stats.Published, stats.Failed, err)
		}
		log.Printf("Outbox batch complete (claimed=%d published=%d failed=%d)", stats.Claimed, stats.Published, stats.Failed)
		return nil
	}
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
	regionalClients := make(map[string]dispatcher.SQSAPI, len(queues))
	for resourcePlaneID, queue := range queues {
		queueURLs[resourcePlaneID] = queue.URL
		if queue.Region != "" {
			regionalConfig := awsCfg
			regionalConfig.Region = queue.Region
			regionalClients[queue.URL] = sqs.NewFromConfig(regionalConfig)
		}
	}
	publisher, err := dispatcher.NewRegionalSQSPublisher(sqs.NewFromConfig(awsCfg), regionalClients)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("configure SQS publisher: %w", err)
	}
	engine, err := dispatcher.New(dispatcher.StoreAdapter{Pool: pool, Policy: transaction.DefaultPolicy()}, publisher, queueURLs, dispatcher.Config{
		BatchSize: 10, LeaseDuration: 2 * time.Minute, BaseBackoff: time.Second, MaxBackoff: time.Minute,
	})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("configure Outbox Dispatcher: %w", err)
	}
	return handlerFor(engine.RunOnce), nil
}

type queueConfiguration struct {
	URL    string
	Region string
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
		queues[registration.ID] = queueConfiguration{URL: registration.CommandQueueURL, Region: registration.Region}
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
