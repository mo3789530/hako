package idempotency

import (
	"strings"
	"testing"
)

func TestValidateKeyBoundaries(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{name: "visible ASCII", key: "req_123-abc", want: true},
		{name: "maximum length", key: strings.Repeat("x", MaxKeyLength), want: true},
		{name: "empty", key: ""},
		{name: "too long", key: strings.Repeat("x", MaxKeyLength+1)},
		{name: "space", key: "contains space"},
		{name: "control", key: "line\nbreak"},
		{name: "non ASCII", key: "鍵"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateKey(test.key)
			if (err == nil) != test.want {
				t.Fatalf("ValidateKey(%q) error = %v, want valid=%t", test.key, err, test.want)
			}
		})
	}
}

func TestFingerprintIsStableAndDependsOnNormalizedValue(t *testing.T) {
	type request struct {
		Name  string `json:"name"`
		Class string `json:"class"`
	}
	first, err := Fingerprint(request{Name: "api", Class: "standard"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Fingerprint(request{Name: "api", Class: "standard"})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := Fingerprint(request{Name: "worker", Class: "standard"})
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first == changed || len(first) != 64 {
		t.Fatalf("unexpected fingerprints: first=%q second=%q changed=%q", first, second, changed)
	}
	if _, err := Fingerprint(make(chan int)); err == nil {
		t.Fatal("non-JSON request should fail fingerprinting")
	}
}
