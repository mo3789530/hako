package api

import (
	"context"
	"encoding/json"
	"github.com/mo3789530/hako/internal/workspacesession"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCreateWorkloadArtifactDownloadTicketUsesExactTenantRunAndArtifactRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tenants/tenant_123/workload-runs/run_123/artifacts/artifact_123/download-ticket" ||
			r.Header.Get("Authorization") != "Bearer artifact-test-token" || r.ContentLength != 0 {
			t.Errorf("unexpected artifact ticket request: %s %s content-length=%d auth=%q", r.Method, r.URL.Path, r.ContentLength, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(WorkloadArtifactDownloadTicket{Ticket: "opaque-ticket", ExpiresAt: time.Now().Add(time.Minute),
			SizeBytes: 12, SHA256: strings.Repeat("a", 64), Kind: "agent_changes"})
	}))
	defer server.Close()
	result, err := CreateWorkloadArtifactDownloadTicket(context.Background(), server.URL, "artifact-test-token",
		"tenant_123", "run_123", "artifact_123", server.Client())
	if err != nil || result.Ticket != "opaque-ticket" || result.SizeBytes != 12 || result.Kind != "agent_changes" {
		t.Fatalf("ticket = %+v, %v", result, err)
	}
	for _, ids := range [][3]string{{"../tenant", "run_123", "artifact_123"}, {"tenant_123", "../run", "artifact_123"}, {"tenant_123", "run_123", "../artifact"}} {
		if _, err := CreateWorkloadArtifactDownloadTicket(context.Background(), server.URL, "artifact-test-token", ids[0], ids[1], ids[2], server.Client()); err == nil {
			t.Errorf("unsafe artifact identity accepted: %v", ids)
		}
	}
}

