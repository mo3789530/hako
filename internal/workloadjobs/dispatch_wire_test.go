package workloadjobs_test

import (
	"encoding/json"
	"testing"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadcontroller"
	"github.com/mo3789530/hako/internal/workloadjobs"
)

func TestControlPlaneDispatchEnvelopeMatchesResourcePlaneIdentityDecoder(t *testing.T) {
	command := workloadjobs.DispatchCommand{
		SchemaVersion: 1, TenantID: domain.TenantID("tenant_1"), WorkloadRunID: domain.WorkloadRunID("run_1"),
		AttemptID: "attempt_1", Generation: 1, ResourcePlane: domain.ResourcePlaneID("rp_1"), IdempotencyToken: "token_1",
	}
	payload, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := workloadcontroller.Decode(payload)
	if err != nil {
		t.Fatalf("Control Plane dispatch payload was rejected by Resource Plane: %v; payload=%s", err, payload)
	}
	if decoded.TenantID != "tenant_1" || decoded.WorkloadRunID != "run_1" || decoded.AttemptID != "attempt_1" ||
		decoded.Generation != 1 || decoded.ResourcePlaneID != "rp_1" || decoded.IdempotencyToken != "token_1" {
		t.Fatalf("decoded identity = %+v", decoded)
	}
}
