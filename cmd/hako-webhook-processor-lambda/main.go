package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/githubbroker"
	"github.com/mo3789530/hako/internal/store/dsql"
	webhookstore "github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/workloadjobs"
)

func main() {
	ctx := context.Background()
	handler, err := buildHandler(ctx)
	if err != nil {
		log.Fatal("configure Hako GitHub Webhook Processor Lambda failed (failure_class=handler_configuration)")
	}
	lambda.Start(handler)
}

type runOnce func(context.Context) (webhookstore.BatchStats, error)

func handlerFor(run runOnce) func(context.Context, events.CloudWatchEvent) error {
	return func(ctx context.Context, _ events.CloudWatchEvent) error {
		stats, err := run(ctx)
		if err != nil {
			log.Printf("GitHub webhook processor batch failed (failure_class=process_batch claimed=%d processed=%d workload_runs_created=%d actions_runner_jobs_admitted=%d deferred=%d raw_payloads_purged=%d)", stats.Claimed, stats.Processed, stats.WorkloadRunsCreated, stats.ActionsRunnerJobsAdmitted, stats.Deferred, stats.RawPayloadsPurged)
			return errors.New("GitHub webhook processor batch failed")
		}
		log.Printf("GitHub webhook processor batch complete (claimed=%d processed=%d workload_runs_created=%d actions_runner_jobs_admitted=%d deferred=%d raw_payloads_purged=%d)", stats.Claimed, stats.Processed, stats.WorkloadRunsCreated, stats.ActionsRunnerJobsAdmitted, stats.Deferred, stats.RawPayloadsPurged)
		return nil
	}
}

func combineRuns(first, second runOnce) runOnce {
	return func(ctx context.Context) (webhookstore.BatchStats, error) {
		var stats webhookstore.BatchStats
		var errs []error
		for _, run := range []runOnce{first, second} {
			if run == nil {
				continue
			}
			current, err := run(ctx)
			stats.Claimed += current.Claimed
			stats.Processed += current.Processed
			stats.WorkloadRunsCreated += current.WorkloadRunsCreated
			stats.RepositoryPreviewsUpdated += current.RepositoryPreviewsUpdated
			stats.ActionsRunnerJobsAdmitted += current.ActionsRunnerJobsAdmitted
			stats.Deferred += current.Deferred
			stats.RawPayloadsPurged += current.RawPayloadsPurged
			if err != nil {
				errs = append(errs, err)
			}
		}
		return stats, errors.Join(errs...)
	}
}

func buildHandler(ctx context.Context) (func(context.Context, events.CloudWatchEvent) error, error) {
	pool, err := openDatabase(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect Hako webhook processor database: %w", err)
	}
	repositoryRun := func(ctx context.Context) (webhookstore.BatchStats, error) {
		now := time.Now().UTC()
		workloadTriggersEnabled := strings.EqualFold(strings.TrimSpace(os.Getenv("HAKO_WEBHOOK_WORKLOAD_TRIGGERS_ENABLED")), "true")
		actionsRunnerAdmissionsEnabled := strings.EqualFold(strings.TrimSpace(os.Getenv("HAKO_GITHUB_ACTIONS_RUNNER_ADMISSIONS_ENABLED")), "true")
		if actionsRunnerAdmissionsEnabled {
			var createRun webhookstore.RepositoryRunCreator
			if workloadTriggersEnabled {
				createRun = workloadjobs.CreateWebhookRepositoryRun
			}
			return webhookstore.ProcessBatchWithActionsRunnerAdmissionsAndPreviews(ctx, pool, transaction.DefaultPolicy(), 10, now, createRun, workloadjobs.ApplyWebhookRepositoryPreview)
		}
		var createRun webhookstore.RepositoryRunCreator
		if workloadTriggersEnabled {
			createRun = workloadjobs.CreateWebhookRepositoryRun
		}
		return webhookstore.ProcessBatchWithRepositoryPreviews(ctx, pool, transaction.DefaultPolicy(), 10, now, createRun, workloadjobs.ApplyWebhookRepositoryPreview)
	}
	issueRun, err := agentIssueCommandRun(os.Getenv, pool)
	if err != nil {
		return nil, err
	}
	return handlerFor(combineRuns(repositoryRun, issueRun)), nil
}

func agentIssueCommandRun(getenv func(string) string, pool *pgxpool.Pool) (runOnce, error) {
	if getenv == nil {
		return nil, fmt.Errorf("environment reader is required")
	}
	enabled := strings.EqualFold(strings.TrimSpace(getenv("HAKO_AGENT_ISSUE_COMMANDS_ENABLED")), "true")
	if !enabled {
		return nil, nil
	}
	if !strings.EqualFold(strings.TrimSpace(getenv("HAKO_WORKLOAD_RUNS_ENABLED")), "true") ||
		!strings.EqualFold(strings.TrimSpace(getenv("HAKO_NATIVE_AGENT_ENABLED")), "true") {
		return nil, fmt.Errorf("Agent Issue command processing requires Workload Run and Native Agent execution gates")
	}
	if pool == nil {
		return nil, fmt.Errorf("Agent Issue command processing requires a database pool")
	}
	provider, err := agentIssueContextProviderFromFiles(getenv)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (webhookstore.BatchStats, error) {
		return workloadjobs.ProcessAgentIssueCommandBatch(ctx, pool, transaction.DefaultPolicy(), 10, provider, time.Now().UTC())
	}, nil
}

func agentIssueContextProviderFromFiles(getenv func(string) string) (*githubbroker.HTTPAgentIssueContextProvider, error) {
	baseURL := strings.TrimSpace(getenv("HAKO_AGENT_ISSUE_CONTEXT_BROKER_URL"))
	certPath := strings.TrimSpace(getenv("HAKO_AGENT_ISSUE_BROKER_CLIENT_CERT_FILE"))
	keyPath := strings.TrimSpace(getenv("HAKO_AGENT_ISSUE_BROKER_CLIENT_KEY_FILE"))
	caPath := strings.TrimSpace(getenv("HAKO_AGENT_ISSUE_BROKER_CA_FILE"))
	if baseURL == "" || certPath == "" || keyPath == "" || caPath == "" {
		return nil, fmt.Errorf("Agent Issue mTLS Broker URL, client certificate, private key, and CA files are required")
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read Agent Issue Broker client certificate")
	}
	defer clear(certPEM)
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read Agent Issue Broker client key")
	}
	defer clear(keyPEM)
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read Agent Issue Broker CA bundle")
	}
	defer clear(caPEM)
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse Agent Issue Broker client identity")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse Agent Issue Broker CA bundle")
	}
	client, err := githubbroker.NewAgentIssueContextMTLSClient(certificate, roots)
	if err != nil {
		return nil, fmt.Errorf("configure Agent Issue Broker mTLS client")
	}
	provider, err := githubbroker.NewHTTPAgentIssueContextProvider(baseURL, client)
	if err != nil {
		return nil, fmt.Errorf("configure Agent Issue Broker endpoint")
	}
	return provider, nil
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