func TestWorkspaceServiceClientListsAndAddsWithTenantScopedPaths(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/tenants/tenant_123/workspaces/ws_123/services/db/actions" && r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"action":"stop"}` && string(body) != `{"action":"rotate-credentials"}` {
				t.Errorf("unexpected service action body: %s", body)
			}
			w.WriteHeader(http.StatusAccepted)
			if string(body) == `{"action":"rotate-credentials"}` {
				_, _ = w.Write([]byte(`{"id":"svc_123","workspace_id":"ws_123","name":"db","desired_state":"running","observed_state":"running","credential_generation":2}`))
			} else {
				_, _ = w.Write([]byte(`{"id":"svc_123","workspace_id":"ws_123","name":"db","desired_state":"stopped","observed_state":"pending"}`))
			}
			return
		}
		if r.URL.Path == "/v1/tenants/tenant_123/workspaces/ws_123/services" && r.Method == http.MethodPost {
			if r.Header.Get("Authorization") != "Bearer test-access-token" {
				t.Errorf("service add missing authorization: %q", r.Header.Get("Authorization"))
			}
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"catalog_name":"postgres","name":"db"}` {
				t.Errorf("unexpected service create body: %s", body)
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"svc_123","workspace_id":"ws_123","name":"db","catalog_version":"postgres/1.0.0","desired_state":"running","observed_state":"pending"}`))
			return
		}
		if r.URL.Path == "/v1/tenants/tenant_123/workspaces/ws_123/services" && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"items":[{"id":"svc_123","workspace_id":"ws_123","name":"db","desired_state":"running","observed_state":"pending"}]}`))
			return
		}
		t.Errorf("unexpected service API request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	added, err := AddWorkspaceService(context.Background(), server.URL, "test-access-token", "tenant_123", "ws_123", AddWorkspaceServiceRequest{CatalogName: "postgres", Name: "db"}, server.Client())
	if err != nil || added.ID != "svc_123" || added.ObservedState != "pending" {
		t.Fatalf("unexpected add response: %+v error=%v", added, err)
	}
	listed, err := ListWorkspaceServices(context.Background(), server.URL, "test-access-token", "tenant_123", "ws_123", server.Client())
	if err != nil || len(listed.Items) != 1 || listed.Items[0].Name != "db" {
		t.Fatalf("unexpected service list: %+v error=%v", listed, err)
	}
	stopped, err := RequestWorkspaceServiceAction(context.Background(), server.URL, "test-access-token", "tenant_123", "ws_123", "db", "stop", server.Client())
	if err != nil || stopped.DesiredState != "stopped" || stopped.ObservedState != "pending" {
		t.Fatalf("unexpected service action result: %+v error=%v", stopped, err)
	}
	rotated, err := RequestWorkspaceServiceAction(context.Background(), server.URL, "test-access-token", "tenant_123", "ws_123", "db", "rotate-credentials", server.Client())
	if err != nil || rotated.DesiredState != "running" || rotated.CredentialGeneration != 2 {
		t.Fatalf("unexpected credential rotation result: %+v error=%v", rotated, err)
	}
}

func TestAddWorkspaceServiceRejectsUnsafeInputs(t *testing.T) {
	for _, request := range []AddWorkspaceServiceRequest{{CatalogName: "postgres", Name: "DB"}, {CatalogName: "postgres;rm", Name: "db"}} {
		if _, err := AddWorkspaceService(context.Background(), "https://api.example.test", "token", "tenant_1", "ws_1", request, nil); err == nil {
			t.Fatalf("unsafe service request was accepted: %+v", request)
		}
	}
	if _, err := ListWorkspaceServices(context.Background(), "https://api.example.test", "token", "tenant_1", "ws/foreign", nil); err == nil {
		t.Fatal("unsafe Workspace ID was accepted")
	}
}

func TestGetServiceCatalogClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/service-catalog" || r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("unexpected catalog request: %s %s authorization=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = io.Copy(w, strings.NewReader(`{"schema_version":1,"items":[]}`))
	}))
	defer server.Close()
	result, err := GetServiceCatalog(context.Background(), server.URL, "token", server.Client())
	if err != nil || result.SchemaVersion != 1 || result.Items == nil {
		t.Fatalf("unexpected catalog response: %+v error=%v", result, err)
	}
}

func TestHealthClientSendsBearerToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" || r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			t.Errorf("unexpected authorization header: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	if err := Health(context.Background(), server.URL, "test-access-token", server.Client()); err != nil {
		t.Fatalf("health request failed: %v", err)
	}
}

func TestGetTenantMembershipSendsBearerTokenAndDecodesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tenants/tenant_123/membership" || r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			t.Errorf("unexpected authorization header: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(TenantMembership{TenantID: "tenant_123", UserID: "usr_123", Role: "member"})
	}))
	defer server.Close()

	membership, err := GetTenantMembership(context.Background(), server.URL, "test-access-token", "tenant_123", server.Client())
	if err != nil {
		t.Fatalf("membership request failed: %v", err)
	}
	if membership != (TenantMembership{TenantID: "tenant_123", UserID: "usr_123", Role: "member"}) {
		t.Fatalf("unexpected membership: %+v", membership)
	}
}

func TestGetTenantMembershipRejectsUnsafeTenantID(t *testing.T) {
	if _, err := GetTenantMembership(context.Background(), "https://api.example.test", "token", "tenant/other", nil); err == nil {
		t.Fatal("expected unsafe Tenant ID to be rejected")
	}
}

func TestTenantPlacementPolicyClientGetAndPut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tenants/tenant_123/placement-policy" || r.Header.Get("Authorization") != "Bearer test-access-token" {
			t.Errorf("unexpected policy request: %s %s authorization=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		if r.Method == http.MethodPut {
			var request TenantPlacementPolicy
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.AllowedRegions) != 1 || request.AllowedRegions[0] != "ap-northeast-1" || request.MaxCostTier != "standard" || request.MinimumIsolationTier != "dedicated" {
				t.Errorf("unexpected policy update body: %+v error=%v", request, err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(TenantPlacementPolicy{
			TenantID: "tenant_123", AllowedRegions: []string{"ap-northeast-1"},
			ResourcePlaneIDs: []string{}, RequiredCapabilities: []string{"microvm"},
			MaxCostTier: "standard", MinimumIsolationTier: "dedicated",
		})
	}))
	defer server.Close()

	policy, err := GetTenantPlacementPolicy(context.Background(), server.URL, "test-access-token", "tenant_123", server.Client())
	if err != nil || policy.TenantID != "tenant_123" || len(policy.AllowedRegions) != 1 {
		t.Fatalf("unexpected policy read: %+v error=%v", policy, err)
	}
	policy, err = PutTenantPlacementPolicy(context.Background(), server.URL, "test-access-token", TenantPlacementPolicy{
		TenantID: "tenant_123", AllowedRegions: []string{"ap-northeast-1"},
		MaxCostTier: "standard", MinimumIsolationTier: "dedicated",
	}, server.Client())
	if err != nil || policy.TenantID != "tenant_123" {
		t.Fatalf("unexpected policy update result: %+v error=%v", policy, err)
	}
}

func TestTenantWorkspaceIdlePolicyClientGetAndPut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tenants/tenant_123/workspace-idle-policy" || r.Header.Get("Authorization") != "Bearer access-token" {
			t.Fatalf("unexpected request path/auth: %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		if r.Method == http.MethodPut {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["enabled"] != true || body["grace_period_seconds"] != float64(900) {
				t.Fatalf("unexpected idle policy request: %+v err=%v", body, err)
			}
		}
		_ = json.NewEncoder(w).Encode(TenantWorkspaceIdlePolicy{TenantID: "tenant_123", Enabled: true, GracePeriodSeconds: 900, Revision: 1})
	}))
	defer server.Close()
	policy, err := GetTenantWorkspaceIdlePolicy(context.Background(), server.URL, "access-token", "tenant_123", server.Client())
	if err != nil || policy.Revision != 1 || !policy.Enabled {
		t.Fatalf("get idle policy = %+v, err=%v", policy, err)
	}
	policy, err = PutTenantWorkspaceIdlePolicy(context.Background(), server.URL, "access-token", TenantWorkspaceIdlePolicy{
		TenantID: "tenant_123", Enabled: true, GracePeriodSeconds: 900,
	}, server.Client())
	if err != nil || policy.Revision != 1 || !policy.Enabled {
		t.Fatalf("put idle policy = %+v, err=%v", policy, err)
	}
	if _, err := PutTenantWorkspaceIdlePolicy(context.Background(), server.URL, "access-token", TenantWorkspaceIdlePolicy{
		TenantID: "tenant_123", Enabled: true, GracePeriodSeconds: 299,
	}, server.Client()); err == nil {
		t.Fatal("client accepted an out-of-range grace period")
	}
}

func TestCreateWorkspaceSendsIdempotencyAndDecodesAsyncResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tenants/tenant_123/workspaces" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-access-token" || r.Header.Get("Idempotency-Key") != "request-key-1" {
			t.Errorf("unexpected request credentials: authorization=%q idempotency=%q", r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"))
		}
		var request struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Name != "api-dev" {
			t.Errorf("unexpected create request body: %+v, error=%v", request, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"workspace":{"id":"ws_1","tenant_id":"tenant_123","owner_id":"usr_1","name":"api-dev"},"status":{"desired_state":"running","observed_state":"pending"},"operation_id":"op_1","operation_state":"pending"}`))
	}))
	defer server.Close()

	created, err := CreateWorkspace(context.Background(), server.URL, "test-access-token", "tenant_123", "api-dev", "request-key-1", server.Client())
	if err != nil {
		t.Fatalf("create Workspace request failed: %v", err)
	}
	if created.ID != "ws_1" || created.OperationID != "op_1" || created.ObservedState != "pending" {
		t.Fatalf("unexpected Workspace result: %+v", created)
	}
}

