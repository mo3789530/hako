package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func TestHandlerForwardsSQSPartialBatchResponse(t *testing.T) {
	want := events.SQSEventResponse{BatchItemFailures: []events.SQSBatchItemFailure{{ItemIdentifier: "message-1"}}}
	fake := &fakePreviewResultHandler{response: want}
	handler := newHandler(fake)
	got, err := handler(context.Background(), events.SQSEvent{Records: []events.SQSMessage{{MessageId: "message-1"}}})
	if err != nil || len(got.BatchItemFailures) != 1 || got.BatchItemFailures[0].ItemIdentifier != "message-1" {
		t.Fatalf("partial batch response = %+v, %v; want %+v", got, err, want)
	}
	if !fake.called {
		t.Fatal("Preview result handler was not called")
	}
}

func TestHandlerPropagatesUnexpectedFailure(t *testing.T) {
	wantErr := errors.New("consumer unavailable")
	_, err := newHandler(&fakePreviewResultHandler{err: wantErr})(context.Background(), events.SQSEvent{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("handler error = %v, want %v", err, wantErr)
	}
}

func TestHandlerRejectsInvalidDependenciesAndCanceledContext(t *testing.T) {
	tests := []struct {
		name    string
		ctx     context.Context
		handler previewResultHandler
	}{
		{name: "missing handler", ctx: context.Background()},
		{name: "canceled", ctx: canceledContext(), handler: &fakePreviewResultHandler{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := newHandler(test.handler)(test.ctx, events.SQSEvent{})
			if err == nil || len(got.BatchItemFailures) != 0 {
				t.Fatalf("handler response = %+v, %v; want configuration error", got, err)
			}
		})
	}
}

func TestDatabaseConnectionConfigSelectsExactlyOneBackend(t *testing.T) {
	getenv := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	if _, err := databaseConnectionConfig(nil); err == nil {
		t.Fatal("databaseConnectionConfig accepted a nil environment reader")
	}
	for _, test := range []struct {
		name   string
		values map[string]string
		want   string
	}{
		{name: "missing backend", want: "HAKO_DSQL_HOST is required"},
		{name: "both backends", values: map[string]string{"HAKO_DATABASE_URL": " postgres://localhost/hako ", "HAKO_DSQL_HOST": " dsql.example "}, want: "set either HAKO_DATABASE_URL or HAKO_DSQL_HOST, not both"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := databaseConnectionConfig(getenv(test.values)); err == nil || err.Error() != test.want {
				t.Fatalf("databaseConnectionConfig error = %v, want %q", err, test.want)
			}
		})
	}
	postgres, err := databaseConnectionConfig(getenv(map[string]string{"HAKO_DATABASE_URL": " postgres://localhost/hako "}))
	if err != nil || postgres.postgresURL != "postgres://localhost/hako" || postgres.dsql.Host != "" {
		t.Fatalf("PostgreSQL connection selection = %+v, %v", postgres, err)
	}
	dsqlDefault, err := databaseConnectionConfig(getenv(map[string]string{"HAKO_DSQL_HOST": " dsql.example ", "AWS_REGION": " ap-northeast-1 ", "HAKO_DSQL_DATABASE": " hako "}))
	if err != nil || dsqlDefault.dsql.Host != "dsql.example" || dsqlDefault.dsql.Region != "ap-northeast-1" ||
		dsqlDefault.dsql.User != "hako_preview_result_consumer" || dsqlDefault.dsql.Database != "hako" {
		t.Fatalf("default DSQL connection selection = %+v, %v", dsqlDefault, err)
	}
	dsqlCustom, err := databaseConnectionConfig(getenv(map[string]string{"HAKO_DSQL_HOST": "dsql.example", "HAKO_DSQL_USER": "custom_user", "HAKO_DSQL_DATABASE": "custom_db"}))
	if err != nil || dsqlCustom.dsql.User != "custom_user" || dsqlCustom.dsql.Database != "custom_db" {
		t.Fatalf("custom DSQL connection selection = %+v, %v", dsqlCustom, err)
	}
}

func TestOpenDatabaseRejectsInvalidPostgresURLWithoutLeakingIt(t *testing.T) {
	secretURL := "mysql://local-user:local-password@localhost/hako"
	t.Setenv("HAKO_DATABASE_URL", secretURL)
	t.Setenv("HAKO_DSQL_HOST", "")
	_, err := openDatabase(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HAKO_DATABASE_URL must be a PostgreSQL URL") || strings.Contains(err.Error(), "local-password") {
		t.Fatalf("invalid PostgreSQL URL error = %v", err)
	}
}

type fakePreviewResultHandler struct {
	response events.SQSEventResponse
	err      error
	called   bool
}

func (h *fakePreviewResultHandler) Handle(context.Context, events.SQSEvent) (events.SQSEventResponse, error) {
	h.called = true
	return h.response, h.err
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
