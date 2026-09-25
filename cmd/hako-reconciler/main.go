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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/reconciler"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := openDatabase(ctx)
	if err != nil {
		log.Fatalf("connect Hako Reconciler database: %v", err)
	}
	defer pool.Close()

	worker, err := reconciler.New(reconciler.StoreAdapter{Pool: pool, Policy: transaction.DefaultPolicy()}, reconciler.Config{
		BatchSize: 100, FailureDelay: durationEnv("HAKO_RECONCILER_FAILURE_DELAY", time.Minute),
		OperationTimeout: durationEnv("HAKO_RECONCILER_OPERATION_TIMEOUT", 30*time.Minute),
	})
	if err != nil {
		log.Fatalf("configure Hako Reconciler: %v", err)
	}
	interval := durationEnv("HAKO_RECONCILER_INTERVAL", 10*time.Second)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.Printf("Hako Reconciler started (poll interval %s)", interval)
	for {
		stats, err := worker.RunOnce(ctx)
		if err != nil {
			log.Printf("reconcile batch: %v (timed_out=%d checked=%d created=%d skipped=%d failed=%d)", err, stats.TimedOut, stats.Checked, stats.Created, stats.Skipped, stats.Failed)
		} else if stats.Checked > 0 || stats.TimedOut > 0 {
			log.Printf("reconcile batch complete (timed_out=%d checked=%d created=%d skipped=%d)", stats.TimedOut, stats.Checked, stats.Created, stats.Skipped)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
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