func TestCreateWorkspacePreservesQuotaErrorCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"workspace_quota_exceeded","message":"Tenant workspace limit reached (10)"}}`))
	}))
	defer server.Close()

	_, err := CreateWorkspace(context.Background(), server.URL, "test-access-token", "tenant_123", "api-dev", "request-key-1", server.Client())
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.StatusCode != http.StatusConflict || apiErr.Code != "workspace_quota_exceeded" {
		t.Fatalf("expected typed quota error, got %T %v", err, err)
	}
}

func TestListWorkspacesSendsPageAndDecodesResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tenants/tenant_123/workspaces" || r.URL.Query().Get("limit") != "1" || r.URL.Query().Get("offset") != "2" {
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			t.Errorf("unexpected authorization header: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"workspace":{"id":"ws_1","tenant_id":"tenant_123","owner_id":"usr_1","name":"api-dev"},"status":{"desired_state":"running","observed_state":"running"}}],"total":3,"limit":1,"offset":2}`))
	}))
	defer server.Close()

	page, err := ListWorkspaces(context.Background(), server.URL, "test-access-token", "tenant_123", 1, 2, server.Client())
	if err != nil {
		t.Fatalf("list Workspaces request failed: %v", err)
	}
	if page.Total != 3 || len(page.Items) != 1 || page.Items[0].Workspace.Name != "api-dev" || page.Offset != 2 {
		t.Fatalf("unexpected Workspace page: %+v", page)
	}
}

