package main

import (
	"context"
	"testing"
	"time"
)

func TestConfiguredQueueURLs(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "missing", wantErr: true},
		{name: "malformed", value: "{", wantErr: true},
		{name: "empty object", value: "{}", wantErr: true},
		{name: "null", value: "null", wantErr: true},
		{name: "empty plane ID", value: `{"":"https://sqs.example/commands"}`, wantErr: true},
		{name: "empty queue URL", value: `{"rp_tokyo":" "}`, wantErr: true},
		{name: "normalized duplicate", value: `{"rp_tokyo":"url-a"," rp_tokyo ":"url-b"}`, wantErr: true},
		{name: "valid", value: `{"rp_tokyo":"https://sqs.example/commands"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HAKO_RESOURCE_PLANE_QUEUE_URLS", test.value)
			got, err := configuredQueueURLs()
			if (err != nil) != test.wantErr {
				t.Fatalf("configuredQueueURLs error = %v, want error=%t", err, test.wantErr)
			}
			if err == nil && got["rp_tokyo"] != "https://sqs.example/commands" {
				t.Fatalf("unexpected queue URL mapping: %#v", got)
			}
		})
	}
}

func TestDurationEnvDefaultsAndParses(t *testing.T) {
	t.Setenv("HAKO_DISPATCHER_INTERVAL", "")
	if got := durationEnv("HAKO_DISPATCHER_INTERVAL", 3*time.Second); got != 3*time.Second {
		t.Fatalf("empty duration env = %s, want fallback", got)
	}
	t.Setenv("HAKO_DISPATCHER_INTERVAL", " 5s ")
	if got := durationEnv("HAKO_DISPATCHER_INTERVAL", time.Second); got != 5*time.Second {
		t.Fatalf("parsed duration = %s, want 5s", got)
	}
}

func TestOpenDatabaseRejectsMixedModes(t *testing.T) {
	t.Setenv("HAKO_DATABASE_URL", "postgres://local/db")
	t.Setenv("HAKO_DSQL_HOST", "cluster.dsql.us-east-1.on.aws")
	if _, err := openDatabase(context.Background()); err == nil {
		t.Fatal("both database modes should be rejected")
	}
}
