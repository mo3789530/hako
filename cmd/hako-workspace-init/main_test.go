package main

import "testing"

func TestWorkspaceServicesEnabledSettingFailsClosed(t *testing.T) {
	for _, test := range []struct {
		value string
		want  bool
		bad   bool
	}{
		{value: "", want: false},
		{value: "false", want: false},
		{value: "true", want: true},
		{value: "1", bad: true},
		{value: "TRUE", bad: true},
	} {
		got, err := workspaceServicesEnabled(test.value)
		if test.bad && err == nil {
			t.Errorf("accepted setting %q", test.value)
		}
		if !test.bad && (err != nil || got != test.want) {
			t.Errorf("setting %q = %v, %v; want %v", test.value, got, err, test.want)
		}
	}
}
