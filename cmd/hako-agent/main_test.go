package main

import "testing"

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
