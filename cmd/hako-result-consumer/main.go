package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/operationresults"
	"github.com/mo3789530/hako/internal/resourceplane"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
)

func main() {
	queueURLs, err := configuredResultQueues()
	if err != nil {
		log.Fatalf("configure Resource Plane result queues: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := openDatabase(ctx)
	if err != nil {
		log.Fatalf("connect Hako result consumer database: %v", err)
	}
	defer pool.Close()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("load AWS SDK configuration: %v", err)
	}
	store := operationresults.StoreAdapter{Pool: pool, Policy: transaction.DefaultPolicy()}
	var workers sync.WaitGroup
	for resourcePlaneID, queue := range queueURLs {
		regionalConfig := awsCfg
		if queue.Region != "" {
			regionalConfig.Region = queue.Region
		}
		client := sqs.NewFromConfig(regionalConfig)
		workers.Add(1)
		go func(resourcePlaneID, queueURL string, client *sqs.Client) {
			defer workers.Done()
			if err := consumeQueue(ctx, client, store, resourcePlaneID, queueURL); err != nil && ctx.Err() == nil {
				log.Printf("result consumer for Resource Plane %s stopped: %v", resourcePlaneID, err)
			}
		}(resourcePlaneID, queue.URL, client)
	}
	log.Printf("Hako Operation result consumer started for %d Resource Plane queue(s)", len(queueURLs))
	<-ctx.Done()
	workers.Wait()
}

func consumeQueue(ctx context.Context, client *sqs.Client, store operationresults.Store, resourcePlaneID, queueURL string) error {
	queue, err := operationresults.NewSQSQueue(client, queueURL)
	if err != nil {
		return fmt.Errorf("configure result queue: %w", err)
	}
	consumer, err := operationresults.New(store, queue, operationresults.Config{BatchSize: 10, WaitTime: 20 * time.Second})
	if err != nil {
		return fmt.Errorf("configure result consumer: %w", err)
	}
	for ctx.Err() == nil {
		stats, err := consumer.RunOnce(ctx)
		if err != nil {
			log.Printf("consume Operation results from %s: %v (received=%d applied=%d duplicate=%d failed=%d)", resourcePlaneID, err, stats.Received, stats.Applied, stats.Duplicate, stats.Failed)
		} else if stats.Received > 0 {
			log.Printf("Operation result batch complete for %s (received=%d applied=%d duplicate=%d)", resourcePlaneID, stats.Received, stats.Applied, stats.Duplicate)
		}
	}
	return ctx.Err()
}

type resultQueueConfiguration struct {
	URL    string
	Region string
}

func configuredResultQueueURLs() (map[string]string, error) {
	queues, err := configuredResultQueues()
	if err != nil {
		return nil, err
	}
	queueURLs := make(map[string]string, len(queues))
	for resourcePlaneID, queue := range queues {
		queueURLs[resourcePlaneID] = queue.URL
	}
	return queueURLs, nil
}

func configuredResultQueues() (map[string]resultQueueConfiguration, error) {
	manifestPath := strings.TrimSpace(os.Getenv("HAKO_RESOURCE_PLANE_MANIFEST"))
	rawMap := strings.TrimSpace(os.Getenv("HAKO_OPERATION_RESULT_QUEUE_URLS"))
	singleURL := strings.TrimSpace(os.Getenv("HAKO_OPERATION_RESULT_QUEUE_URL"))
	configured := 0
	for _, value := range []string{manifestPath, rawMap, singleURL} {
		if value != "" {
			configured++
		}
	}
	if configured > 1 {
		return nil, errors.New("set only one of HAKO_RESOURCE_PLANE_MANIFEST, HAKO_OPERATION_RESULT_QUEUE_URLS, or HAKO_OPERATION_RESULT_QUEUE_URL")
	}
	if manifestPath != "" {
		manifest, err := resourceplane.LoadFile(manifestPath)
		if err != nil {
			return nil, fmt.Errorf("load Resource Plane manifest: %w", err)
		}
		queues := make(map[string]resultQueueConfiguration, len(manifest.ResourcePlanes))
		for _, registration := range manifest.ResourcePlanes {
			queues[registration.ID] = resultQueueConfiguration{URL: registration.ResultQueueURL, Region: registration.Region}
		}
		return queues, nil
	}
	if rawMap != "" {
		var queueURLs map[string]string
		if err := json.Unmarshal([]byte(rawMap), &queueURLs); err != nil || len(queueURLs) == 0 {
			return nil, errors.New("HAKO_OPERATION_RESULT_QUEUE_URLS must be a non-empty JSON object of Resource Plane IDs to SQS queue URLs")
		}
		normalized := make(map[string]resultQueueConfiguration, len(queueURLs))
		for resourcePlaneID, queueURL := range queueURLs {
			resourcePlaneID = strings.TrimSpace(resourcePlaneID)
			queueURL = strings.TrimSpace(queueURL)
			if resourcePlaneID == "" || queueURL == "" {
				return nil, errors.New("HAKO_OPERATION_RESULT_QUEUE_URLS must not contain empty Resource Plane IDs or queue URLs")
			}
			if _, exists := normalized[resourcePlaneID]; exists {
				return nil, errors.New("HAKO_OPERATION_RESULT_QUEUE_URLS contains duplicate normalized Resource Plane IDs")
			}
			normalized[resourcePlaneID] = resultQueueConfiguration{URL: queueURL}
		}
		return normalized, nil
	}
	if singleURL == "" {
		return nil, errors.New("HAKO_RESOURCE_PLANE_MANIFEST, HAKO_OPERATION_RESULT_QUEUE_URLS, or HAKO_OPERATION_RESULT_QUEUE_URL is required")
	}
	return map[string]resultQueueConfiguration{"default": {URL: singleURL}}, nil
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
