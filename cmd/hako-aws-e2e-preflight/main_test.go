package main

import (
	"context"
	"strings"
	"testing"
)

func testEnvironment() map[string]string {
	return map[string]string{
		"HAKO_AWS_E2E_ENABLED":                "true",
		"HAKO_AWS_E2E_REGION":                 "ap-northeast-1",
		"HAKO_AWS_E2E_RUN_ID":                 "local-e2e-001",
		"HAKO_AWS_E2E_CONTROL_PROFILE":        "hako-e2e-control",
		"HAKO_AWS_E2E_RESOURCE_PROFILE":       "hako-e2e-resource",
		"HAKO_AWS_E2E_CONTROL_ACCOUNT_ID":     "111111111111",
		"HAKO_AWS_E2E_RESOURCE_ACCOUNT_ID":    "222222222222",
		"HAKO_AWS_E2E_MANAGEMENT_ACCOUNT_ID":  "999999999999",
		"HAKO_AWS_E2E_PRODUCTION_ACCOUNT_IDS": "333333333333,444444444444",
	}
}

func TestPreflightChecksBothExplicitProfilesAndOnlyPrintsApprovedIDs(t *testing.T) {
	environment := testEnvironment()
	getenv := func(name string) string { return environment[name] }
	var calls []string
	lookup := func(_ context.Context, profile, region string) (string, error) {
		if region != "ap-northeast-1" {
			t.Fatalf("STS region = %q", region)
		}
		calls = append(calls, profile)
		if profile == "hako-e2e-control" {
			return "111111111111", nil
		}
		if profile == "hako-e2e-resource" {
			return "222222222222", nil
		}
		t.Fatalf("unexpected AWS profile %q", profile)
		return "", nil
	}
	var output strings.Builder
	if err := run(context.Background(), []string{"preflight"}, getenv, lookup, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "hako-e2e-control,hako-e2e-resource" {
		t.Fatalf("STS profile calls = %v", calls)
	}
	if !strings.Contains(output.String(), "run=local-e2e-001") || !strings.Contains(output.String(), "region=ap-northeast-1") || strings.Contains(output.String(), "arn:") {
		t.Fatalf("unexpected preflight output %q", output.String())
	}
}

func TestPreflightDoesNotCallSTSUntilEveryGuardPasses(t *testing.T) {
	tests := []struct {
		name   string
		change func(map[string]string)
	}{
		{name: "opt in", change: func(values map[string]string) { values["HAKO_AWS_E2E_ENABLED"] = "false" }},
		{name: "management account", change: func(values map[string]string) { values["HAKO_AWS_E2E_CONTROL_ACCOUNT_ID"] = "999999999999" }},
		{name: "production account", change: func(values map[string]string) { values["HAKO_AWS_E2E_RESOURCE_ACCOUNT_ID"] = "333333333333" }},
		{name: "ambient credentials", change: func(values map[string]string) { values["AWS_ACCESS_KEY_ID"] = "synthetic" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			environment := testEnvironment()
			tt.change(environment)
			lookupCalls := 0
			lookup := func(context.Context, string, string) (string, error) {
				lookupCalls++
				return "111111111111", nil
			}
			err := run(context.Background(), []string{"preflight"}, func(name string) string { return environment[name] }, lookup, &strings.Builder{})
			if err == nil || lookupCalls != 0 {
				t.Fatalf("preflight error=%v, STS calls=%d", err, lookupCalls)
			}
		})
	}
}

func TestPreflightRejectsCallerMismatch(t *testing.T) {
	environment := testEnvironment()
	lookup := func(_ context.Context, profile, _ string) (string, error) {
		if profile == "hako-e2e-control" {
			return "111111111111", nil
		}
		return "999999999999", nil
	}
	if err := run(context.Background(), []string{"preflight"}, func(name string) string { return environment[name] }, lookup, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched caller accepted: %v", err)
	}
}
