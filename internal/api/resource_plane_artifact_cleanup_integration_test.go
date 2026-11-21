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

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/mo3789530/hako/internal/artifactretention"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestResourcePlaneArtifactCleanupIsSignedPlaneScopedAndIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	expiresAt := now.Add(-time.Minute)
	seeds := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_cleanup_api','Cleanup API',$1)`, []any{now}},
		{`INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES ('rp_cleanup_a','aws','ap-northeast-1','{}'),('rp_cleanup_b','aws','us-east-1','{}')`, nil},
		{`INSERT INTO workload_runs(id,tenant_id,kind,ref,runtime_class,trust_level,desired_state,state,resource_plane_id,idempotency_key,request_hash,timeout_seconds,timeout_at,created_at,updated_at)
		 VALUES ('run_cleanup_a','tenant_cleanup_api','job','refs/heads/main','standard','untrusted','running','succeeded','rp_cleanup_a','key_cleanup_a','hash_cleanup_a',600,$1,$2,$2),
		 ('run_cleanup_b','tenant_cleanup_api','job','refs/heads/main','standard','untrusted','running','succeeded','rp_cleanup_b','key_cleanup_b','hash_cleanup_b',600,$1,$2,$2)`, []any{now.Add(time.Hour), now}},
		{`INSERT INTO workload_artifacts(id,tenant_id,workload_run_id,kind,storage_ref,size_bytes,sha256,redacted,expires_at,created_at)
		 VALUES ('artifact_cleanup_a','tenant_cleanup_api','run_cleanup_a','logs','s3://bucket/workloads/a.txt',1,$1,true,$2,$3),
		 ('artifact_cleanup_b','tenant_cleanup_api','run_cleanup_b','logs','s3://bucket/workloads/b.txt',1,$1,true,$2,$3)`, []any{strings.Repeat("a", 64), expiresAt, now.Add(-time.Hour)}},
	}
	for _, seed := range seeds {
		if _, err := pool.Exec(ctx, seed.query, seed.args...); err != nil {
			t.Fatalf("seed Resource Plane artifact cleanup: %v", err)
		}
	}
	key := []byte(strings.Repeat("k", 32))
	handler := NewHandler(nil, pool, WorkspaceCreateConfig{ResourcePlaneArtifactCleanupKeyResolver: func(_ context.Context, planeID domain.ResourcePlaneID) ([]byte, error) {
		if planeID != "rp_cleanup_a" {
			return nil, fmt.Errorf("unknown Resource Plane")
		}
		return key, nil
	}})
	request := func(method, route, requestID string, body []byte, signature string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, route, strings.NewReader(string(body)))
		if body == nil {
			req = httptest.NewRequest(method, route, nil)
		}
		req.Header.Set(healthReportTimestampHeader, fmt.Sprintf("%d", now.Unix()))
		req.Header.Set(workloadSpecRequestIDHeader, requestID)
		req.Header.Set(healthReportSignatureHeader, signature)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	sign := func(method, route, requestID string, body []byte) string {
		bodyHash := sha256.Sum256(body)
		mac := hmac.New(sha256.New, key)
		_, _ = fmt.Fprintf(mac, "%s\n%s\n%d\n%s\n%s", method, route, now.Unix(), requestID, hex.EncodeToString(bodyHash[:]))
		return hex.EncodeToString(mac.Sum(nil))
	}
	listPath := "/v1/internal/resource-planes/rp_cleanup_a/workload-artifacts/expired?limit=10"
	list := request(http.MethodGet, listPath, "req_cleanup_list_123456", nil, sign(http.MethodGet, listPath, "req_cleanup_list_123456", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list artifacts = HTTP %d %s", list.Code, list.Body.String())
	}
	var result struct {
		Artifacts []domain.WorkloadArtifact `json:"artifacts"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &result); err != nil || len(result.Artifacts) != 1 || result.Artifacts[0].ID != "artifact_cleanup_a" {
		t.Fatalf("Resource Plane received cross-plane artifact candidates: %+v err=%v", result, err)
	}
	if response := request(http.MethodGet, listPath, "req_cleanup_bad_hmac_123", nil, strings.Repeat("0", 64)); response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid HMAC returned HTTP %d", response.Code)
	}
	tamperedListPath := strings.Replace(listPath, "limit=10", "limit=9", 1)
	if response := request(http.MethodGet, tamperedListPath, "req_cleanup_tampered_query_123", nil,
		sign(http.MethodGet, listPath, "req_cleanup_tampered_query_123", nil)); response.Code != http.StatusUnauthorized {
		t.Fatalf("tampered signed query returned HTTP %d", response.Code)
	}
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	cleanupClient, err := artifactretention.NewControlPlaneClient(artifactretention.ControlPlaneClientConfig{
		Endpoint: server.URL, ResourcePlaneID: "rp_cleanup_a", HMACKey: key, Transport: server.Client().Transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	objects := &cleanupS3Recorder{}
	worker, err := artifactretention.New(cleanupClient, objects, artifactretention.Config{Bucket: "bucket", KeyPrefix: "workloads/", BatchSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	report, err := worker.RunOnce(ctx, now)
	if err != nil || report != (artifactretention.Report{Candidates: 1, Deleted: 1}) {
		t.Fatalf("Resource Plane cleanup worker = %+v err=%v", report, err)
	}
	if len(objects.inputs) != 1 || *objects.inputs[0].Key != "workloads/a.txt" {
		t.Fatalf("Resource Plane S3 deletion request = %+v", objects.inputs)
	}
	replay, err := cleanupClient.DeleteExpired(ctx, result.Artifacts[0], now)
	if err != nil || replay {
		t.Fatalf("cleanup acknowledgement replay removed=%t err=%v", replay, err)
	}
	crossPlaneBody, _ := json.Marshal(artifactDeletionAcknowledgement{TenantID: "tenant_cleanup_api", RunID: "run_cleanup_b",
		StorageRef: "s3://bucket/workloads/b.txt", ExpiresAt: expiresAt})
	crossPlanePath := "/v1/internal/resource-planes/rp_cleanup_a/workload-artifacts/artifact_cleanup_b/deleted"
	crossID := "req_cleanup_cross_plane_123"
	cross := request(http.MethodPost, crossPlanePath, crossID, crossPlaneBody, sign(http.MethodPost, crossPlanePath, crossID, crossPlaneBody))
	if cross.Code != http.StatusOK || !strings.Contains(cross.Body.String(), `"removed":false`) {
		t.Fatalf("cross-plane cleanup ack = HTTP %d %s", cross.Code, cross.Body.String())
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workload_artifacts`).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("remaining artifacts=%d err=%v, want other plane artifact retained", remaining, err)
	}
}

type cleanupS3Recorder struct{ inputs []*s3.DeleteObjectInput }

func (s *cleanupS3Recorder) DeleteObject(_ context.Context, input *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	s.inputs = append(s.inputs, input)
	return &s3.DeleteObjectOutput{}, nil
}
