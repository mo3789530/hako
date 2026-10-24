package main

import (
	"reflect"
	"testing"
)

func TestConfiguredCheckoutCallers(t *testing.T) {
	got, err := configuredCheckoutCallers(" rp-tokyo-01, rp-us-01 ")
	want := []string{"spiffe://hako/resource-plane/rp-tokyo-01", "spiffe://hako/resource-plane/rp-us-01"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("configuredCheckoutCallers = %#v, %v", got, err)
	}
	if got, err := configuredCheckoutCallers("  "); err != nil || len(got) != 0 {
		t.Fatalf("empty allowlist = %#v, %v", got, err)
	}
	for _, invalid := range []string{"rp-a,rp-a", "rp-a,", "rp-a,../control-plane", "spiffe://hako/control-plane"} {
		if _, err := configuredCheckoutCallers(invalid); err == nil {
			t.Fatalf("invalid caller list %q was accepted", invalid)
		}
	}
}

func TestConfiguredRunnerCleanupCallers(t *testing.T) {
	got, err := configuredRunnerCleanupCallers(" rp-tokyo-01, rp-us-01 ")
	want := []string{"spiffe://hako/resource-plane/rp-tokyo-01", "spiffe://hako/resource-plane/rp-us-01"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("configuredRunnerCleanupCallers = %#v, %v", got, err)
	}
	if got, err := configuredRunnerCleanupCallers(" "); err != nil || len(got) != 0 {
		t.Fatalf("empty allowlist = %#v, %v", got, err)
	}
	for _, invalid := range []string{"rp-a,rp-a", "rp-a,", "rp-a,../control-plane", "spiffe://hako/control-plane"} {
		if _, err := configuredRunnerCleanupCallers(invalid); err == nil {
			t.Fatalf("invalid cleanup caller list %q was accepted", invalid)
		}
	}
}

func TestConfiguredAgentTaskCallers(t *testing.T) {
	got, err := configuredResourcePlaneCallers("rp-tokyo-01", "HAKO_GITHUB_BROKER_AGENT_TASK_RESOURCE_PLANE_IDS")
	want := []string{"spiffe://hako/resource-plane/rp-tokyo-01"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("configured Agent task callers = %#v, %v", got, err)
	}
	if got, err := configuredResourcePlaneCallers(" ", "HAKO_GITHUB_BROKER_AGENT_TASK_RESOURCE_PLANE_IDS"); err != nil || len(got) != 0 {
		t.Fatalf("empty allowlist = %#v, %v", got, err)
	}
	for _, invalid := range []string{"rp-tokyo-01,rp-tokyo-01", "rp-a,", "../rp", "spiffe://hako/control-plane"} {
		if _, err := configuredResourcePlaneCallers(invalid, "HAKO_GITHUB_BROKER_AGENT_TASK_RESOURCE_PLANE_IDS"); err == nil {
			t.Fatalf("invalid Agent task caller list %q was accepted", invalid)
		}
	}
}
