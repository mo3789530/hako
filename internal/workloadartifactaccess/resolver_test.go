package workloadartifactaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResolverSignsPlaneScopedRequestAndValidatesGrant(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	key := bytes.Repeat([]byte{0x71}, 32)
	claims := Claims{Version: 1, Audience: Audience, TicketID: strings.Repeat("a", 48), UserID: "usr_1", TenantID: "tenant_1",
		WorkloadRunID: "run_1", ArtifactID: "artifact_1", ResourcePlane: "rp_1", IssuedAt: now.Unix(), ExpiresAt: now.Add(TicketMaxTTL).Unix()}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/internal/resource-planes/rp_1/workload-artifacts/artifact_1/access" ||
			r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request route/method/content type = %s %s %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		timestamp, requestID, signature := r.Header.Get("X-Hako-Timestamp"), r.Header.Get("X-Hako-Request-ID"), r.Header.Get("X-Hako-Signature")
		if !requestIDPattern.MatchString(requestID) || signature != signRequest(key, http.MethodPost, r.URL.EscapedPath(), timestamp, requestID, body) {
			t.Errorf("invalid Resource Plane request signature: request_id=%q", requestID)
		}
		var request struct {
			Claims Claims `json:"claims"`
		}
		if err := json.Unmarshal(body, &request); err != nil || request.Claims != claims {
			t.Errorf("resolver body claims=%+v err=%v", request.Claims, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Grant{StorageRef: "s3://bucket/workloads/tenant/run/logs", SHA256: strings.Repeat("b", 64), SizeBytes: 10, Kind: "logs"})
	}))
	defer server.Close()
	resolver, err := NewResolver(ResolverConfig{Endpoint: server.URL, PlaneID: "rp_1", HMACKey: key, Client: server.Client(), Now: func() time.Time { return now },
		Random: bytes.NewReader(bytes.Repeat([]byte{0x02}, 16))})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := resolver.Resolve(context.Background(), claims)
	if err != nil || grant.StorageRef != "s3://bucket/workloads/tenant/run/logs" || grant.SizeBytes != 10 {
		t.Fatalf("grant=%+v err=%v", grant, err)
	}
}

func TestResolverRejectsInvalidEndpointClaimsAndGrant(t *testing.T) {
	key := bytes.Repeat([]byte{0x71}, 32)
	for _, endpoint := range []string{"http://example.test", "https://user:pass@example.test", "https://example.test/path"} {
		if _, err := NewResolver(ResolverConfig{Endpoint: endpoint, PlaneID: "rp_1", HMACKey: key}); err == nil {
			t.Errorf("accepted endpoint %q", endpoint)
		}
	}
	now := time.Now().UTC()
	claims := Claims{Version: 1, Audience: Audience, TicketID: strings.Repeat("a", 48), UserID: "usr_1", TenantID: "tenant_1",
		WorkloadRunID: "run_1", ArtifactID: "artifact_1", ResourcePlane: "rp_1", IssuedAt: now.Unix(), ExpiresAt: now.Add(TicketMaxTTL).Unix()}
	for name, mutate := range map[string]func(*Claims){
		"other plane":   func(c *Claims) { c.ResourcePlane = "rp_2" },
		"invalid nonce": func(c *Claims) { c.TicketID = "not-a-ticket" },
		"expired":       func(c *Claims) { c.ExpiresAt = now.Add(-time.Second).Unix() },
	} {
		t.Run(name, func(t *testing.T) {
			changed := claims
			mutate(&changed)
			resolver, err := NewResolver(ResolverConfig{Endpoint: "https://example.test", PlaneID: "rp_1", HMACKey: key, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := resolver.Resolve(context.Background(), changed); err == nil {
				t.Fatal("Resolve accepted invalid claims")
			}
		})
	}

	for _, responseBody := range []string{
		`{"storage_ref":"https://bucket/object","sha256":"` + strings.Repeat("a", 64) + `","size_bytes":1,"kind":"logs"}`,
		`{"storage_ref":"s3://bucket/workloads/../secret","sha256":"` + strings.Repeat("a", 64) + `","size_bytes":1,"kind":"logs"}`,
		fmt.Sprintf(`{"storage_ref":"s3://bucket/workloads/object","sha256":"%s","size_bytes":%d,"kind":"logs"}`, strings.Repeat("a", 64), MaxArtifactSize+1),
	} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(responseBody))
		}))
		resolver, err := NewResolver(ResolverConfig{Endpoint: server.URL, PlaneID: "rp_1", HMACKey: key, Client: server.Client()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := resolver.Resolve(context.Background(), claims); err == nil {
			t.Errorf("Resolve accepted unsafe Grant %s", responseBody)
		}
		server.Close()
	}
}
