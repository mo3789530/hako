package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/store/dsql"
)

func main() {
	if err := run(); err != nil {
		slog.Error("database migration failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	postgresURL := strings.TrimSpace(os.Getenv("HAKO_DATABASE_URL"))
	dsqlHost := strings.TrimSpace(os.Getenv("HAKO_DSQL_HOST"))
	if postgresURL != "" && dsqlHost != "" {
		return errors.New("set either HAKO_DATABASE_URL or HAKO_DSQL_HOST, not both")
	}

	var (
		pool *pgxpool.Pool
		err  error
		mode string
	)
	if postgresURL != "" {
		pool, err = dsql.OpenPostgres(ctx, postgresURL)
		mode = "postgres"
	} else {
		config := dsql.Config{
			Host:     dsqlHost,
			Region:   os.Getenv("AWS_REGION"),
			User:     strings.TrimSpace(os.Getenv("HAKO_DSQL_USER")),
			Database: os.Getenv("HAKO_DSQL_DATABASE"),
		}
		if config.User == "" {
			config.User = "admin"
		}
		pool, err = dsql.Open(ctx, config)
		mode = "aurora-dsql"
	}
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := dsql.Migrate(ctx, pool); err != nil {
		return err
	}
	slog.Info("database migrations applied", "database", mode)
	return nil
}
