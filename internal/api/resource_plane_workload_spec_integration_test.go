//go:build integration

package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestResourcePlaneWorkloadSpecRouteAuthenticatesAndRevalidatesCurrentAttempt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	seeds := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_spec_api','Spec API',$1)`, []any{now}},
		{`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_spec_api','spec-api-subject','',$1)`, []any{now}},
		{`INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES ('rp_spec_api','aws','ap-northeast-1','{}')`, nil},
		{`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_spec_api',171,'acme','active','usr_spec_api',$1,$1)`, []any{now}},
		{`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (171,'tenant_spec_api',$1)`, []any{now}},
		{`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at) VALUES ('tenant_spec_api',171,181,'acme','api','main',$1)`, []any{now}},
		{`INSERT INTO workload_runs(id,tenant_id,kind,github_installation_id,github_repository_id,correlation_id,commit_sha,ref,runtime_class,trust_level,desired_state,state,resource_plane_id,idempotency_key,request_hash,timeout_seconds,timeout_at,revision,created_at,updated_at)
		 VALUES ('run_spec_api','tenant_spec_api','job',171,181,'req_22222222-2222-4222-8222-222222222222',$1,'refs/heads/main','standard','untrusted','running','provisioning','rp_spec_api','key_spec_api','hash_spec_api',600,$2,2,$3,$3)`, []any{strings.Repeat("b", 40), now.Add(10 * time.Minute), now}},
		{`INSERT INTO workload_run_attempts(id,tenant_id,workload_run_id,generation,attempt_number,resource_plane_id,idempotency_token,state,created_at,updated_at)
		 VALUES ('attempt_spec_api','tenant_spec_api','run_spec_api',1,1,'rp_spec_api','token_spec_api','dispatch_pending',$1,$1)`, []any{now}},
	}
	for _, seed := range seeds {
		if _, err := pool.Exec(ctx, seed.query, seed.args...); err != nil {
			t.Fatalf("seed Resource Plane Workload spec: %v", err)
		}
	}

	key := []byte(strings.Repeat("k", 32))
	handler := NewHandler(nil, pool, WorkspaceCreateConfig{
		ResourcePlaneWorkloadSpecKeyResolver: func(_ context.Context, planeID domain.ResourcePlaneID) ([]byte, error) {
			if planeID != "rp_spec_api" {
				return nil, fmt.Errorf("unknown Resource Plane")
			}
			return key, nil
		},
	})
	path := "/v1/internal/resource-planes/rp_spec_api/tenants/tenant_spec_api/workloads/run_spec_api/attempts/attempt_spec_api/generations/1/specification"
	request := func(route, timestamp, requestID, signature string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, route, nil)
		req.Header.Set(healthReportTimestampHeader, timestamp)
		req.Header.Set(workloadSpecRequestIDHeader, requestID)
		req.Header.Set(healthReportSignatureHeader, signature)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	sign := func(route, timestamp, requestID string) string {
		t.Helper()
		mac := hmac.New(sha256.New, key)
		bodyHash := sha256.Sum256(nil)
		_, _ = fmt.Fprintf(mac, "%s\n%s\n%s\n%s\n%s", http.MethodGet, route, timestamp, requestID, hex.EncodeToString(bodyHash[:]))
		return hex.EncodeToString(mac.Sum(nil))
	}
	timestamp := fmt.Sprintf("%d", now.Unix())
	requestID := "req_spec_api_123456789"
	valid := request(path, timestamp, requestID, sign(path, timestamp, requestID))
	if valid.Code != http.StatusOK {
		t.Fatalf("valid Resource Plane request = HTTP %d %s", valid.Code, valid.Body.String())
	}
	var metadata resourcePlaneWorkloadSpecResponse
	if err := json.Unmarshal(valid.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.SchemaVersion != 4 || metadata.Kind != domain.WorkloadJob || metadata.TenantID != "tenant_spec_api" || metadata.WorkloadRunID != "run_spec_api" ||
		metadata.AttemptID != "attempt_spec_api" || metadata.Generation != 1 || metadata.ResourcePlaneID != "rp_spec_api" ||
		metadata.CorrelationID != "req_22222222-2222-4222-8222-222222222222" ||
		metadata.GitHubOwner != "acme" || metadata.GitHubRepository != "api" || metadata.CommitSHA != strings.Repeat("b", 40) ||
		metadata.RuntimeClass != "standard" || !metadata.RequestedAt.Equal(now) || !metadata.Deadline.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("unexpected API execution metadata: %+v", metadata)
	}
	if strings.Contains(valid.Body.String(), "secret") || strings.Contains(valid.Body.String(), "artifact_prefix") || strings.Contains(valid.Body.String(), "image_identifier") {
		t.Fatalf("Control Plane leaked a credential or Resource Plane-owned setting: %s", valid.Body.String())
	}

	if response := request(path, timestamp, requestID, strings.Repeat("0", 64)); response.Code != http.StatusUnauthorized {
		t.Fatalf("bad HMAC returned HTTP %d", response.Code)
	}
	staleTimestamp := fmt.Sprintf("%d", now.Add(-6*time.Minute).Unix())
	if response := request(path, staleTimestamp, "req_spec_api_stale_123", sign(path, staleTimestamp, "req_spec_api_stale_123")); response.Code != http.StatusUnauthorized {
		t.Fatalf("stale signed request returned HTTP %d", response.Code)
	}
	wrongPlane := strings.Replace(path, "rp_spec_api", "rp_other_api", 1)
	if response := request(wrongPlane, timestamp, "req_spec_api_plane_123", sign(wrongPlane, timestamp, "req_spec_api_plane_123")); response.Code != http.StatusUnauthorized {
		t.Fatalf("unconfigured Resource Plane returned HTTP %d", response.Code)
	}

	if _, err := pool.Exec(ctx, `UPDATE workload_runs SET desired_state='cancelled' WHERE id='run_spec_api'`); err != nil {
		t.Fatal(err)
	}
	if response := request(path, timestamp, "req_spec_api_cancelled_123", sign(path, timestamp, "req_spec_api_cancelled_123")); response.Code != http.StatusNotFound {
		t.Fatalf("cancelled Run returned metadata: HTTP %d %s", response.Code, response.Body.String())
	}
}