func TestGetTenantUsageUsesScopedPathAndValidatesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tenants/tenant_123/usage" || r.Header.Get("Authorization") != "Bearer test-access-token" {
			t.Errorf("unexpected request: %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workspaces_used":3,"workspaces_limit":10,"workload_runs_used":2,"workload_runs_limit":5,"queued_workload_runs_used":1,"queued_workload_runs_limit":5,"preview_runs_used":1,"preview_runs_limit":2,"preview_lifetime_seconds_limit":86400,"artifact_bytes_used":1234,"artifact_bytes_reserved":104857600,"artifact_bytes_limit":10737418240,"workspace_compute_milli_cpu_used":1500,"workspace_compute_memory_mib_used":1600,"workspace_compute_profiles_missing":1,"workspace_compute_milli_cpu_limit":4000,"workspace_compute_memory_mib_limit":8192}`))
	}))
	defer server.Close()

	usage, err := GetTenantUsage(context.Background(), server.URL, "test-access-token", "tenant_123", server.Client())
	if err != nil || usage.WorkspacesUsed != 3 || usage.WorkspacesLimit != 10 || usage.WorkloadRunsUsed != 2 || usage.WorkloadRunsLimit != 5 || usage.QueuedWorkloadRunsUsed != 1 || usage.QueuedWorkloadRunsLimit != 5 || usage.PreviewRunsUsed != 1 || usage.PreviewRunsLimit != 2 || usage.PreviewLifetimeLimit != 86400 || usage.ArtifactBytesUsed != 1234 || usage.ArtifactBytesReserved != 104857600 || usage.ArtifactBytesLimit != 10737418240 || usage.WorkspaceComputeCPUMilliUsed != 1500 || usage.WorkspaceComputeMemoryMiBUsed != 1600 || usage.WorkspaceComputeProfilesMissing != 1 || usage.WorkspaceComputeCPUMilliLimit != 4000 || usage.WorkspaceComputeMemoryMiBLimit != 8192 {
		t.Fatalf("unexpected Tenant usage response: %+v error=%v", usage, err)
	}

	legacyOverQuota := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"workspaces_used":11,"workspaces_limit":10,"workload_runs_used":6,"workload_runs_limit":5,"queued_workload_runs_used":6,"queued_workload_runs_limit":5,"preview_runs_used":3,"preview_runs_limit":2,"preview_lifetime_seconds_limit":86400,"artifact_bytes_used":0,"artifact_bytes_limit":10737418240}`))
	}))
	defer legacyOverQuota.Close()
	if overage, err := GetTenantUsage(context.Background(), legacyOverQuota.URL, "test-access-token", "tenant_123", legacyOverQuota.Client()); err != nil || overage.WorkspacesUsed != 11 || overage.WorkloadRunsUsed != 6 || overage.PreviewRunsUsed != 3 {
		t.Fatalf("did not expose legacy over-quota usage: %+v err=%v", overage, err)
	}
}

