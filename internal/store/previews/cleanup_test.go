package previews

import (
	"testing"
	"time"
)

func TestCleanupRetryDelayIsExponentialAndCapped(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: 5 * time.Second},
		{attempt: 2, want: 10 * time.Second},
		{attempt: 3, want: 20 * time.Second},
		{attempt: 10, want: 2560 * time.Second},
		{attempt: 11, want: time.Hour},
		{attempt: 100, want: time.Hour},
	}
	for _, test := range tests {
		if got := cleanupRetryDelay(test.attempt); got != test.want {
			t.Errorf("cleanupRetryDelay(%d) = %s, want %s", test.attempt, got, test.want)
		}
	}
}

func TestCleanupErrorCodeRejectsUnboundedProviderError(t *testing.T) {
	for _, code := range []string{"provider_unavailable", "microvm_terminate_failed", "gateway_route_remove_failed", "preview_binding_missing"} {
		if !validCleanupErrorCode(code) {
			t.Errorf("rejected fixed cleanup error code %q", code)
		}
	}
	if validCleanupErrorCode("provider timeout: secret=do-not-log") {
		t.Fatal("accepted raw provider error as a persisted cleanup category")
	}
}
