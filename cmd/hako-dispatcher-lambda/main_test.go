package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/mo3789530/hako/internal/actionsrunner"
	"github.com/mo3789530/hako/internal/dispatcher"
	"github.com/mo3789530/hako/internal/store/outbox"
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

func TestMarshalOutboxMetricUsesBoundedDimensions(t *testing.T) {
	payload, err := marshalOutboxMetric(outbox.PendingMetrics{OldestUnpublishedAgeSeconds: 42.5, HasUnpublished: true}, time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	if document["Service"] != "OutboxDispatcher" || document["OutboxOldestUnpublishedAgeSeconds"] != 42.5 || document["OutboxHasUnpublishedEvents"] != float64(1) {
		t.Fatalf("unexpected Outbox metric payload: %s", payload)
	}
	if strings.Contains(string(payload), "tenant") || strings.Contains(string(payload), "event_id") {
		t.Fatalf("Outbox metric contains high-cardinality identity: %s", payload)
	}
}

func TestHandlerForReadsOutboxLagWithoutFailingInvocation(t *testing.T) {
	read := false
	handler := handlerFor(func(context.Context) (dispatcher.Stats, error) {
		return dispatcher.Stats{}, nil
	}, func(context.Context, time.Time) (outbox.PendingMetrics, error) {
		read = true
		return outbox.PendingMetrics{}, errors.New("database details must not escape")
	})
	if err := handler(context.Background(), events.CloudWatchEvent{}); err != nil || !read {
		t.Fatalf("metric failure should not fail dispatch invocation: read=%v err=%v", read, err)
	}
}

func TestHandlerForReturnsBatchFailureForEventBridgeRetry(t *testing.T) {
	wantErr := errors.New("SQS unavailable at https://sensitive.example/private-queue")
	handler := handlerFor(func(context.Context) (dispatcher.Stats, error) {
		return dispatcher.Stats{Claimed: 1, Failed: 1}, wantErr
	})
	if err := handler(context.Background(), events.CloudWatchEvent{}); err == nil || errors.Is(err, wantErr) || strings.Contains(err.Error(), "sensitive.example") {
		t.Fatalf("handler should return a bounded retry error, got %v", err)
	}
}

func TestRunWithActionsRunnerDispatchesAfterOutboxAndPropagatesFailure(t *testing.T) {
	var calls []string
	base := func(context.Context) (dispatcher.Stats, error) {
		calls = append(calls, "outbox")
		return dispatcher.Stats{Claimed: 2, Published: 2}, nil
	}
	runnerErr := errors.New("runner queue unavailable")
	run := runWithActionsRunner(base, func(context.Context) (actionsrunner.RunnerStartupDispatchStats, error) {
		calls = append(calls, "runner")
		return actionsrunner.RunnerStartupDispatchStats{Claimed: 1, Failed: 1}, runnerErr
	})
	stats, err := run(context.Background())
	if !errors.Is(err, runnerErr) || stats.Published != 2 || len(calls) != 2 || calls[0] != "outbox" || calls[1] != "runner" {
		t.Fatalf("combined dispatcher stats=%+v calls=%v err=%v", stats, calls, err)
	}
	calls = nil
	if _, err := runWithActionsRunner(base, nil)(context.Background()); err != nil || len(calls) != 1 || calls[0] != "outbox" {
		t.Fatalf("default-off runner dispatch calls=%v err=%v", calls, err)
	}
}

func TestConfiguredQueuesRequiresManifest(t *testing.T) {
	t.Setenv("HAKO_RESOURCE_PLANE_MANIFEST_JSON", "")
	if _, err := configuredQueues(); err == nil {
		t.Fatal("expected missing manifest JSON to fail")
	}
}

func TestConfiguredQueuesUsesRegionAwareManifest(t *testing.T) {
	t.Setenv("HAKO_RESOURCE_PLANE_MANIFEST_JSON", `{"schema_version":1,"resource_planes":[{"id":"rp-tokyo-01","provider":"aws","account_id":"123456789012","region":"ap-northeast-1","capabilities":["microvm"],"command_queue_url":"https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-commands","result_queue_url":"https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-results","workload_command_queue_url":"https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-workload-commands","workload_result_queue_url":"https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-workload-results","actions_runner_startup_queue_url":"https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-actions-runner-startup"}]}`)
	queues, err := configuredQueues()
	if err != nil {
		t.Fatal(err)
	}
	if got := queues["rp-tokyo-01"]; got.Region != "ap-northeast-1" || got.URL != "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-commands" || got.WorkloadURL != "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-workload-commands" {
		t.Fatalf("manifest queue = %+v", got)
	}
	runnerQueue, found, err := configuredActionsRunnerQueue()
	if err != nil || !found || runnerQueue.ID != "rp-tokyo-01" || runnerQueue.Region != "ap-northeast-1" ||
		runnerQueue.ActionsRunnerStartupQueueURL != "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-actions-runner-startup" {
		t.Fatalf("runner startup route = %+v found=%v error=%v", runnerQueue, found, err)
	}
}
