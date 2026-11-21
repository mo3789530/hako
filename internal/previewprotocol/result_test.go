package previewprotocol

import (
	"reflect"
	"testing"
	"time"
)

func TestRuntimeResultStrictRoundTripAndIdentityFence(t *testing.T) {
	now := time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)
	result := validResult(now)
	body, err := EncodeResult(result)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeResult(body)
	if err != nil || !reflect.DeepEqual(decoded, result) {
		t.Fatalf("result round trip = %+v, %v", decoded, err)
	}
	if _, err := DecodeResult(append(body[:len(body)-1], []byte(`,"unknown":true}`)...)); err == nil {
		t.Fatal("result decoder accepted an unknown field")
	}
	if _, err := DecodeResult(append(body, []byte(` {}`)...)); err == nil {
		t.Fatal("result decoder accepted trailing JSON")
	}
}

func TestRuntimeResultRequiresActionSpecificBindingAndErrorCode(t *testing.T) {
	now := time.Now().UTC()
	for _, result := range []Result{
		func() Result { r := validResult(now); r.Binding = nil; return r }(),
		func() Result { r := validResult(now); r.ErrorCode = "provider secret detail"; return r }(),
		func() Result { r := validResult(now); r.Action = ActionCleanup; return r }(),
		func() Result {
			r := validResult(now)
			r.Status = "failed"
			r.Binding = nil
			r.ErrorCode = "unknown"
			return r
		}(),
	} {
		if err := result.Validate(); err == nil {
			t.Errorf("accepted invalid Preview result: %+v", result)
		}
	}
	failed := validResult(now)
	failed.Status, failed.Binding, failed.ErrorCode = "failed", nil, "gateway_route_publish_failed"
	if err := failed.Validate(); err != nil {
		t.Fatalf("valid fixed failure result rejected: %v", err)
	}
}

func validResult(now time.Time) Result {
	return Result{
		SchemaVersion: 1, Action: ActionProvision, TenantID: "tenant_1", InstallationID: 17,
		RepositoryID: 23, PullRequestNumber: 4, Generation: 2,
		RuntimeWorkspaceID: "prvws_0123456789abcdef0123456789abcdef", ResourcePlaneID: "rp-test",
		ClaimID: "preview-claim-1234", ClaimAttempt: 1, LeaseExpiresAt: now.Add(time.Minute),
		Status: "succeeded", Binding: &RuntimeBinding{MicroVMID: "microvm-1", GatewayRouteID: "route-1"}, CompletedAt: now,
	}
}
