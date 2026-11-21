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

func TestAgentIssueCommandRunIsDisabledByDefaultAndRequiresExecutionGates(t *testing.T) {
	env := map[string]string{}
	getenv := func(key string) string { return env[key] }
	if run, err := agentIssueCommandRun(getenv, nil); err != nil || run != nil {
		t.Fatalf("default Agent Issue run = %v, %v", run != nil, err)
	}
	env["HAKO_AGENT_ISSUE_COMMANDS_ENABLED"] = "true"
	if run, err := agentIssueCommandRun(getenv, nil); err == nil || run != nil || !strings.Contains(err.Error(), "Workload Run and Native Agent execution gates") {
		t.Fatalf("Agent Issue run without runtime gates = %v, %v", run != nil, err)
	}
	env["HAKO_WORKLOAD_RUNS_ENABLED"] = "true"
	env["HAKO_NATIVE_AGENT_ENABLED"] = "true"
	if run, err := agentIssueCommandRun(getenv, nil); err == nil || run != nil || !strings.Contains(err.Error(), "database pool") {
		t.Fatalf("Agent Issue run without database pool = %v, %v", run != nil, err)
	}
}

func TestCombinedWebhookProcessorRunsBothBatchesAndAggregatesFailures(t *testing.T) {
	privateErr := errors.New("private Agent Issue broker detail")
	var firstCalls, secondCalls int
	combined := combineRuns(
		func(context.Context) (webhookstore.BatchStats, error) {
			firstCalls++
			return webhookstore.BatchStats{Claimed: 2, Processed: 1, RawPayloadsPurged: 3}, nil
		},
		func(context.Context) (webhookstore.BatchStats, error) {
			secondCalls++
			return webhookstore.BatchStats{Claimed: 1, Processed: 1, WorkloadRunsCreated: 1}, privateErr
		},
	)
	stats, err := combined(context.Background())
	if !errors.Is(err, privateErr) || firstCalls != 1 || secondCalls != 1 || stats.Claimed != 3 || stats.Processed != 2 ||
		stats.WorkloadRunsCreated != 1 || stats.RawPayloadsPurged != 3 {
		t.Fatalf("combined batches stats=%+v error=%v calls=(%d,%d)", stats, err, firstCalls, secondCalls)
	}
	handler := handlerFor(combined)
	if got := handler(context.Background(), events.CloudWatchEvent{}); got == nil || strings.Contains(got.Error(), "private Agent Issue broker detail") {
		t.Fatalf("combined Lambda error leaked private detail: %v", got)
	}
}

func TestAgentIssueProviderFilesRequireCompleteHTTPSMTLSConfiguration(t *testing.T) {
	env := map[string]string{"HAKO_AGENT_ISSUE_CONTEXT_BROKER_URL": "https://broker.example.test"}
	getenv := func(key string) string { return env[key] }
	if provider, err := agentIssueContextProviderFromFiles(getenv); err == nil || provider != nil || !strings.Contains(err.Error(), "mTLS Broker URL") {
		t.Fatalf("incomplete mTLS files = %v, %v", provider, err)
	}
	env["HAKO_AGENT_ISSUE_BROKER_CLIENT_CERT_FILE"] = "/missing/cert.pem"
	env["HAKO_AGENT_ISSUE_BROKER_CLIENT_KEY_FILE"] = "/missing/key.pem"
	env["HAKO_AGENT_ISSUE_BROKER_CA_FILE"] = "/missing/ca.pem"
	if provider, err := agentIssueContextProviderFromFiles(getenv); err == nil || provider != nil || !strings.Contains(err.Error(), "read Agent Issue Broker client certificate") || strings.Contains(err.Error(), "/missing") {
		t.Fatalf("unreadable mTLS files = %v, %v", provider, err)
	}
}

func TestCombineRunsReturnsNilErrorWhenBothBatchesSucceed(t *testing.T) {
	run := combineRuns(
		func(context.Context) (webhookstore.BatchStats, error) {
			return webhookstore.BatchStats{Claimed: 1}, nil
		},
		func(context.Context) (webhookstore.BatchStats, error) {
			return webhookstore.BatchStats{Processed: 1}, nil
		},
	)
	stats, err := run(context.Background())
	if err != nil || stats.Claimed != 1 || stats.Processed != 1 {
		t.Fatalf("combined successful batches stats=%+v error=%v", stats, err)
	}
}
