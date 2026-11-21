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
	"github.com/mo3789530/hako/internal/store/workspaceactivity"
)

func TestParseBatchSize(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int
		bad   bool
	}{{"", 100, false}, {"1", 1, false}, {"1000", 1000, false}, {"0", 0, true}, {"1001", 0, true}, {"bad", 0, true}} {
		got, err := parseBatchSize(test.value)
		if (err != nil) != test.bad || !test.bad && got != test.want {
			t.Errorf("parseBatchSize(%q) = %d, %v; want %d bad=%v", test.value, got, err, test.want, test.bad)
		}
	}
}

func TestIdleHandlerReturnsFixedRetryErrorWithoutLoggingDatabaseDetails(t *testing.T) {
	want := errors.New("sensitive database connection detail")
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	handler := newIdleHandler(10, func(context.Context, time.Time, int) (workspaceactivity.SweepStats, error) {
		return workspaceactivity.SweepStats{Checked: 3}, want
	})
	err := handler(context.Background(), events.CloudWatchEvent{})
	if err == nil || err.Error() != "Workspace idle reconciliation failed" || errors.Is(err, want) {
		t.Fatalf("handler error=%v, want fixed retry error", err)
	}
	if strings.Contains(logs.String(), want.Error()) || !strings.Contains(logs.String(), "failure_class=sweep") {
		t.Fatalf("failure log is not bounded: %s", logs.String())
	}
}
