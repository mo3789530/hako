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
	"github.com/mo3789530/hako/internal/store/workspaceactivity"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := openDatabase(ctx)
	if err != nil {
		log.Fatal("configure Workspace idle reconciler database")
	}
	batchSize, err := parseBatchSize(os.Getenv("HAKO_WORKSPACE_IDLE_RECONCILER_BATCH_SIZE"))
	if err != nil {
		log.Fatal(err)
	}
	lambda.Start(func(ctx context.Context, _ events.CloudWatchEvent) error {
		stats, err := workspaceactivity.RunIdleSweep(ctx, pool, time.Now().UTC(), batchSize)
		if err != nil {
			log.Printf("Workspace idle reconciliation failed (checked=%d suspended=%d)", stats.Checked, stats.Suspended)
			return err
		}
		log.Printf("Workspace idle reconciliation completed (checked=%d suspended=%d)", stats.Checked, stats.Suspended)
		return nil
	})
}

func parseBatchSize(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 100, nil
	}
	size, err := strconv.Atoi(value)
	if err != nil || size < 1 || size > 1000 {
		return 0, errors.New("HAKO_WORKSPACE_IDLE_RECONCILER_BATCH_SIZE must be between 1 and 1000")
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
		config.User = "hako_workspace_idle_reconciler"
	}
	return dsql.Open(ctx, config)
}
