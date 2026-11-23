package workloadagentcore

import (
	"strings"
	"testing"

	"github.com/mo3789530/hako/internal/workloadjournal"
)

func TestSessionIDBindsEveryAttemptField(t *testing.T) {
	base := workloadjournal.Command{TenantID: "tenant", WorkloadRunID: "run", AttemptID: "attempt",
		Generation: 1, ResourcePlaneID: "plane", IdempotencyToken: "token"}
	want, err := SessionID(base)
	if err != nil || len(want) < 33 || len(want) > 256 || strings.Contains(want, "tenant") {
		t.Fatalf("invalid session ID %q: %v", want, err)
	}
	again, err := SessionID(base)
	if err != nil || again != want {
		t.Fatalf("session ID is not deterministic: %q, %v", again, err)
	}
	variants := []workloadjournal.Command{base, base, base, base, base, base}
	variants[0].TenantID = "other"
	variants[1].WorkloadRunID = "other"
	variants[2].AttemptID = "other"
	variants[3].Generation++
	variants[4].ResourcePlaneID = "other"
	variants[5].IdempotencyToken = "other"
	for _, variant := range variants {
		got, err := SessionID(variant)
		if err != nil || got == want {
			t.Fatalf("different attempt mapped to same session: %q, %v", got, err)
		}
	}
}

func TestSessionIDRejectsInvalidIdentity(t *testing.T) {
	base := workloadjournal.Command{TenantID: "tenant", WorkloadRunID: "run", AttemptID: "attempt",
		Generation: 1, ResourcePlaneID: "plane", IdempotencyToken: "token"}
	variants := []workloadjournal.Command{base, base, base, base, base, base}
	variants[0].TenantID = ""
	variants[1].WorkloadRunID = "bad/path"
	variants[2].AttemptID = strings.Repeat("a", 129)
	variants[3].Generation = 0
	variants[4].ResourcePlaneID = ""
	variants[5].IdempotencyToken = ""
	for _, variant := range variants {
		if _, err := SessionID(variant); err == nil {
			t.Fatalf("accepted invalid identity: %+v", variant)
		}
	}
}

func TestStopClientTokenIsStableAndAttemptBound(t *testing.T) {
	base := workloadjournal.Command{TenantID: "tenant", WorkloadRunID: "run", AttemptID: "attempt",
		Generation: 1, ResourcePlaneID: "plane", IdempotencyToken: "token"}
	want, err := StopClientToken(base)
	if err != nil || len(want) < 33 || len(want) > 256 || strings.Contains(want, "tenant") {
		t.Fatalf("invalid stop client token %q: %v", want, err)
	}
	again, err := StopClientToken(base)
	if err != nil || again != want {
		t.Fatalf("stop token is not stable for an attempt: %q, %v", again, err)
	}
	base.Generation++
	changed, err := StopClientToken(base)
	if err != nil || changed == want {
		t.Fatalf("different attempt reused stop token: %q, %v", changed, err)
	}
}
