package scheduler

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

type guardTx struct{ pgx.Tx }

func TestPlacementFilters(t *testing.T) {
	tests := []struct {
		name string
		got  bool
		want bool
	}{
		{"no plane restriction", matchesTenantPlane("rp-a", nil), true},
		{"allowed plane", matchesTenantPlane("rp-a", []string{"rp-b", "rp-a"}), true},
		{"disallowed plane", matchesTenantPlane("rp-a", []string{"rp-b"}), false},
		{"no region restriction", matchesTenantRegion("ap-northeast-1", nil), true},
		{"region is case insensitive", matchesTenantRegion("AP-NORTHEAST-1", []string{"ap-northeast-1"}), true},
		{"disallowed region", matchesTenantRegion("us-east-1", []string{"ap-northeast-1"}), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("got %t, want %t", test.got, test.want)
			}
		})
	}
}

func TestContainsCapabilitiesNormalizesValues(t *testing.T) {
	required := map[string]struct{}{"microvm": {}, "private-network": {}}
	if !containsCapabilities([]string{" MicroVM ", "private-network", "container"}, required) {
		t.Fatal("normalized available capabilities should satisfy requirements")
	}
	if containsCapabilities([]string{"microvm"}, required) {
		t.Fatal("missing capability must reject candidate")
	}
	if !containsCapabilities(nil, nil) {
		t.Fatal("no requirements should match any candidate")
	}
}

func TestSelectRejectsNilTransactionAndEmptyCapability(t *testing.T) {
	if _, err := Select(t.Context(), nil, Policy{}); err == nil {
		t.Fatal("nil transaction should fail")
	}
	if _, err := Select(t.Context(), guardTx{}, Policy{RequiredCapabilities: []string{"  "}}); err == nil {
		t.Fatal("empty capability should fail before transaction access")
	}
}
