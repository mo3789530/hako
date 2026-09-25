package main

import (
	"context"
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
	"github.com/mo3789530/hako/internal/operationresults"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
)

func main() {
	queueURL := strings.TrimSpace(os.Getenv("HAKO_OPERATION_RESULT_QUEUE_URL"))
	if queueURL == "" {
		log.Fatal("HAKO_OPERATION_RESULT_QUEUE_URL is required")
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
	queue, err := operationresults.NewSQSQueue(sqs.NewFromConfig(awsCfg), queueURL)
	if err != nil {
		log.Fatalf("configure Operation result queue: %v", err)
	}
	consumer, err := operationresults.New(operationresults.StoreAdapter{Pool: pool, Policy: transaction.DefaultPolicy()}, queue, operationresults.Config{
		BatchSize: 10, WaitTime: 20 * time.Second,
	})
	if err != nil {
		log.Fatalf("configure Operation result consumer: %v", err)
	}
	log.Printf("Hako Operation result consumer started")
	for {
		stats, err := consumer.RunOnce(ctx)
		if err != nil {
			log.Printf("consume Operation results: %v (received=%d applied=%d duplicate=%d failed=%d)", err, stats.Received, stats.Applied, stats.Duplicate, stats.Failed)
		} else if stats.Received > 0 {
			log.Printf("Operation result batch complete (received=%d applied=%d duplicate=%d)", stats.Received, stats.Applied, stats.Duplicate)
		}
		if ctx.Err() != nil {
			return
		}
	}
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
