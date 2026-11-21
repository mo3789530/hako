package main

import (
	"testing"
	"time"
)

func TestParseCommandMaxAge(t *testing.T) {
	if got, err := parseCommandMaxAge(""); err != nil || got != 30*time.Minute {
		t.Fatalf("default command max age = %s, %v", got, err)
	}
	if got, err := parseCommandMaxAge("12m"); err != nil || got != 12*time.Minute {
		t.Fatalf("configured command max age = %s, %v", got, err)
	}
	for _, value := range []string{"invalid", "0s", "-1s"} {
		if _, err := parseCommandMaxAge(value); err == nil {
			t.Errorf("invalid command max age %q should fail", value)
		}
	}
}
