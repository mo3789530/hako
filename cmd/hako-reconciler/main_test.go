package main

import (
	"context"
	"testing"
	"time"
)

func TestDurationEnvDefaultsAndParses(t *testing.T) {
	t.Setenv("HAKO_RECONCILER_INTERVAL", "")
	if got := durationEnv("HAKO_RECONCILER_INTERVAL", 10*time.Second); got != 10*time.Second {
		t.Fatalf("empty duration env = %s, want fallback", got)
	}
	t.Setenv("HAKO_RECONCILER_INTERVAL", " 2m ")
	if got := durationEnv("HAKO_RECONCILER_INTERVAL", time.Second); got != 2*time.Minute {
		t.Fatalf("parsed duration = %s, want 2m", got)
	}
	t.Setenv("HAKO_RECONCILER_OPERATION_TIMEOUT", "")
	if got := durationEnv("HAKO_RECONCILER_OPERATION_TIMEOUT", 30*time.Minute); got != 30*time.Minute {
		t.Fatalf("Operation timeout default = %s, want 30m", got)
	}
}

func TestOpenDatabaseRejectsMixedModes(t *testing.T) {
	t.Setenv("HAKO_DATABASE_URL", "postgres://local/db")
	t.Setenv("HAKO_DSQL_HOST", "cluster.dsql.us-east-1.on.aws")
	if _, err := openDatabase(context.Background()); err == nil {
		t.Fatal("both database modes should be rejected")
	}
}
