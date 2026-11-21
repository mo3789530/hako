//go:build integration

// Package testutil contains fixtures used by database integration tests.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewIsolatedPostgres creates a unique schema inside HAKO_TEST_DATABASE_URL.
// The schema is dropped during cleanup; the database itself is never dropped.
func NewIsolatedPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("HAKO_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set HAKO_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal("connect to integration PostgreSQL database")
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatal("ping integration PostgreSQL database")
	}

	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		adminPool.Close()
		t.Fatalf("generate isolated test schema name: %v", err)
	}
	schema := "hako_test_" + hex.EncodeToString(suffix)
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated test schema: %v", err)
	}

	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		_, _ = adminPool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		adminPool.Close()
		t.Fatal("parse integration PostgreSQL connection settings")
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		_, _ = adminPool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		adminPool.Close()
		t.Fatal("connect to isolated PostgreSQL schema")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_, _ = adminPool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		adminPool.Close()
		t.Fatal("ping isolated PostgreSQL schema")
	}

	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop isolated integration schema %s: %v", schema, err)
		}
		adminPool.Close()
	})
	return pool
}
