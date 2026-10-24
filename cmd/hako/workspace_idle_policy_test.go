package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/api"
)

func TestWorkspaceIdlePolicyCLIUsesTenantScopedAuthenticatedAPI(t *testing.T) {
	previousGetter := cliCredentialGetter
	cliCredentialGetter = workspaceShellCredentials(`{"access_token":"idle-policy-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	t.Cleanup(func() { cliCredentialGetter = previousGetter })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tenants/tenant_idle/workspace-idle-policy" {
			t.Errorf("policy path = %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer idle-policy-access" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(api.TenantWorkspaceIdlePolicy{
				TenantID: "tenant_idle", Enabled: false, GracePeriodSeconds: 900, Revision: 0,
			})
		case http.MethodPut:
			var request struct {
				Enabled            bool `json:"enabled"`
				GracePeriodSeconds int  `json:"grace_period_seconds"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode policy request: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if !request.Enabled || request.GracePeriodSeconds != 1800 {
				t.Errorf("policy request = %+v", request)
				http.Error(w, "bad policy", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(api.TenantWorkspaceIdlePolicy{
				TenantID: "tenant_idle", Enabled: request.Enabled, GracePeriodSeconds: request.GracePeriodSeconds, Revision: 1,
			})
		default:
			t.Errorf("method = %s", r.Method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)

	got, err := tenantWorkspaceIdlePolicy("tenant_idle", nil)
	if err != nil || got.Enabled || got.GracePeriodSeconds != 900 || got.TenantID != "tenant_idle" {
		t.Fatalf("get idle policy = %+v, error = %v", got, err)
	}
	update := &api.TenantWorkspaceIdlePolicy{TenantID: "attacker_supplied_tenant", Enabled: true, GracePeriodSeconds: 1800}
	got, err = tenantWorkspaceIdlePolicy("tenant_idle", update)
	if err != nil || !got.Enabled || got.GracePeriodSeconds != 1800 || got.Revision != 1 || update.TenantID != "tenant_idle" {
		t.Fatalf("set idle policy = %+v, input = %+v, error = %v", got, update, err)
	}
}

func TestWorkspaceIdlePolicyCLIFailsClosedWithoutConfiguration(t *testing.T) {
	t.Run("missing token", func(t *testing.T) {
		previousGetter := cliCredentialGetter
		cliCredentialGetter = workspaceShellCredentials("")
		t.Cleanup(func() { cliCredentialGetter = previousGetter })
		t.Setenv("HAKO_API_URL", "https://api.example")
		if _, err := tenantWorkspaceIdlePolicy("tenant_idle", nil); err == nil {
			t.Fatal("missing token must fail")
		}
	})
	t.Run("missing API URL", func(t *testing.T) {
		previousGetter := cliCredentialGetter
		cliCredentialGetter = workspaceShellCredentials(`{"access_token":"idle-policy-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
		t.Cleanup(func() { cliCredentialGetter = previousGetter })
		t.Setenv("HAKO_API_URL", "")
		if _, err := tenantWorkspaceIdlePolicy("tenant_idle", nil); err == nil {
			t.Fatal("missing API URL must fail")
		}
	})
}
