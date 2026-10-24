package main

import (
	"context"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/previewdispatch"
	"github.com/mo3789530/hako/internal/store/dsql"
)

type previewRuntimeWorker interface {
	RunOnce(context.Context) (previewdispatch.Stats, error)
	RunCleanupOnce(context.Context) (previewdispatch.Stats, error)
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := openDatabase(ctx)
	if err != nil {
		log.Fatal("configure Preview runtime dispatcher database")
	}
	batchSize, err := parseBatchSize(os.Getenv("HAKO_PREVIEW_RUNTIME_BATCH_SIZE"))
	if err != nil {
		log.Fatal("invalid Preview runtime batch size (failure_class=configuration)")
	}
	runtimeClass := strings.TrimSpace(os.Getenv("HAKO_PREVIEW_RUNTIME_CLASS"))
	if runtimeClass == "" {
		runtimeClass = "standard"
	}
	if len(runtimeClass) > 64 || strings.ToLower(runtimeClass) != runtimeClass || strings.ContainsAny(runtimeClass, " \t\r\n") {
		log.Fatal("invalid Preview runtime class (failure_class=configuration)")
	}
	worker := previewdispatch.Worker{Pool: pool, RuntimeClass: runtimeClass, BatchSize: batchSize, Lease: 2 * time.Minute}
	lambda.Start(newHandler(worker, time.Now))
}

func newHandler(worker previewRuntimeWorker, clock func() time.Time) func(context.Context, events.CloudWatchEvent) error {
	return func(ctx context.Context, _ events.CloudWatchEvent) error {
		if ctx == nil || ctx.Err() != nil || worker == nil || clock == nil {
			return errors.New("Preview runtime dispatcher is not configured")
		}
		provisioned, provisionErr := worker.RunOnce(ctx)
		cleaned, cleanupErr := worker.RunCleanupOnce(ctx)
		log.Printf("Preview runtime dispatch completed (provision_claimed=%d provision_enqueued=%d cleanup_claimed=%d cleanup_enqueued=%d cleanup_local=%d at=%s)",
			provisioned.Claimed, provisioned.Enqueued, cleaned.Claimed, cleaned.Enqueued, cleaned.CleanedWithoutProvider, clock().UTC().Format(time.RFC3339))
		if provisionErr != nil || cleanupErr != nil {
			return errors.New("Preview runtime dispatch failed")
		}
		return nil
	}
}

func parseBatchSize(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 10, nil
	}
	size, err := strconv.Atoi(value)
	if err != nil || size < 1 || size > 100 {
		return 0, errors.New("HAKO_PREVIEW_RUNTIME_BATCH_SIZE must be between 1 and 100")
	}
	return size, nil
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
	if dsqlHost == "" {
		return nil, errors.New("HAKO_DSQL_HOST is required")
	}
	config := dsql.Config{Host: dsqlHost, Region: strings.TrimSpace(os.Getenv("AWS_REGION")),
		User: strings.TrimSpace(os.Getenv("HAKO_DSQL_USER")), Database: strings.TrimSpace(os.Getenv("HAKO_DSQL_DATABASE"))}
	if config.User == "" {
		config.User = "hako_preview_runtime_dispatcher"
	}
	return dsql.Open(ctx, config)
}

var _ previewRuntimeWorker = previewdispatch.Worker{}
