package main

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/mo3789530/hako/internal/dispatcher"
)

func TestHandlerForReturnsSuccessfulBatch(t *testing.T) {
	called := false
	handler := handlerFor(func(context.Context) (dispatcher.Stats, error) {
		called = true
		return dispatcher.Stats{Claimed: 2, Published: 2}, nil
	})
	if err := handler(context.Background(), events.CloudWatchEvent{}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("expected dispatcher batch to run")
	}
}

func TestHandlerForReturnsBatchFailureForEventBridgeRetry(t *testing.T) {
	wantErr := errors.New("SQS unavailable")
	handler := handlerFor(func(context.Context) (dispatcher.Stats, error) {
		return dispatcher.Stats{Claimed: 1, Failed: 1}, wantErr
	})
	if err := handler(context.Background(), events.CloudWatchEvent{}); !errors.Is(err, wantErr) {
		t.Fatalf("handler error = %v, want wrapped SQS error", err)
	}
}

func TestConfiguredQueuesRequiresManifest(t *testing.T) {
	t.Setenv("HAKO_RESOURCE_PLANE_MANIFEST_JSON", "")
	if _, err := configuredQueues(); err == nil {
		t.Fatal("expected missing manifest JSON to fail")
	}
}

func TestConfiguredQueuesUsesRegionAwareManifest(t *testing.T) {
	t.Setenv("HAKO_RESOURCE_PLANE_MANIFEST_JSON", `{"schema_version":1,"resource_planes":[{"id":"rp-tokyo-01","provider":"aws","account_id":"123456789012","region":"ap-northeast-1","capabilities":["microvm"],"command_queue_url":"https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-commands","result_queue_url":"https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-results"}]}`)
	queues, err := configuredQueues()
	if err != nil {
		t.Fatal(err)
	}
	if got := queues["rp-tokyo-01"]; got.Region != "ap-northeast-1" || got.URL != "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-commands" {
		t.Fatalf("manifest queue = %+v", got)
	}
}
