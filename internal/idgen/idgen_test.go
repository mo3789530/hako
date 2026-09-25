package idgen

import (
	"regexp"
	"testing"
)

func TestNewCreatesPrefixedUUIDv4(t *testing.T) {
	pattern := regexp.MustCompile(`^ws_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	first, err := New("ws_")
	if err != nil {
		t.Fatal(err)
	}
	second, err := New("ws_")
	if err != nil {
		t.Fatal(err)
	}
	if !pattern.MatchString(first) || first == second {
		t.Fatalf("invalid or repeated UUIDv4 identifiers: first=%q second=%q", first, second)
	}
}
