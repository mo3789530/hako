package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/mo3789530/hako/internal/workspaceruntime"
)

func TestConfiguredRuntimeRequiresAnExplicitImplementation(t *testing.T) {
	for _, implementation := range []string{"", "aws", "real", " FAKE "} {
		if _, err := configuredRuntime(implementation, aws.Config{}, nil); err == nil {
			t.Errorf("runtime implementation %q should be rejected", implementation)
		}
	}
	runtime, err := configuredRuntime(" fake ", aws.Config{}, nil)
	if err != nil || runtime == nil {
		t.Fatalf("explicit fake runtime = %T, %v", runtime, err)
	}
}

func TestConfiguredLambdaMicroVMRuntimeRequiresExplicitTrustedConfiguration(t *testing.T) {
	spec := controllerWorkspaceLaunchSpec()
	profile, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"HAKO_WORKSPACE_RUNTIME_JOURNAL_BUCKET":   "hako-rp-journal",
		"HAKO_WORKSPACE_RUNTIME_JOURNAL_PREFIX":   "workspace-runtime/rp-test",
		"HAKO_WORKSPACE_RUNTIME_LAUNCH_SPEC_JSON": string(profile),
		"HAKO_WORKSPACE_RUNTIME_POLL_INTERVAL":    "100ms",
		"HAKO_WORKSPACE_RUNTIME_POLL_TIMEOUT":     "5s",
	}
	runtime, err := configuredRuntime("lambda-microvm", aws.Config{Region: "us-east-1"}, func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := runtime.(*workspaceruntime.LambdaMicroVM); !ok {
		t.Fatalf("configured runtime = %T, want Lambda MicroVM", runtime)
	}
	if _, err := configuredRuntime("lambda-microvm", aws.Config{Region: "us-east-1"}, nil); err == nil {
		t.Fatal("Lambda MicroVM runtime accepted missing deployment config")
	}
	if _, err := configuredRuntime("lambda-microvm", aws.Config{Region: "us-west-2"}, func(key string) string { return values[key] }); err == nil {
		t.Fatal("Lambda MicroVM runtime accepted a profile from another region")
	}
}

func TestWorkspaceRuntimeProfileRejectsSecretHookUnknownFieldsAndUnboundedInputs(t *testing.T) {
	spec := controllerWorkspaceLaunchSpec()
	profile, _ := json.Marshal(spec)
	base := map[string]string{"HAKO_WORKSPACE_RUNTIME_JOURNAL_BUCKET": "bucket", "HAKO_WORKSPACE_RUNTIME_LAUNCH_SPEC_JSON": string(profile)}
	for name, value := range map[string]string{
		"fixed per-Workspace hook": strings.TrimSuffix(string(profile), "}") + `,"run_hook_payload":"{}"}`,
		"unknown field":            strings.TrimSuffix(string(profile), "}") + `,"public_ingress":true}`,
		"trailing json":            string(profile) + `{}`,
		"oversize profile":         strings.Repeat("x", maxWorkspaceRuntimeProfileBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			values := map[string]string{}
			for key, item := range base {
				values[key] = item
			}
			values["HAKO_WORKSPACE_RUNTIME_LAUNCH_SPEC_JSON"] = value
			if _, err := configuredWorkspaceRuntime(func(key string) string { return values[key] }, "us-east-1"); err == nil {
				t.Fatal("invalid launch profile was accepted")
			}
		})
	}
}

func controllerWorkspaceLaunchSpec() workspaceruntime.LaunchSpec {
	return workspaceruntime.LaunchSpec{
		RuntimeClass: "standard", ImageIdentifier: "arn:aws:lambda:us-east-1:123456789012:microvm-image:hako-standard",
		ImageVersion: "3", ExecutionRoleARN: "arn:aws:iam::123456789012:role/hako-workspace",
		EgressNetworkConnectors:  []string{"arn:aws:lambda:us-east-1:123456789012:network-connector:egress"},
		MaximumDurationInSeconds: 3600, MaxIdleDurationSeconds: 3600,
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
