//go:build linux

package workspaceagent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

func TestSignedWorkspaceConnectionLeaseReporterRenewsAndReleasesExactIdentity(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	now := time.Now().UTC().Truncate(time.Second)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		id := "00112233445566778899aabbccddeeff0011223344556677"
		wantPath := "/v1/internal/resource-planes/rp-a/workspace-connection-leases/" + id
		if r.Method == http.MethodPost {
			wantPath += "/release"
		}
		if r.URL.EscapedPath() != wantPath || r.Header.Get("X-Hako-Report-ID") != id {
			t.Errorf("lease request path/id = %s / %s", r.URL.EscapedPath(), r.Header.Get("X-Hako-Report-ID"))
		}
		timestamp := r.Header.Get("X-Hako-Timestamp")
		bodyHash := sha256.Sum256(body)
		message := fmt.Sprintf("%s\n%s\n%s\n%s\n%s", r.Method, wantPath, timestamp, id, hex.EncodeToString(bodyHash[:]))
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(message))
		if !hmac.Equal(mac.Sum(nil), mustDecodeHex(t, r.Header.Get("X-Hako-Signature"))) {
			t.Error("lease request HMAC did not authenticate exact method/path/body")
		}
		var payload struct {
			SessionID            string    `json:"session_id"`
			TenantID             string    `json:"tenant_id"`
			WorkspaceID          string    `json:"workspace_id"`
			ResourcePlaneID      string    `json:"resource_plane_id"`
			MicroVMID            string    `json:"microvm_id"`
			AllocationGeneration int64     `json:"allocation_generation"`
			ExpiresAt            time.Time `json:"expires_at"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || payload.SessionID != id || payload.TenantID != "tenant-a" || payload.WorkspaceID != "ws-a" ||
			payload.ResourcePlaneID != "rp-a" || payload.MicroVMID != "vm-a" || payload.AllocationGeneration != 7 || !payload.ExpiresAt.After(now) {
			t.Errorf("lease identity/expiry mismatch: %+v err=%v", payload, err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	reporter := &signedWorkspaceConnectionLeaseReporter{endpoint: server.URL, key: key, client: server.Client(), now: func() time.Time { return now },
		newID: func() ([]byte, error) {
			return mustDecodeHex(t, "00112233445566778899aabbccddeeff0011223344556677"), nil
		}}
	identity := WorkspaceConnectionLeaseIdentity{SessionID: "00112233445566778899aabbccddeeff0011223344556677", TenantID: domain.TenantID("tenant-a"), WorkspaceID: domain.WorkspaceID("ws-a"),
		ResourcePlaneID: domain.ResourcePlaneID("rp-a"), MicroVMID: "vm-a", AllocationGeneration: 7}
	lease, err := reporter.Open(context.Background(), identity)
	if err != nil {
		t.Fatalf("Open lease: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("initial lease was not created before returning handle: requests=%d", requests.Load())
	}
	if err := lease.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh lease: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("release lease: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("idempotent release: %v", err)
	}
	if requests.Load() != 3 {
		t.Fatalf("requests=%d, want initial + heartbeat + one idempotent release", requests.Load())
	}
}

func TestWorkspaceConnectionLeaseReporterRejectsInvalidEndpointAndIdentity(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	if _, err := NewSignedWorkspaceConnectionLeaseReporter("http://control.example", key, nil); err == nil {
		t.Fatal("accepted cleartext lease endpoint")
	}
	if _, err := NewSignedWorkspaceConnectionLeaseReporter("https://control.example/path", key, nil); err == nil {
		t.Fatal("accepted lease endpoint with path")
	}
	reporter, err := NewSignedWorkspaceConnectionLeaseReporter("https://control.example", key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reporter.Open(context.Background(), WorkspaceConnectionLeaseIdentity{TenantID: "tenant-a", WorkspaceID: "ws-a",
		ResourcePlaneID: "rp-a", MicroVMID: "vm-a", AllocationGeneration: 0}); err == nil {
		t.Fatal("accepted zero allocation generation")
	}
}

func mustDecodeHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}
