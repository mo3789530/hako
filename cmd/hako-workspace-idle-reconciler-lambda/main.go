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
		log.Fatal("invalid Workspace idle reconciler batch size (failure_class=configuration)")
	}
	lambda.Start(newIdleHandler(batchSize, func(ctx context.Context, now time.Time, limit int) (workspaceactivity.SweepStats, error) {
		return workspaceactivity.RunIdleSweep(ctx, pool, now, limit)
	}))
}

type idleSweep func(context.Context, time.Time, int) (workspaceactivity.SweepStats, error)

func newIdleHandler(batchSize int, sweep idleSweep) func(context.Context, events.CloudWatchEvent) error {
	return func(ctx context.Context, _ events.CloudWatchEvent) error {
		if ctx == nil || batchSize < 1 || batchSize > 1000 || sweep == nil {
			return errors.New("Workspace idle reconciliation is not configured")
		}
		stats, err := sweep(ctx, time.Now().UTC(), batchSize)
		if err != nil {
			log.Printf("Workspace idle reconciliation failed (failure_class=sweep checked=%d suspended=%d)", stats.Checked, stats.Suspended)
			return errors.New("Workspace idle reconciliation failed")
		}
		log.Printf("Workspace idle reconciliation completed (checked=%d suspended=%d)", stats.Checked, stats.Suspended)
		return nil
	}
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
