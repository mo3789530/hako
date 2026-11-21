package main

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestLogMigrationFailureUsesBoundedFailureClass(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	logMigrationFailure(logger, errors.New("database query failed: secret=do-not-log"))
	got := output.String()
	if !strings.Contains(got, "failure_class=migration") || strings.Contains(got, "secret=do-not-log") || strings.Contains(got, "error=") {
		t.Fatalf("migration failure log = %q; want only a bounded failure class", got)
	}
}
