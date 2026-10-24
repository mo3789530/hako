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
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/previews"
)

type expirySweep func(context.Context, time.Time, int) (int64, error)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := openDatabase(ctx)
	if err != nil {
		log.Fatal("configure Preview expiry database")
	}
	batchSize, err := parseBatchSize(os.Getenv("HAKO_PREVIEW_EXPIRY_BATCH_SIZE"))
	if err != nil {
		log.Fatal(err)
	}
	lambda.Start(newHandler(batchSize, time.Now, func(ctx context.Context, now time.Time, limit int) (int64, error) {
		return previews.ExpireBatch(ctx, pool, now, limit)
	}))
}

func newHandler(batchSize int, clock func() time.Time, sweep expirySweep) func(context.Context, events.CloudWatchEvent) error {
	return func(ctx context.Context, _ events.CloudWatchEvent) error {
		if ctx == nil || batchSize < 1 || batchSize > 500 || clock == nil || sweep == nil {
			return errors.New("Preview expiry handler is not configured")
		}
		expired, err := sweep(ctx, clock().UTC(), batchSize)
		if err != nil {
			log.Printf("Preview expiry sweep failed (expired=%d)", expired)
			return err
		}
		log.Printf("Preview expiry sweep completed (expired=%d)", expired)
		return nil
	}
}

func parseBatchSize(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 100, nil
	}
	size, err := strconv.Atoi(value)
	if err != nil || size < 1 || size > 500 {
		return 0, errors.New("HAKO_PREVIEW_EXPIRY_BATCH_SIZE must be between 1 and 500")
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
		config.User = "hako_preview_reconciler"
	}
	return dsql.Open(ctx, config)
}
