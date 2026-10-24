package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	webhookstore "github.com/mo3789530/hako/internal/store/githubwebhook"
)

func TestHandlerForReturnsBoundedFailureForRetry(t *testing.T) {
	secretDetail := errors.New("database failed for delivery body=private-payload")
	handler := handlerFor(func(context.Context) (webhookstore.BatchStats, error) {
		return webhookstore.BatchStats{Claimed: 1, Processed: 0, Deferred: 1}, secretDetail
	})
	err := handler(context.Background(), events.CloudWatchEvent{})
	if err == nil || errors.Is(err, secretDetail) || strings.Contains(err.Error(), "private-payload") {
		t.Fatalf("handler should return a bounded retry error, got %v", err)
	}
}
