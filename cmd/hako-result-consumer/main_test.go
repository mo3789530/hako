package main

import (
	"context"
	"testing"
)

func TestOpenDatabaseRejectsMixedModes(t *testing.T) {
	t.Setenv("HAKO_DATABASE_URL", "postgres://local/db")
	t.Setenv("HAKO_DSQL_HOST", "cluster.dsql.us-east-1.on.aws")
	if _, err := openDatabase(context.Background()); err == nil {
		t.Fatal("both database modes should be rejected")
	}
}

func TestConfiguredResultQueueURLs(t *testing.T) {
	tests := []struct {
		name      string
		manifest  string
		queueURLs string
		queueURL  string
		wantCount int
		wantErr   bool
	}{
		{name: "missing", wantErr: true},
		{name: "single legacy queue", queueURL: "https://sqs.example/one", wantCount: 1},
		{name: "multiple queues", queueURLs: `{"rp-a":"https://sqs.example/a","rp-b":"https://sqs.example/b"}`, wantCount: 2},
		{name: "malformed map", queueURLs: "{", wantErr: true},
		{name: "empty map", queueURLs: "{}", wantErr: true},
		{name: "empty values", queueURLs: `{"rp-a":" "}`, wantErr: true},
		{name: "normalized duplicate ids", queueURLs: `{"rp-a":"url-a"," rp-a ":"url-b"}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HAKO_RESOURCE_PLANE_MANIFEST", test.manifest)
			t.Setenv("HAKO_OPERATION_RESULT_QUEUE_URLS", test.queueURLs)
			t.Setenv("HAKO_OPERATION_RESULT_QUEUE_URL", test.queueURL)
			got, err := configuredResultQueueURLs()
			if (err != nil) != test.wantErr {
				t.Fatalf("configuredResultQueueURLs() error = %v, want error=%t", err, test.wantErr)
			}
			if err == nil && len(got) != test.wantCount {
				t.Fatalf("configured queue count = %d, want %d (%#v)", len(got), test.wantCount, got)
			}
		})
	}
}

func TestConfiguredResultQueueURLsReadsManifest(t *testing.T) {
	t.Setenv("HAKO_RESOURCE_PLANE_MANIFEST", "../../config/resource-planes.example.json")
	t.Setenv("HAKO_OPERATION_RESULT_QUEUE_URLS", "")
	t.Setenv("HAKO_OPERATION_RESULT_QUEUE_URL", "")
	got, err := configuredResultQueueURLs()
	if err != nil {
		t.Fatal(err)
	}
	if got["rp-tokyo-01"] != "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-results" {
		t.Fatalf("manifest result queue mapping = %#v", got)
	}
	queues, err := configuredResultQueues()
	if err != nil || queues["rp-tokyo-01"].Region != "ap-northeast-1" {
		t.Fatalf("manifest result queue region = %+v, %v", queues, err)
	}
	t.Setenv("HAKO_OPERATION_RESULT_QUEUE_URL", "https://sqs.example/legacy")
	if _, err := configuredResultQueueURLs(); err == nil {
		t.Fatal("manifest and legacy result queue config should not be used together")
	}
}
