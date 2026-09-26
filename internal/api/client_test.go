package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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

func TestHealthRejectsNonLoopbackHTTP(t *testing.T) {
	if err := Health(context.Background(), "http://api.example.test", "token", nil); err == nil {
		t.Fatal("expected non-loopback HTTP URL to be rejected")
	}
}
