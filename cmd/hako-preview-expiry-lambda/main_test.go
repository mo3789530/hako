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
)

func TestParseBatchSize(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int
		bad   bool
	}{{"", 100, false}, {" 1 ", 1, false}, {"500", 500, false}, {"0", 0, true}, {"501", 0, true}, {"bad", 0, true}} {
		got, err := parseBatchSize(test.value)
		if (err != nil) != test.bad || !test.bad && got != test.want {
			t.Errorf("parseBatchSize(%q) = %d, %v; want %d bad=%v", test.value, got, err, test.want, test.bad)
		}
	}
}

func TestHandlerUsesUTCAndBoundedSweep(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.FixedZone("local", 9*60*60))
	called := false
	handler := newHandler(20, func() time.Time { return now }, func(_ context.Context, observed time.Time, limit int) (int64, error) {
		called = true
		if !observed.Equal(now.UTC()) || limit != 20 {
			t.Fatalf("sweep args = %s, %d", observed, limit)
		}
		return 3, nil
	})
	if err := handler(context.Background(), events.CloudWatchEvent{}); err != nil || !called {
		t.Fatalf("handler error=%v called=%v", err, called)
	}
}

func TestHandlerReturnsSweepFailure(t *testing.T) {
	want := errors.New("sensitive transaction connection details")
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	handler := newHandler(10, time.Now, func(context.Context, time.Time, int) (int64, error) { return 1, want })
	err := handler(context.Background(), events.CloudWatchEvent{})
	if err == nil || err.Error() != "Preview expiry sweep failed" || errors.Is(err, want) {
		t.Fatalf("handler error=%v, want a fixed retry error", err)
	}
	if strings.Contains(logs.String(), want.Error()) {
		t.Fatalf("raw sweep error leaked to logs: %s", logs.String())
	}
}
