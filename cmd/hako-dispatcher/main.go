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
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/dispatcher"
	"github.com/mo3789530/hako/internal/resourceplane"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := openDatabase(ctx)
	if err != nil {
		log.Fatalf("connect Hako dispatcher database: %v", err)
	}
	defer pool.Close()

	queues, err := configuredQueues()
	if err != nil {
		log.Fatalf("configure Resource Plane queues: %v", err)
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("load AWS SDK configuration: %v", err)
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
		log.Fatalf("configure SQS publisher: %v", err)
	}
	engine, err := dispatcher.New(dispatcher.StoreAdapter{Pool: pool, Policy: transaction.DefaultPolicy()}, publisher, queueURLs, dispatcher.Config{
		BatchSize: 10, LeaseDuration: 30 * time.Second, BaseBackoff: time.Second, MaxBackoff: time.Minute,
	})
	if err != nil {
		log.Fatalf("configure Outbox dispatcher: %v", err)
	}
	interval := durationEnv("HAKO_DISPATCHER_INTERVAL", 2*time.Second)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.Printf("Hako Outbox Dispatcher started (poll interval %s)", interval)
	for {
		stats, err := engine.RunOnce(ctx)
		if err != nil {
			log.Printf("dispatch batch: %v (claimed=%d published=%d failed=%d)", err, stats.Claimed, stats.Published, stats.Failed)
		} else if stats.Claimed > 0 {
			log.Printf("dispatch batch complete (claimed=%d published=%d)", stats.Claimed, stats.Published)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type queueConfiguration struct {
	URL    string
	Region string
}

func configuredQueueURLs() (map[string]string, error) {
	queues, err := configuredQueues()
	if err != nil {
		return nil, err
	}
	queueURLs := make(map[string]string, len(queues))
	for resourcePlaneID, queue := range queues {
		queueURLs[resourcePlaneID] = queue.URL
	}
	return queueURLs, nil
}

func configuredQueues() (map[string]queueConfiguration, error) {
	manifestPath := strings.TrimSpace(os.Getenv("HAKO_RESOURCE_PLANE_MANIFEST"))
	manifestJSON := strings.TrimSpace(os.Getenv("HAKO_RESOURCE_PLANE_MANIFEST_JSON"))
	raw := strings.TrimSpace(os.Getenv("HAKO_RESOURCE_PLANE_QUEUE_URLS"))
	configured := 0
	for _, value := range []string{manifestPath, manifestJSON, raw} {
		if value != "" {
			configured++
		}
	}
	if configured > 1 {
		return nil, errors.New("set only one of HAKO_RESOURCE_PLANE_MANIFEST, HAKO_RESOURCE_PLANE_MANIFEST_JSON, or HAKO_RESOURCE_PLANE_QUEUE_URLS")
	}
	if manifestPath != "" || manifestJSON != "" {
		var manifest resourceplane.Manifest
		var err error
		if manifestPath != "" {
			manifest, err = resourceplane.LoadFile(manifestPath)
		} else {
			manifest, err = resourceplane.Parse([]byte(manifestJSON))
		}
		if err != nil {
			return nil, fmt.Errorf("load Resource Plane manifest: %w", err)
		}
		queues := make(map[string]queueConfiguration, len(manifest.ResourcePlanes))
		for _, registration := range manifest.ResourcePlanes {
			queues[registration.ID] = queueConfiguration{URL: registration.CommandQueueURL, Region: registration.Region}
		}
		return queues, nil
	}
	if raw == "" {
		return nil, errors.New("HAKO_RESOURCE_PLANE_QUEUE_URLS is required")
	}
	var rawQueueURLs map[string]string
	if err := json.Unmarshal([]byte(raw), &rawQueueURLs); err != nil || len(rawQueueURLs) == 0 {
		return nil, errors.New("HAKO_RESOURCE_PLANE_QUEUE_URLS must be a non-empty JSON object of Resource Plane IDs to SQS queue URLs")
	}
	normalized := make(map[string]queueConfiguration, len(rawQueueURLs))
	for resourcePlaneID, queueURL := range rawQueueURLs {
		resourcePlaneID = strings.TrimSpace(resourcePlaneID)
		queueURL = strings.TrimSpace(queueURL)
		if resourcePlaneID == "" || queueURL == "" {
			return nil, errors.New("HAKO_RESOURCE_PLANE_QUEUE_URLS must not contain empty Resource Plane IDs or queue URLs")
		}
		if _, exists := normalized[resourcePlaneID]; exists {
			return nil, errors.New("HAKO_RESOURCE_PLANE_QUEUE_URLS contains duplicate normalized Resource Plane IDs")
		}
		normalized[resourcePlaneID] = queueConfiguration{URL: queueURL}
	}
	return normalized, nil
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < time.Second {
		log.Fatalf("%s must be a duration of at least 1s", name)
	}
	return duration
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
