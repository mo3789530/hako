package main

import (
	"testing"
	"time"
)

func TestConfiguredRuntimeIsExplicitlyFakeOnly(t *testing.T) {
	for _, implementation := range []string{"", "aws", "real", " FAKE "} {
		if _, err := configuredRuntime(implementation); err == nil {
			t.Errorf("runtime implementation %q should be rejected", implementation)
		}
	}
	runtime, err := configuredRuntime(" fake ")
	if err != nil || runtime == nil {
		t.Fatalf("explicit fake runtime = %T, %v", runtime, err)
	}
}

func TestParseExecutionTimeout(t *testing.T) {
	if got, err := parseExecutionTimeout(" "); err != nil || got != 45*time.Second {
		t.Fatalf("default operation timeout = %s, %v", got, err)
	}
	if got, err := parseExecutionTimeout("90s"); err != nil || got != 90*time.Second {
		t.Fatalf("configured operation timeout = %s, %v", got, err)
	}
	for _, value := range []string{"invalid", "0s", "-1s"} {
		if _, err := parseExecutionTimeout(value); err == nil {
			t.Errorf("invalid operation timeout %q should fail", value)
		}
	}
}

func TestParseMaxCommandAge(t *testing.T) {
	if got, err := parseMaxCommandAge(" "); err != nil || got != 30*time.Minute {
		t.Fatalf("default command max age = %s, %v", got, err)
	}
	if got, err := parseMaxCommandAge("20m"); err != nil || got != 20*time.Minute {
		t.Fatalf("configured command max age = %s, %v", got, err)
	}
	for _, value := range []string{"invalid", "0s", "-1m"} {
		if _, err := parseMaxCommandAge(value); err == nil {
			t.Errorf("invalid command max age %q should fail", value)
		}
	}
}
