package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const tenantQuotaJSON = `{"tenant_id":"tenant_quota","workspaces_limit":10,"workload_runs_limit":5,"queued_workload_runs_limit":5,"preview_runs_limit":2,"preview_lifetime_seconds_limit":86400,"artifact_bytes_limit":10737418240,"service_memory_bytes_limit":2147483648,"service_milli_cpu_limit":1500,"revision":3}`

func TestTenantUsageAndQuotaCLIUseAuthenticatedTenantScopedAPI(t *testing.T) {
	previousGetter := cliCredentialGetter
	cliCredentialGetter = workspaceShellCredentials(`{"access_token":"quota-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	t.Cleanup(func() { cliCredentialGetter = previousGetter })

	var usageReads, quotaReads, quotaWrites int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer quota-access" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/tenant_quota/usage":
			usageReads++
			_, _ = fmt.Fprint(w, `{"workspaces_used":3,"workspaces_limit":10,"workload_runs_used":2,"workload_runs_limit":5,"queued_workload_runs_used":1,"queued_workload_runs_limit":5,"preview_runs_used":1,"preview_runs_limit":2,"preview_lifetime_seconds_limit":86400,"artifact_bytes_used":100,"artifact_bytes_reserved":200,"artifact_bytes_limit":10737418240}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/tenant_quota/quotas":
			quotaReads++
			_, _ = fmt.Fprint(w, tenantQuotaJSON)
		case r.Method == http.MethodPut && r.URL.Path == "/v1/tenants/tenant_quota/quotas":
			quotaWrites++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode quota update: %v", err)
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			if body["workspaces_limit"] != float64(8) || body["workload_runs_limit"] != float64(4) || body["queued_workload_runs_limit"] != float64(4) ||
				body["artifact_bytes_limit"] != float64(10737418240) || body["preview_runs_limit"] != float64(2) {
				t.Errorf("quota payload did not retain omitted limits: %#v", body)
				http.Error(w, "unexpected limits", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprint(w, `{"tenant_id":"tenant_quota","workspaces_limit":8,"workload_runs_limit":4,"queued_workload_runs_limit":4,"preview_runs_limit":2,"preview_lifetime_seconds_limit":86400,"artifact_bytes_limit":10737418240,"service_memory_bytes_limit":2147483648,"service_milli_cpu_limit":1500,"revision":4}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/tenant_quota/workspaces/ws_quota/services/usage":
			_, _ = fmt.Fprint(w, `{"memory_bytes_used":1073741824,"memory_bytes_limit":2147483648,"milli_cpu_used":750,"milli_cpu_limit":1500}`)
		default:
			t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)

	usage, err := tenantUsage("tenant_quota")
	if err != nil || usage.WorkspacesUsed != 3 || usage.ArtifactBytesReserved != 200 {
		t.Fatalf("tenant usage = %+v, error = %v", usage, err)
	}
	limits, err := getTenantQuotaLimits("tenant_quota")
	if err != nil || limits.Revision != 3 || limits.WorkspacesLimit != 10 {
		t.Fatalf("tenant quota limits = %+v, error = %v", limits, err)
	}
	updated, err := setTenantQuotaLimits("tenant_quota", 8, 4, 0, 0, 0, 0, 0, 0, 0, 0)
	if err != nil || updated.Revision != 4 || updated.WorkspacesLimit != 8 || updated.ArtifactBytesLimit != limits.ArtifactBytesLimit {
		t.Fatalf("updated tenant quotas = %+v, error = %v", updated, err)
	}
	serviceUsage, err := workspaceServiceResourceUsage("tenant_quota", "ws_quota")
	if err != nil || serviceUsage.MemoryBytesUsed != 1073741824 || serviceUsage.MilliCPULimit != 1500 {
		t.Fatalf("Workspace Service usage = %+v, error = %v", serviceUsage, err)
	}
	if usageReads != 1 || quotaReads != 2 || quotaWrites != 1 {
		t.Fatalf("API calls usage=%d quota-get=%d quota-put=%d", usageReads, quotaReads, quotaWrites)
	}
}

func TestQuotaAndUsageCLIFailClosedWithoutTokenOrAPIURL(t *testing.T) {
	t.Run("missing token", func(t *testing.T) {
		previousGetter := cliCredentialGetter
		cliCredentialGetter = workspaceShellCredentials("")
		t.Cleanup(func() { cliCredentialGetter = previousGetter })
		t.Setenv("HAKO_API_URL", "https://api.example")
		if _, err := tenantUsage("tenant_quota"); err == nil {
			t.Fatal("missing access token must fail")
		}
	})
	t.Run("missing API URL", func(t *testing.T) {
		previousGetter := cliCredentialGetter
		cliCredentialGetter = workspaceShellCredentials(`{"access_token":"quota-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
		t.Cleanup(func() { cliCredentialGetter = previousGetter })
		t.Setenv("HAKO_API_URL", " ")
		if _, err := getTenantQuotaLimits("tenant_quota"); err == nil {
			t.Fatal("missing API URL must fail")
		}
	})
}
