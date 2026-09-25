package placementpolicies

import (
	"reflect"
	"testing"

	"github.com/mo3789530/hako/internal/domain"
)

func TestNormalizePolicy(t *testing.T) {
	got, err := Normalize(Policy{
		TenantID: "tenant_test", AllowedRegions: []string{" ap-northeast-1 ", "US-EAST-1"},
		ResourcePlaneIDs: []string{" rp-Tokyo-01 "}, RequiredCapabilities: []string{" MicroVM ", "container"},
	})
	if err != nil {
		t.Fatalf("normalize policy: %v", err)
	}
	if !reflect.DeepEqual(got.AllowedRegions, []string{"ap-northeast-1", "us-east-1"}) ||
		!reflect.DeepEqual(got.ResourcePlaneIDs, []string{"rp-Tokyo-01"}) ||
		!reflect.DeepEqual(got.RequiredCapabilities, []string{"container", "microvm"}) {
		t.Fatalf("unexpected normalized policy: %+v", got)
	}
	if got.TenantID != domain.TenantID("tenant_test") {
		t.Fatalf("Tenant ID changed during normalization: %q", got.TenantID)
	}
}

func TestNormalizeRejectsDuplicatesAfterCanonicalization(t *testing.T) {
	_, err := Normalize(Policy{TenantID: "tenant_test", AllowedRegions: []string{"US-EAST-1", "us-east-1"}})
	if err == nil {
		t.Fatal("duplicate case-folded Region should be rejected")
	}
}
