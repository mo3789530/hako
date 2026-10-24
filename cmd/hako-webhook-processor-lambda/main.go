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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/store/dsql"
	webhookstore "github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/workloadjobs"
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
			log.Printf("GitHub repository event batch failed (failure_class=process_batch claimed=%d processed=%d workload_runs_created=%d actions_runner_jobs_admitted=%d deferred=%d raw_payloads_purged=%d)", stats.Claimed, stats.Processed, stats.WorkloadRunsCreated, stats.ActionsRunnerJobsAdmitted, stats.Deferred, stats.RawPayloadsPurged)
			return errors.New("GitHub repository event batch failed")
		}
		log.Printf("GitHub repository event batch complete (claimed=%d processed=%d workload_runs_created=%d actions_runner_jobs_admitted=%d deferred=%d raw_payloads_purged=%d)", stats.Claimed, stats.Processed, stats.WorkloadRunsCreated, stats.ActionsRunnerJobsAdmitted, stats.Deferred, stats.RawPayloadsPurged)
		return nil
	}
}

func buildHandler(ctx context.Context) (func(context.Context, events.CloudWatchEvent) error, error) {
	pool, err := openDatabase(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect Hako webhook processor database: %w", err)
	}
	return handlerFor(func(ctx context.Context) (webhookstore.BatchStats, error) {
		now := time.Now().UTC()
		workloadTriggersEnabled := strings.EqualFold(strings.TrimSpace(os.Getenv("HAKO_WEBHOOK_WORKLOAD_TRIGGERS_ENABLED")), "true")
		actionsRunnerAdmissionsEnabled := strings.EqualFold(strings.TrimSpace(os.Getenv("HAKO_GITHUB_ACTIONS_RUNNER_ADMISSIONS_ENABLED")), "true")
		if actionsRunnerAdmissionsEnabled {
			var createRun webhookstore.RepositoryRunCreator
			if workloadTriggersEnabled {
				createRun = workloadjobs.CreateWebhookRepositoryRun
			}
			return webhookstore.ProcessBatchWithActionsRunnerAdmissions(ctx, pool, transaction.DefaultPolicy(), 10, now, createRun)
		}
		if workloadTriggersEnabled {
			return webhookstore.ProcessBatchWithRepositoryTriggers(ctx, pool, transaction.DefaultPolicy(), 10, now, workloadjobs.CreateWebhookRepositoryRun)
		}
		return webhookstore.ProcessBatch(ctx, pool, transaction.DefaultPolicy(), 10, now)
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
