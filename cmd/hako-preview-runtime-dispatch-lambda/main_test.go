package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/mo3789530/hako/internal/previewdispatch"
)

func TestParseBatchSize(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int
		bad   bool
	}{{"", 10, false}, {"1", 1, false}, {"100", 100, false}, {"0", 0, true}, {"101", 0, true}, {"bad", 0, true}} {
		got, err := parseBatchSize(test.value)
		if (err != nil) != test.bad || (!test.bad && got != test.want) {
			t.Errorf("parseBatchSize(%q) = %d, %v; want %d bad=%v", test.value, got, err, test.want, test.bad)
		}
	}
}

func TestHandlerRunsProvisionAndCleanupSweeps(t *testing.T) {
	worker := &fakePreviewRuntimeWorker{}
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.FixedZone("local", 9*60*60))
	handler := newHandler(worker, func() time.Time { return now })
	if err := handler(context.Background(), events.CloudWatchEvent{}); err != nil {
		t.Fatal(err)
	}
	if worker.provisionCalls != 1 || worker.cleanupCalls != 1 {
		t.Fatalf("runtime sweeps provision=%d cleanup=%d", worker.provisionCalls, worker.cleanupCalls)
	}
}

func TestHandlerAttemptsCleanupAfterProvisionFailureAndRedactsErrors(t *testing.T) {
	secret := errors.New("synthetic database connection secret")
	worker := &fakePreviewRuntimeWorker{provisionErr: secret, cleanupErr: secret}
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	handler := newHandler(worker, time.Now)
	err := handler(context.Background(), events.CloudWatchEvent{})
	if err == nil || err.Error() != "Preview runtime dispatch failed" || errors.Is(err, secret) {
		t.Fatalf("handler error=%v, want fixed retry error", err)
	}
	if worker.provisionCalls != 1 || worker.cleanupCalls != 1 {
		t.Fatalf("cleanup was skipped after provision failure: provision=%d cleanup=%d", worker.provisionCalls, worker.cleanupCalls)
	}
	if strings.Contains(logs.String(), secret.Error()) {
		t.Fatalf("raw failure leaked to logs: %s", logs.String())
	}
}

func TestHandlerRejectsMissingConfiguration(t *testing.T) {
	for _, test := range []struct {
		name   string
		worker previewRuntimeWorker
		clock  func() time.Time
	}{
		{name: "worker"},
		{name: "clock", worker: &fakePreviewRuntimeWorker{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := newHandler(test.worker, test.clock)(context.Background(), events.CloudWatchEvent{}); err == nil {
				t.Fatal("invalid handler configuration accepted")
			}
		})
	}
}

type fakePreviewRuntimeWorker struct {
	provisionCalls int
	cleanupCalls   int
	provisionErr   error
	cleanupErr     error
}

func (w *fakePreviewRuntimeWorker) RunOnce(context.Context) (previewdispatch.Stats, error) {
	w.provisionCalls++
	return previewdispatch.Stats{Claimed: 2, Enqueued: 2}, w.provisionErr
}

func (w *fakePreviewRuntimeWorker) RunCleanupOnce(context.Context) (previewdispatch.Stats, error) {
	w.cleanupCalls++
	return previewdispatch.Stats{Claimed: 3, Enqueued: 2, CleanedWithoutProvider: 1}, w.cleanupErr
}