func TestTenantQuotaClientUsesScopedOwnerRoutes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tenants/tenant_123/quotas" || r.Header.Get("Authorization") != "Bearer test-access-token" {
			t.Errorf("unexpected quota request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut {
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("quota update Content-Type = %q", r.Header.Get("Content-Type"))
			}
			var body map[string]int64
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["workspaces_limit"] != 4 || body["workload_runs_limit"] != 2 || body["queued_workload_runs_limit"] != 1 || body["preview_runs_limit"] != 3 || body["preview_lifetime_seconds_limit"] != 43200 || body["artifact_bytes_limit"] != 2147483648 || body["service_memory_bytes_limit"] != 1073741824 || body["service_milli_cpu_limit"] != 750 || body["workspace_compute_milli_cpu_limit"] != 4000 || body["workspace_compute_memory_mib_limit"] != 8192 {
				t.Errorf("quota update body = %+v err=%v", body, err)
			}
		} else if r.Method != http.MethodGet {
			t.Errorf("unexpected quota method: %s", r.Method)
		}
		_, _ = w.Write([]byte(`{"tenant_id":"tenant_123","workspaces_limit":4,"workload_runs_limit":2,"queued_workload_runs_limit":1,"preview_runs_limit":3,"preview_lifetime_seconds_limit":43200,"artifact_bytes_limit":2147483648,"service_memory_bytes_limit":1073741824,"service_milli_cpu_limit":750,"workspace_compute_milli_cpu_limit":4000,"workspace_compute_memory_mib_limit":8192,"revision":3}`))
	}))
	defer server.Close()

	limits, err := GetTenantQuotaLimits(context.Background(), server.URL, "test-access-token", "tenant_123", server.Client())
	if err != nil || limits.WorkspacesLimit != 4 || limits.WorkloadRunsLimit != 2 || limits.QueuedWorkloadRunsLimit != 1 || limits.PreviewRunsLimit != 3 || limits.PreviewLifetimeSecondsLimit != 43200 || limits.ArtifactBytesLimit != 2147483648 || limits.WorkspaceServiceMemoryBytesLimit != 1073741824 || limits.WorkspaceServiceMilliCPULimit != 750 || limits.WorkspaceComputeMilliCPULimit != 4000 || limits.WorkspaceComputeMemoryMiBLimit != 8192 || limits.Revision != 3 {
		t.Fatalf("GET quota limits = %+v err=%v", limits, err)
	}
	limits, err = SetTenantQuotaLimits(context.Background(), server.URL, "test-access-token", "tenant_123",
		TenantQuotaLimits{WorkspacesLimit: 4, WorkloadRunsLimit: 2, QueuedWorkloadRunsLimit: 1, PreviewRunsLimit: 3, PreviewLifetimeSecondsLimit: 43200, ArtifactBytesLimit: 2147483648, WorkspaceServiceMemoryBytesLimit: 1073741824, WorkspaceServiceMilliCPULimit: 750, WorkspaceComputeMilliCPULimit: 4000, WorkspaceComputeMemoryMiBLimit: 8192}, server.Client())
	if err != nil || limits.WorkspacesLimit != 4 || limits.WorkloadRunsLimit != 2 || limits.QueuedWorkloadRunsLimit != 1 || limits.PreviewRunsLimit != 3 || limits.PreviewLifetimeSecondsLimit != 43200 || limits.ArtifactBytesLimit != 2147483648 || limits.WorkspaceServiceMemoryBytesLimit != 1073741824 || limits.WorkspaceServiceMilliCPULimit != 750 || limits.WorkspaceComputeMilliCPULimit != 4000 || limits.WorkspaceComputeMemoryMiBLimit != 8192 {
		t.Fatalf("PUT quota limits = %+v err=%v", limits, err)
	}
	if _, err := SetTenantQuotaLimits(context.Background(), server.URL, "test-access-token", "tenant_123",
		TenantQuotaLimits{WorkspacesLimit: 11, WorkloadRunsLimit: 2}, server.Client()); err == nil {
		t.Fatal("quota limit above platform maximum should be rejected client-side")
	}
}

func TestGetWorkspaceServiceResourceUsageUsesScopedPathAndValidatesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tenants/tenant_123/workspaces/ws_1/services/usage" || r.Header.Get("Authorization") != "Bearer test-access-token" {
			t.Errorf("unexpected request: %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"memory_bytes_used":1073741824,"memory_bytes_limit":2147483648,"milli_cpu_used":750,"milli_cpu_limit":1500}`))
	}))
	defer server.Close()

	usage, err := GetWorkspaceServiceResourceUsage(context.Background(), server.URL, "test-access-token", "tenant_123", "ws_1", server.Client())
	if err != nil || usage.MemoryBytesUsed != 1073741824 || usage.MemoryBytesLimit != 2147483648 || usage.MilliCPUUsed != 750 || usage.MilliCPULimit != 1500 {
		t.Fatalf("unexpected Workspace Service usage response: %+v error=%v", usage, err)
	}
	if _, err := GetWorkspaceServiceResourceUsage(context.Background(), server.URL, "test-access-token", "tenant_123", "../ws_1", server.Client()); err == nil {
		t.Fatal("unsafe Workspace ID should be rejected")
	}
}

func TestGetWorkspaceUsesTenantScopedPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tenants/tenant_123/workspaces/ws_1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workspace":{"id":"ws_1","tenant_id":"tenant_123","owner_id":"usr_1","name":"api-dev"},"status":{"desired_state":"running","observed_state":"running"}}`))
	}))
	defer server.Close()

	view, err := GetWorkspace(context.Background(), server.URL, "test-access-token", "tenant_123", "ws_1", server.Client())
	if err != nil {
		t.Fatalf("get Workspace request failed: %v", err)
	}
	if view.Workspace.ID != "ws_1" || view.Workspace.Name != "api-dev" || view.Status.ObservedState != "running" {
		t.Fatalf("unexpected Workspace view: %+v", view)
	}
}

