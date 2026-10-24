//go:build integration

package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/workspaceactivity"
	"github.com/mo3789530/hako/internal/testutil"
	"github.com/mo3789530/hako/internal/workspaceagent"
)

func TestResourcePlaneWorkspaceConnectionLeaseHMACLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants (id, name, created_at) VALUES ('tenant_lease_api', 'lease api', $1)`, []any{now}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('user_lease_api', 'lease-api', '', $1)`, []any{now}},
		{`INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES ('rp_lease_api', 'aws', 'ap-northeast-1', '[]')`, nil},
		{`INSERT INTO workspaces (id, tenant_id, owner_id, name, runtime_class, image, created_at, updated_at)
			VALUES ('ws_lease_api', 'tenant_lease_api', 'user_lease_api', 'lease', 'standard', 'image:test', $1, $1)`, []any{now}},
		{`INSERT INTO placements (workspace_id, resource_plane_id, placed_at) VALUES ('ws_lease_api', 'rp_lease_api', $1)`, []any{now}},
		{`INSERT INTO workspace_status (workspace_id, desired_state, observed_state, updated_at)
			VALUES ('ws_lease_api', 'running', 'running', $1)`, []any{now}},
		{`INSERT INTO resource_bindings (workspace_id, resource_plane_id, kind, external_id, created_at, allocation_generation)
			VALUES ('ws_lease_api', 'rp_lease_api', 'lambda_microvm', 'vm_lease_api', $1, 1)`, []any{now}},
	} {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed connection lease API: %v\nquery: %s", err, statement.query)
		}
	}
	key := []byte("workspace-lease-api-integration-key-000000000000000000")
	resolveKey := func(_ context.Context, id domain.ResourcePlaneID) ([]byte, error) {
		if id != "rp_lease_api" {
			return nil, fmt.Errorf("unknown Resource Plane")
		}
		return key, nil
	}
	e := echo.New()
	e.PUT("/v1/internal/resource-planes/:resource_plane_id/workspace-connection-leases/:lease_id",
		workspaceConnectionLeaseHandler(pool, resolveKey, false))
	e.POST("/v1/internal/resource-planes/:resource_plane_id/workspace-connection-leases/:lease_id/release",
		workspaceConnectionLeaseHandler(pool, resolveKey, true))
	server := httptest.NewTLSServer(e)
	defer server.Close()
	leaseID := "0123456789abcdef0123456789abcdef0123456789abcdef"
	body, err := json.Marshal(workspaceConnectionLeaseRequest{TenantID: "tenant_lease_api", WorkspaceID: "ws_lease_api",
		ResourcePlaneID: "rp_lease_api", MicroVMID: "vm_lease_api", AllocationGeneration: 1, ExpiresAt: now.Add(90 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	callLease := func(method, id, suffix string, body []byte, validSignature bool) *httptest.ResponseRecorder {
		path := "/v1/internal/resource-planes/rp_lease_api/workspace-connection-leases/" + id + suffix
		timestamp := fmt.Sprint(time.Now().UTC().Unix())
		request := httptest.NewRequest(method, path, bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(healthReportTimestampHeader, timestamp)
		request.Header.Set(healthReportIDHeader, id)
		bodyHash := sha256.Sum256(body)
		message := fmt.Sprintf("%s\n%s\n%s\n%s\n%s", method, request.URL.EscapedPath(), timestamp, id, hex.EncodeToString(bodyHash[:]))
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(message))
		signature := hex.EncodeToString(mac.Sum(nil))
		if !validSignature {
			signature = "invalid"
		}
		request.Header.Set(healthReportSignatureHeader, signature)
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, request)
		return recorder
	}
	call := func(method, suffix string, body []byte, validSignature bool) *httptest.ResponseRecorder {
		return callLease(method, leaseID, suffix, body, validSignature)
	}
	if response := call(http.MethodPut, "", body, true); response.Code != http.StatusNoContent {
		t.Fatalf("lease renew response = %d %s", response.Code, response.Body.String())
	}
	if response := call(http.MethodPut, "", body, false); response.Code != http.StatusUnauthorized {
		t.Fatalf("bad lease HMAC response = %d %s", response.Code, response.Body.String())
	}
	if response := call(http.MethodPost, "/release", body, true); response.Code != http.StatusNoContent {
		t.Fatalf("lease release response = %d %s", response.Code, response.Body.String())
	}
	if response := call(http.MethodPut, "", body, true); response.Code != http.StatusConflict {
		t.Fatalf("renew after release response = %d %s, want conflict", response.Code, response.Body.String())
	}

	reporter, err := workspaceagent.NewSignedWorkspaceConnectionLeaseReporter(server.URL, key, server.Client())
	if err != nil {
		t.Fatalf("construct signed Gateway lease reporter: %v", err)
	}
	lease, err := reporter.Open(ctx, workspaceagent.WorkspaceConnectionLeaseIdentity{TenantID: "tenant_lease_api", WorkspaceID: "ws_lease_api",
		ResourcePlaneID: "rp_lease_api", MicroVMID: "vm_lease_api", AllocationGeneration: 1})
	if err != nil {
		t.Fatalf("Gateway lease reporter create: %v", err)
	}
	if err := lease.Refresh(ctx); err != nil {
		t.Fatalf("Gateway lease reporter heartbeat: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("Gateway lease reporter release: %v", err)
	}
	var released, generation int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE released_at IS NOT NULL), MAX(allocation_generation)
		FROM workspace_connection_leases WHERE tenant_id = 'tenant_lease_api' AND workspace_id = 'ws_lease_api'`).Scan(&released, &generation); err != nil {
		t.Fatalf("read Gateway-reported lease state: %v", err)
	}
	if released != 2 || generation != 1 {
		t.Fatalf("Gateway→Control Plane→PostgreSQL lifecycle released=%d generation=%d, want 2 released rows and generation 1", released, generation)
	}
	for i := 0; i < workspaceactivity.MaxActiveLeasesPerWorkspace; i++ {
		id := fmt.Sprintf("%048x", i+100)
		if response := callLease(http.MethodPut, id, "", body, true); response.Code != http.StatusNoContent {
			t.Fatalf("shared active connection %d response=%d %s", i+1, response.Code, response.Body.String())
		}
	}
	if response := callLease(http.MethodPut, fmt.Sprintf("%048x", 100+workspaceactivity.MaxActiveLeasesPerWorkspace), "", body, true); response.Code != http.StatusTooManyRequests {
		t.Fatalf("connection over shared Workspace cap response=%d %s, want 429", response.Code, response.Body.String())
	}
}
