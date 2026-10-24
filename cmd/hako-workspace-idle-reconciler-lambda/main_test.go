package main

import "testing"

func TestParseBatchSize(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int
		bad   bool
	}{{"", 100, false}, {"1", 1, false}, {"1000", 1000, false}, {"0", 0, true}, {"1001", 0, true}, {"bad", 0, true}} {
		got, err := parseBatchSize(test.value)
		if (err != nil) != test.bad || !test.bad && got != test.want {
			t.Errorf("parseBatchSize(%q) = %d, %v; want %d bad=%v", test.value, got, err, test.want, test.bad)
		}
	}
}