func TestWorkspaceSessionClientIssueAndRevoke(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			t.Errorf("unexpected Authorization header")
		}
		switch r.Method {
		case http.MethodPost:
			if r.URL.Path != "/v1/tenants/tenant_123/workspaces/ws_1/sessions" {
				t.Errorf("issue path = %q", r.URL.Path)
			}
			var body struct {
				Permissions []string `json:"permissions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Permissions) != 1 || body.Permissions[0] != "shell" {
				t.Errorf("issue body = %+v, error=%v", body, err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"session_id":"0123456789abcdef0123456789abcdef0123456789abcdef","ticket":"key.payload.signature","expires_at":"2026-10-12T05:00:00Z","permissions":["shell"]}`))
		case http.MethodDelete:
			if r.URL.Path != "/v1/tenants/tenant_123/workspaces/ws_1/sessions/0123456789abcdef0123456789abcdef0123456789abcdef" {
				t.Errorf("revoke path = %q", r.URL.Path)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()
	session, err := CreateWorkspaceSession(context.Background(), server.URL, "test-access-token", "tenant_123", "ws_1", []workspacesession.Permission{workspacesession.PermissionShell}, server.Client())
	if err != nil || session.SessionID == "" || session.Ticket == "" {
		t.Fatalf("create session: %+v error=%v", session, err)
	}
	if err := RevokeWorkspaceSession(context.Background(), server.URL, "test-access-token", "tenant_123", "ws_1", string(session.SessionID), server.Client()); err != nil {
		t.Fatalf("revoke session: %v", err)
	}
}

func TestWorkspaceSessionClientRejectsInvalidPermissionAndIDs(t *testing.T) {
	if _, err := CreateWorkspaceSession(context.Background(), "https://api.example.test", "token", "tenant_123", "ws_1", []workspacesession.Permission{"admin"}, nil); err == nil {
		t.Fatal("unknown session permission accepted")
	}
	if _, err := CreateWorkspaceSession(context.Background(), "https://api.example.test", "token", "tenant/other", "ws_1", nil, nil); err == nil {
		t.Fatal("unsafe Tenant ID accepted")
	}
	if err := RevokeWorkspaceSession(context.Background(), "https://api.example.test", "token", "tenant_123", "ws_1", "short", nil); err == nil {
		t.Fatal("invalid session ID accepted")
	}
}

func TestRequestWorkspaceActionSendsIdempotencyAndDecodesAcceptedOperation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tenants/tenant_123/workspaces/ws_1/actions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-access-token" || r.Header.Get("Idempotency-Key") != "request-key-2" {
			t.Errorf("unexpected request credentials: authorization=%q idempotency=%q", r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"))
		}
		var body struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Action != "suspend" {
			t.Errorf("unexpected action request: %+v, error=%v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"workspace":{"workspace":{"id":"ws_1","tenant_id":"tenant_123","owner_id":"usr_1","name":"api-dev"},"status":{"desired_state":"suspended","observed_state":"running"}},"operation_id":"op_2","operation_type":"suspend","operation_state":"pending"}`))
	}))
	defer server.Close()

	result, err := RequestWorkspaceAction(context.Background(), server.URL, "test-access-token", "tenant_123", "ws_1", "suspend", "request-key-2", server.Client())
	if err != nil {
		t.Fatalf("Workspace action request failed: %v", err)
	}
	if result.OperationID != "op_2" || result.OperationType != "suspend" || result.Workspace.Status.DesiredState != "suspended" {
		t.Fatalf("unexpected action result: %+v", result)
	}
}

func TestRequestWorkspaceActionRejectsInvalidAction(t *testing.T) {
	if _, err := RequestWorkspaceAction(context.Background(), "https://api.example.test", "token", "tenant_123", "ws_1", "restart", "request-key", nil); err == nil {
		t.Fatal("expected unsupported action to be rejected")
	}
}

func TestWorkloadRunClientListGetAndCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			t.Errorf("unexpected Authorization header %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/tenant_123/workload-runs":
			if r.URL.Query().Get("limit") != "2" || r.URL.Query().Get("offset") != "4" {
				t.Errorf("unexpected list pagination %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"items":[{"id":"run_1","tenant_id":"tenant_123","kind":"job","state":"running"}],"total":5,"limit":2,"offset":4}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/tenant_123/workload-runs/run_1":
			_, _ = w.Write([]byte(`{"id":"run_1","tenant_id":"tenant_123","kind":"job","state":"running"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tenants/tenant_123/workload-runs/run_1/cancel":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"run_1","tenant_id":"tenant_123","kind":"job","desired_state":"cancelled","state":"running"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	page, err := ListWorkloadRuns(context.Background(), server.URL, "test-access-token", "tenant_123", 2, 4, server.Client())
	if err != nil || page.Total != 5 || len(page.Items) != 1 || page.Items[0].ID != "run_1" || page.Items[0].State != "running" {
		t.Fatalf("unexpected Workload Run page: %+v error=%v", page, err)
	}
	run, err := GetWorkloadRun(context.Background(), server.URL, "test-access-token", "tenant_123", "run_1", server.Client())
	if err != nil || run.ID != "run_1" || run.State != "running" {
		t.Fatalf("unexpected Workload Run: %+v error=%v", run, err)
	}
	run, err = CancelWorkloadRun(context.Background(), server.URL, "test-access-token", "tenant_123", "run_1", server.Client())
	if err != nil || run.DesiredState != "cancelled" || run.State != "running" {
		t.Fatalf("unexpected cancellation response: %+v error=%v", run, err)
	}
}

func TestGetWorkloadAgentPublicationValidatesSafeStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tenants/tenant_123/workload-runs/run_agent/agent-publication" ||
			r.Header.Get("Authorization") != "Bearer test-access-token" {
			t.Errorf("unexpected publication request %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tenant_id":"tenant_123","workload_run_id":"run_agent","state":"pull_request_created","base_branch":"main","branch_name":"hako/agent/proposal-123","branch_commit_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","pull_request_number":42,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:01:00Z"}`))
	}))
	defer server.Close()
	status, err := GetWorkloadAgentPublication(context.Background(), server.URL, "test-access-token", "tenant_123", "run_agent", server.Client())
	if err != nil || status.State != "pull_request_created" || status.PullRequestNumber != 42 || status.BranchName != "hako/agent/proposal-123" {
		t.Fatalf("unexpected Agent publication status: %+v error=%v", status, err)
	}
}

func TestGetWorkloadAgentPublicationAcceptsOnlyConsistentStateTransitions(t *testing.T) {
	const responsePrefix = `{"tenant_id":"tenant_123","workload_run_id":"run_agent","base_branch":"main","branch_name":"hako/agent/proposal-123","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:01:00Z"`
	tests := []struct {
		name      string
		response  string
		wantError bool
	}{
		{name: "requested", response: responsePrefix + `,"state":"requested"}`},
		{name: "branch created", response: responsePrefix + `,"state":"branch_created","branch_commit_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`},
		{name: "pull request created", response: responsePrefix + `,"state":"pull_request_created","branch_commit_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","pull_request_number":42}`},
		{name: "unknown state", response: responsePrefix + `,"state":"merged","branch_commit_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","pull_request_number":42}`, wantError: true},
		{name: "wrong tenant", response: strings.Replace(responsePrefix, `"tenant_123"`, `"tenant_other"`, 1) + `,"state":"requested"}`, wantError: true},
		{name: "requested with branch commit", response: responsePrefix + `,"state":"requested","branch_commit_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, wantError: true},
		{name: "branch created without commit", response: responsePrefix + `,"state":"branch_created"}`, wantError: true},
		{name: "branch created with pull request", response: responsePrefix + `,"state":"branch_created","branch_commit_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","pull_request_number":42}`, wantError: true},
		{name: "pull request without commit", response: responsePrefix + `,"state":"pull_request_created","pull_request_number":42}`, wantError: true},
		{name: "pull request without number", response: responsePrefix + `,"state":"pull_request_created","branch_commit_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, wantError: true},
		{name: "missing timestamps", response: `{"tenant_id":"tenant_123","workload_run_id":"run_agent","state":"requested","base_branch":"main","branch_name":"hako/agent/proposal-123"}`, wantError: true},
		{name: "malformed json", response: `{`, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/tenants/tenant_123/workload-runs/run_agent/agent-publication" {
					t.Errorf("unexpected publication request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.response)
			}))
			defer server.Close()

			_, err := GetWorkloadAgentPublication(context.Background(), server.URL, "test-access-token", "tenant_123", "run_agent", server.Client())
			if (err != nil) != test.wantError {
				t.Fatalf("GetWorkloadAgentPublication error = %v, wantError %t", err, test.wantError)
			}
		})
	}
}

