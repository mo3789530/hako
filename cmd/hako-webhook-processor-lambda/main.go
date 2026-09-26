package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/store/dsql"
	webhookstore "github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
)

func main() {
	ctx := context.Background()
	handler, err := buildHandler(ctx)
	if err != nil {
		log.Fatalf("configure Hako GitHub Webhook Processor Lambda: %v", err)
	}
	lambda.Start(handler)
}

type runOnce func(context.Context) (webhookstore.BatchStats, error)

func handlerFor(run runOnce) func(context.Context, events.CloudWatchEvent) error {
	return func(ctx context.Context, _ events.CloudWatchEvent) error {
		stats, err := run(ctx)
		if err != nil {
			return fmt.Errorf("process GitHub repository event batch (claimed=%d processed=%d deferred=%d): %w", stats.Claimed, stats.Processed, stats.Deferred, err)
		}
		log.Printf("GitHub repository event batch complete (claimed=%d processed=%d deferred=%d)", stats.Claimed, stats.Processed, stats.Deferred)
		return nil
	}
}

func buildHandler(ctx context.Context) (func(context.Context, events.CloudWatchEvent) error, error) {
	pool, err := openDatabase(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect Hako webhook processor database: %w", err)
	}
	return handlerFor(func(ctx context.Context) (webhookstore.BatchStats, error) {
		return webhookstore.ProcessBatch(ctx, pool, transaction.DefaultPolicy(), 10, time.Now().UTC())
	}), nil
}

func openDatabase(ctx context.Context) (*pgxpool.Pool, error) {
	postgresURL := strings.TrimSpace(os.Getenv("HAKO_DATABASE_URL"))
	dsqlHost := strings.TrimSpace(os.Getenv("HAKO_DSQL_HOST"))
	if postgresURL != "" && dsqlHost != "" {
		return nil, fmt.Errorf("set either HAKO_DATABASE_URL or HAKO_DSQL_HOST, not both")
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
