package artifactretention

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

func TestControlPlaneClientSignsScopedListingAndDeletionAcknowledgement(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	key := []byte(strings.Repeat("k", 32))
	expires := now.Add(-time.Minute)
	var ack artifactDeletionAcknowledgementForTest
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		requestID := r.Header.Get("X-Hako-Request-ID")
		timestamp := r.Header.Get("X-Hako-Timestamp")
		if r.Header.Get("X-Hako-Signature") != cleanupSignature(key, r.Method, signedURLPath(r.URL), timestamp, requestID, body) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			if r.URL.Path != "/v1/internal/resource-planes/rp_test/workload-artifacts/expired" || r.URL.Query().Get("limit") != "7" {
				t.Errorf("unexpected signed listing route: %s?%s", r.URL.Path, r.URL.RawQuery)
				http.Error(w, "wrong route", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"artifacts": []domain.WorkloadArtifact{{ID: "artifact_1", TenantID: "tenant_1", RunID: "run_1",
				StorageRef: "s3://bucket/workloads/a.log", ExpiresAt: &expires}}})
		case http.MethodPost:
			if r.URL.Path != "/v1/internal/resource-planes/rp_test/workload-artifacts/artifact_1/deleted" {
				t.Errorf("unexpected acknowledgement route: %s", r.URL.Path)
				http.Error(w, "wrong route", http.StatusBadRequest)
				return
			}
			if err := json.Unmarshal(body, &ack); err != nil {
				t.Errorf("decode cleanup acknowledgement: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"removed": true})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	client, err := NewControlPlaneClient(ControlPlaneClientConfig{Endpoint: server.URL, ResourcePlaneID: "rp_test", HMACKey: key,
		Transport: server.Client().Transport, Now: func() time.Time { return now }, Random: bytes.NewReader(bytes.Repeat([]byte{1}, 64))})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := client.ListExpired(context.Background(), now, 7)
	if err != nil || len(artifacts) != 1 || artifacts[0].ID != "artifact_1" {
		t.Fatalf("expired artifacts = %+v err=%v", artifacts, err)
	}
	removed, err := client.DeleteExpired(context.Background(), artifacts[0], now)
	if err != nil || !removed {
		t.Fatalf("acknowledge deleted artifact: removed=%t err=%v", removed, err)
	}
	if ack.TenantID != "tenant_1" || ack.RunID != "run_1" || ack.StorageRef != "s3://bucket/workloads/a.log" || !ack.ExpiresAt.Equal(expires) {
		t.Fatalf("cleanup acknowledgement did not bind candidate identity: %+v", ack)
	}
}

type artifactDeletionAcknowledgementForTest struct {
	TenantID   string    `json:"tenant_id"`
	RunID      string    `json:"workload_run_id"`
	StorageRef string    `json:"storage_ref"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func TestControlPlaneClientRejectsUnsafeEndpointAndBadCandidates(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	for _, endpoint := range []string{"http://control.test", "https://control.test/path", "https://user@control.test"} {
		if _, err := NewControlPlaneClient(ControlPlaneClientConfig{Endpoint: endpoint, ResourcePlaneID: "rp_test", HMACKey: key}); err == nil {
			t.Errorf("unsafe endpoint accepted: %q", endpoint)
		}
	}
	now := time.Now().UTC()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"artifacts": []domain.WorkloadArtifact{{ID: "artifact_1", TenantID: "tenant_1", RunID: "run_1",
			StorageRef: "s3://bucket/workloads/a.log", ExpiresAt: ptrTime(now.Add(time.Minute))}}})
	}))
	defer server.Close()
	client, err := NewControlPlaneClient(ControlPlaneClientConfig{Endpoint: server.URL, ResourcePlaneID: "rp_test", HMACKey: key,
		Transport: server.Client().Transport})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListExpired(context.Background(), now, 1); err == nil {
		t.Fatal("future-expiry artifact candidate was accepted")
	}
}

func ptrTime(value time.Time) *time.Time { return &value }
