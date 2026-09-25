package main

import "testing"

func TestParseCLIPage(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantLimit  int
		wantOffset int
		wantError  bool
	}{
		{name: "defaults", wantLimit: 20, wantOffset: 0},
		{name: "limit only", args: []string{"50"}, wantLimit: 50},
		{name: "limit and offset", args: []string{"100", "1000000"}, wantLimit: 100, wantOffset: 1000000},
		{name: "non integer limit", args: []string{"many"}, wantError: true},
		{name: "non integer offset", args: []string{"20", "later"}, wantError: true},
		{name: "zero limit", args: []string{"0"}, wantError: true},
		{name: "limit above maximum", args: []string{"101"}, wantError: true},
		{name: "negative offset", args: []string{"20", "-1"}, wantError: true},
		{name: "offset above maximum", args: []string{"20", "1000001"}, wantError: true},
		{name: "too many arguments", args: []string{"20", "0", "extra"}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limit, offset, err := parseCLIPage(test.args)
			if (err != nil) != test.wantError {
				t.Fatalf("parseCLIPage(%q) error = %v, want error=%t", test.args, err, test.wantError)
			}
			if err == nil && (limit != test.wantLimit || offset != test.wantOffset) {
				t.Fatalf("parseCLIPage(%q) = (%d, %d), want (%d, %d)", test.args, limit, offset, test.wantLimit, test.wantOffset)
			}
		})
	}
}
