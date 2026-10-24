package githubwebhook

import (
	"context"
	"testing"
)

func TestCorrelationIDContextAcceptsOnlyServerIDFormat(t *testing.T) {
	const valid = "req_01234567-89ab-4cde-8fab-0123456789ab"
	if got := CorrelationIDFromContext(WithCorrelationID(context.Background(), valid)); got != valid {
		t.Fatalf("correlation ID=%q want %q", got, valid)
	}
	for _, invalid := range []string{"", "req_not-a-uuid", "req_01234567-89AB-4cde-8fab-0123456789ab"} {
		if got := CorrelationIDFromContext(WithCorrelationID(context.Background(), invalid)); got != "" {
			t.Fatalf("invalid correlation ID %q was retained as %q", invalid, got)
		}
	}
	if got := CorrelationIDFromContext(nil); got != "" {
		t.Fatalf("nil context correlation ID=%q, want empty", got)
	}
}
