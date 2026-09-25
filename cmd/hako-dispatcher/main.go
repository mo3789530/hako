package main

import (
	"context"
	"encoding/json"
	"errors"
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

	queueURLs, err := configuredQueueURLs()
	if err != nil {
		log.Fatalf("configure Resource Plane queues: %v", err)
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("load AWS SDK configuration: %v", err)
	}
	publisher, err := dispatcher.NewSQSPublisher(sqs.NewFromConfig(awsCfg))
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

func configuredQueueURLs() (map[string]string, error) {
	raw := strings.TrimSpace(os.Getenv("HAKO_RESOURCE_PLANE_QUEUE_URLS"))
	if raw == "" {
		return nil, errors.New("HAKO_RESOURCE_PLANE_QUEUE_URLS is required")
	}
	var queueURLs map[string]string
	if err := json.Unmarshal([]byte(raw), &queueURLs); err != nil || len(queueURLs) == 0 {
		return nil, errors.New("HAKO_RESOURCE_PLANE_QUEUE_URLS must be a non-empty JSON object of Resource Plane IDs to SQS queue URLs")
	}
	normalized := make(map[string]string, len(queueURLs))
	for resourcePlaneID, queueURL := range queueURLs {
		resourcePlaneID = strings.TrimSpace(resourcePlaneID)
		queueURL = strings.TrimSpace(queueURL)
		if resourcePlaneID == "" || queueURL == "" {
			return nil, errors.New("HAKO_RESOURCE_PLANE_QUEUE_URLS must not contain empty Resource Plane IDs or queue URLs")
		}
		if _, exists := normalized[resourcePlaneID]; exists {
			return nil, errors.New("HAKO_RESOURCE_PLANE_QUEUE_URLS contains duplicate normalized Resource Plane IDs")
		}
		normalized[resourcePlaneID] = queueURL
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
