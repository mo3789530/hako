package main

import (
	"testing"

	"github.com/mo3789530/hako/internal/workloadagent"
)

func TestParseJobServicesEnabled(t *testing.T) {
	for _, test := range []struct {
		value string
		want  bool
		bad   bool
	}{{"", false, false}, {"false", false, false}, {" true ", true, false}, {"yes", false, true}, {"TRUE", false, true}} {
		got, err := parseJobServicesEnabled(test.value)
		if (err != nil) != test.bad || (!test.bad && got != test.want) {
			t.Errorf("parseJobServicesEnabled(%q) = %t, %v", test.value, got, err)
		}
	}
}

func TestConfigureJobServicesDefaultsOff(t *testing.T) {
	t.Setenv("HAKO_JOB_SERVICES_ENABLED", "")
	provisioner, runtime, err := configureJobServices()
	if err != nil || provisioner != nil || runtime != nil {
		t.Fatalf("default Job Services configuration = %v, %v, %v", provisioner, runtime, err)
	}
	t.Setenv("HAKO_JOB_SERVICES_ENABLED", "false")
	provisioner, runtime, err = configureJobServices()
	if err != nil || provisioner != nil || runtime != nil {
		t.Fatalf("disabled Job Services configuration = %v, %v, %v", provisioner, runtime, err)
	}
}

func TestNativeAgentProcessCompositionDefaultsOffAndRequiresFixedModel(t *testing.T) {
	if enabled, err := parseNativeAgentEnabled(""); err != nil || enabled {
		t.Fatalf("Native Agent default = %t, %v", enabled, err)
	}
	if _, err := parseNativeAgentEnabled("sometimes"); err == nil {
		t.Fatal("invalid Native Agent setting was accepted")
	}
	defaultFactory, err := configureProcessFactory(false, "", nil, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := defaultFactory.(workloadagent.NativeAgentCredentialProcessFactory); ok {
		t.Fatal("default process factory unexpectedly accepts model credentials")
	}
	if _, err := configureProcessFactory(true, "", nil, t.TempDir(), nil); err == nil {
		t.Fatal("enabled Native Agent accepted a missing fixed model")
	}
	factory, err := configureProcessFactory(true, "claude-sonnet-4-6", nil, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	native, ok := factory.(workloadagent.NativeAgentCredentialProcessFactory)
	if !ok || !native.SupportsNativeAgentCredential() {
		t.Fatal("explicitly enabled process factory does not declare Native Agent credential handling")
	}
	composed, ok := factory.(workloadProcessFactory)
	if !ok || composed.agent.Model == nil || composed.agent.Tests == nil {
		t.Fatal("Native Agent factory is not composed with a model transport and fixed test runner")
	}
}