func TestSubmitWorkloadAgentPublicationUsesAuthenticatedIdempotentRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tenants/tenant_123/workload-runs/run_agent/agent-publication/publish" ||
			r.Header.Get("Authorization") != "Bearer test-access-token" {
			t.Errorf("unexpected publication submit request %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) != 0 {
			t.Errorf("publication submit unexpectedly sent a body: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(`{"tenant_id":"tenant_123","workload_run_id":"run_agent","state":"pull_request_created","base_branch":"main","branch_name":"hako/proposal-123","branch_commit_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","pull_request_number":42}`))
	}))
	defer server.Close()
	result, err := SubmitWorkloadAgentPublication(context.Background(), server.URL, "test-access-token", "tenant_123", "run_agent", server.Client())
	if err != nil || result.State != "pull_request_created" || result.PullRequestNumber != 42 {
		t.Fatalf("unexpected publication submit result: %+v error=%v", result, err)
	}
}

func TestSubmitWorkloadAgentPublicationAcceptsQueuedVerification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-access-token" {
			t.Errorf("unexpected queued publication request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"tenant_id":"tenant_123","workload_run_id":"run_agent","state":"verification_queued"}`))
	}))
	defer server.Close()
	result, err := SubmitWorkloadAgentPublication(context.Background(), server.URL, "test-access-token", "tenant_123", "run_agent", server.Client())
	if err != nil || result.State != "verification_queued" || result.BranchName != "" || result.BranchCommitSHA != "" || result.PullRequestNumber != 0 {
		t.Fatalf("unexpected queued Agent publication result: %+v error=%v", result, err)
	}
}

func TestCreateWorkloadRunSendsIdempotentRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tenants/tenant_123/workload-runs" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-access-token" || r.Header.Get("Idempotency-Key") != "create-request-1" {
			t.Errorf("unexpected auth/idempotency headers: %q %q", r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"))
		}
		var request WorkloadRunCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.GitHubInstallationID != 123 || request.GitHubRepositoryID != 456 || request.CommitSHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
			t.Errorf("unexpected create request: %+v error=%v", request, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"run_1","tenant_id":"tenant_123","kind":"job","state":"pending"}`))
	}))
	defer server.Close()

	run, err := CreateWorkloadRun(context.Background(), server.URL, "test-access-token", "tenant_123", "create-request-1", WorkloadRunCreateRequest{
		GitHubInstallationID: 123, GitHubRepositoryID: 456, CommitSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ref: "refs/heads/main", TimeoutSeconds: 900,
	}, server.Client())
	if err != nil || run.ID != "run_1" || run.State != "pending" {
		t.Fatalf("unexpected Workload Run create response: %+v error=%v", run, err)
	}
}

func TestWorkloadRunClientRejectsInvalidArguments(t *testing.T) {
	if _, err := ListWorkloadRuns(context.Background(), "https://api.example.test", "token", "bad/tenant", 10, 0, nil); err == nil {
		t.Fatal("ListWorkloadRuns accepted an unsafe Tenant ID")
	}
	if _, err := ListWorkloadRuns(context.Background(), "https://api.example.test", "token", "tenant_1", 101, 0, nil); err == nil {
		t.Fatal("ListWorkloadRuns accepted an excessive page size")
	}
	if _, err := GetWorkloadRun(context.Background(), "https://api.example.test", "token", "tenant_1", "../run", nil); err == nil {
		t.Fatal("GetWorkloadRun accepted an unsafe Run ID")
	}
	if _, err := CancelWorkloadRun(context.Background(), "https://api.example.test", "token", "tenant_1", "run_1", nil); err == nil {
		t.Fatal("CancelWorkloadRun accepted non-loopback HTTP URL")
	}
	if _, err := CreateWorkloadRun(context.Background(), "https://api.example.test", "token", "tenant_1", "key", WorkloadRunCreateRequest{
		GitHubInstallationID: 1, GitHubRepositoryID: 1, CommitSHA: "not-a-commit",
	}, nil); err == nil {
		t.Fatal("CreateWorkloadRun accepted a mutable/invalid commit revision")
	}
}

func TestHealthRejectsNonLoopbackHTTP(t *testing.T) {
	if err := Health(context.Background(), "http://api.example.test", "token", nil); err == nil {
		t.Fatal("expected non-loopback HTTP URL to be rejected")
	}
}
