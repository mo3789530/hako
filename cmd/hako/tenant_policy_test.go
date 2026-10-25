package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/api"
)

func TestTenantMembershipAndPlacementPolicyUseAuthenticatedTenantRoutes(t *testing.T) {
	previousGetter := cliCredentialGetter
	cliCredentialGetter = workspaceShellCredentials(`{"access_token":"tenant-policy-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	t.Cleanup(func() { cliCredentialGetter = previousGetter })

	var membershipCalls, policyReads, policyWrites int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tenant-policy-access" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/tenant_policy/membership":
			membershipCalls++
			_ = json.NewEncoder(w).Encode(api.TenantMembership{TenantID: "tenant_policy", UserID: "usr_policy", Role: "admin"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/tenant_policy/placement-policy":
			policyReads++
			_ = json.NewEncoder(w).Encode(api.TenantPlacementPolicy{TenantID: "tenant_policy", AllowedRegions: []string{"ap-northeast-1"}, ResourcePlaneIDs: []string{"rp_tokyo"}, RequiredCapabilities: []string{"microvm"}, MaxCostTier: "standard"})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/tenants/tenant_policy/placement-policy":
			policyWrites++
			var request struct {
				TenantID             string   `json:"tenant_id"`
				AllowedRegions       []string `json:"allowed_regions"`
				ResourcePlaneIDs     []string `json:"resource_plane_ids"`
				RequiredCapabilities []string `json:"required_capabilities"`
				MaxCostTier          string   `json:"max_cost_tier"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode placement policy: %v", err)
				http.Error(w, "invalid", http.StatusBadRequest)
				return
			}
			if request.TenantID != "" || len(request.AllowedRegions) != 1 || request.AllowedRegions[0] != "us-west-2" || len(request.ResourcePlaneIDs) != 1 || request.ResourcePlaneIDs[0] != "rp_oregon" || len(request.RequiredCapabilities) != 1 || request.RequiredCapabilities[0] != "microvm" || request.MaxCostTier != "economy" {
				t.Errorf("placement policy payload = %+v", request)
				http.Error(w, "invalid policy", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(api.TenantPlacementPolicy{TenantID: "tenant_policy", AllowedRegions: request.AllowedRegions, ResourcePlaneIDs: request.ResourcePlaneIDs, RequiredCapabilities: request.RequiredCapabilities, MaxCostTier: request.MaxCostTier})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)

	membership, err := tenantMembership("tenant_policy")
	if err != nil || membership.TenantID != "tenant_policy" || membership.UserID != "usr_policy" || membership.Role != "admin" {
		t.Fatalf("Tenant membership = %+v, err=%v", membership, err)
	}
	policy, err := tenantPlacementPolicy("tenant_policy", nil)
	if err != nil || policy.TenantID != "tenant_policy" || policy.AllowedRegions[0] != "ap-northeast-1" || policy.ResourcePlaneIDs[0] != "rp_tokyo" || policy.MaxCostTier != "standard" {
		t.Fatalf("placement policy GET = %+v, err=%v", policy, err)
	}
	update := &api.TenantPlacementPolicy{TenantID: "attacker_supplied_tenant", AllowedRegions: []string{"us-west-2"}, ResourcePlaneIDs: []string{"rp_oregon"}, RequiredCapabilities: []string{"microvm"}, MaxCostTier: "economy"}
	policy, err = tenantPlacementPolicy("tenant_policy", update)
	if err != nil || policy.TenantID != "tenant_policy" || policy.AllowedRegions[0] != "us-west-2" || update.TenantID != "tenant_policy" {
		t.Fatalf("placement policy PUT = %+v, request = %+v, err=%v", policy, update, err)
	}
	if membershipCalls != 1 || policyReads != 1 || policyWrites != 1 {
		t.Fatalf("API calls: membership=%d policy GET=%d policy PUT=%d", membershipCalls, policyReads, policyWrites)
	}
}

func TestTenantMembershipAndPlacementPolicyFailClosedWithoutCredentialsOrURL(t *testing.T) {
	previousGetter := cliCredentialGetter
	t.Cleanup(func() { cliCredentialGetter = previousGetter })
	t.Setenv("HAKO_API_URL", "https://api.example")
	cliCredentialGetter = workspaceShellCredentials("")
	if _, err := tenantMembership("tenant_policy"); err == nil {
		t.Fatal("Tenant membership accepted missing credentials")
	}
	if _, err := tenantPlacementPolicy("tenant_policy", nil); err == nil {
		t.Fatal("placement policy accepted missing credentials")
	}
	cliCredentialGetter = workspaceShellCredentials(`{"access_token":"tenant-policy-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	t.Setenv("HAKO_API_URL", "")
	if _, err := tenantMembership("tenant_policy"); err == nil {
		t.Fatal("Tenant membership accepted missing API URL")
	}
	if _, err := tenantPlacementPolicy("tenant_policy", &api.TenantPlacementPolicy{}); err == nil {
		t.Fatal("placement policy accepted missing API URL")
	}
}
