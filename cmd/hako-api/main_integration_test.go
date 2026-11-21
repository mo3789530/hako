//go:build integration

package main

import (
	"context"
	"os"
	"testing"

	"github.com/mo3789530/hako/internal/testutil"
)

func TestOpenDatabaseUsesConfiguredLocalPostgres(t *testing.T) {
	_ = testutil.NewIsolatedPostgres(t)
	databaseURL := os.Getenv("HAKO_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set HAKO_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	t.Setenv("HAKO_DATABASE_URL", databaseURL)
	t.Setenv("HAKO_DSQL_HOST", "")
	pool, err := openDatabase(context.Background())
	if err != nil {
		t.Fatalf("open configured PostgreSQL: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping configured PostgreSQL: %v", err)
	}
}
