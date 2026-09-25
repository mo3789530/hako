package main

import (
	"context"
	"testing"
)

func TestOpenDatabaseRejectsAmbiguousOrMissingConfiguration(t *testing.T) {
	t.Setenv("HAKO_DATABASE_URL", "postgres://local/db")
	t.Setenv("HAKO_DSQL_HOST", "cluster.dsql.us-east-1.on.aws")
	if _, err := openDatabase(context.Background()); err == nil {
		t.Fatal("both database modes should be rejected")
	}
	t.Setenv("HAKO_DATABASE_URL", "")
	t.Setenv("HAKO_DSQL_HOST", "")
	if _, err := openDatabase(context.Background()); err == nil {
		t.Fatal("missing DSQL host should fail validation")
	}
}
