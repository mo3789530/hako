package main

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/previewresult"
	"github.com/mo3789530/hako/internal/store/dsql"
)

type previewResultHandler interface {
	Handle(context.Context, events.SQSEvent) (events.SQSEventResponse, error)
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := openDatabase(ctx)
	if err != nil {
		log.Fatal("configure Preview result consumer database")
	}
	handler, err := previewresult.NewLambdaHandler(previewresult.Consumer{Pool: pool})
	if err != nil {
		log.Fatal("configure Preview result consumer")
	}
	lambda.Start(newHandler(handler))
}

func newHandler(handler previewResultHandler) func(context.Context, events.SQSEvent) (events.SQSEventResponse, error) {
	return func(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
		if ctx == nil || ctx.Err() != nil || handler == nil {
			return events.SQSEventResponse{}, errors.New("Preview result consumer is not configured")
		}
		return handler.Handle(ctx, event)
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
	if dsqlHost == "" {
		return nil, errors.New("HAKO_DSQL_HOST is required")
	}
	config := dsql.Config{Host: dsqlHost, Region: strings.TrimSpace(os.Getenv("AWS_REGION")),
		User: strings.TrimSpace(os.Getenv("HAKO_DSQL_USER")), Database: strings.TrimSpace(os.Getenv("HAKO_DSQL_DATABASE"))}
	if config.User == "" {
		config.User = "hako_preview_result_consumer"
	}
	return dsql.Open(ctx, config)
}

var _ previewResultHandler = (*previewresult.LambdaHandler)(nil)
