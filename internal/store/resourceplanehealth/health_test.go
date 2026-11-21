package resourceplanehealth

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateHealthReport(t *testing.T) {
	tests := []struct {
		name      string
		status    Status
		reason    string
		want      string
		wantError bool
	}{
		{name: "valid", status: Degraded, reason: "  queue delay  ", want: "queue delay"},
		{name: "empty reason allowed", status: Healthy},
		{name: "unsupported status", status: "unknown", wantError: true},
		{name: "oversized reason", status: Unhealthy, reason: strings.Repeat("x", MaxReasonLength+1), wantError: true},
		{name: "control character", status: Degraded, reason: "line one\nline two", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Validate(test.status, test.reason)
			if (err != nil) != test.wantError {
				t.Fatalf("Validate() error = %v, want error %t", err, test.wantError)
			}
			if test.wantError && !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate() error = %v, want ErrInvalid", err)
			}
			if got != test.want {
				t.Fatalf("Validate() reason = %q, want %q", got, test.want)
			}
		})
	}
}
