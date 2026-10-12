package workspacesession

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestHTTPActiveSessionLookupSignsIdentityBoundRequest(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	now := time.Unix(1_800_000_000, 0).UTC()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Scheme != "https" ||
			request.URL.Path != "/v1/internal/resource-planes/rp_a/workspace-sessions/active" || request.URL.RawQuery != "" {
			t.Fatalf("unexpected active lookup request URL=%s method=%s", request.URL, request.Method)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var claims Claims
		if err := json.Unmarshal(body, &claims); err != nil {
			t.Fatal(err)
		}
		if claims.TicketID != strings.Repeat("a", 48) || claims.UserID != "usr_a" || claims.TenantID != "tenant_a" || claims.WorkspaceID != "ws_a" || claims.ResourcePlane != "rp_a" {
			t.Fatalf("lookup claims were not identity-bound: %+v", claims)
		}
		timestamp := request.Header.Get(activeLookupTimestampHeader)
		requestID := request.Header.Get(activeLookupRequestIDHeader)
		if !activeLookupRequestIDPattern.MatchString(requestID) || !VerifyActiveLookupSignature(key, request.Method, request.URL.EscapedPath(),
			timestamp, requestID, body, request.Header.Get(activeLookupSignatureHeader)) {
			t.Fatal("active lookup request signature invalid")
		}
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(`{"active":true}`))}, nil
	})
	lookup, err := NewHTTPActiveSessionLookup(HTTPActiveSessionLookupConfig{Endpoint: "https://control.example", ResourcePlaneID: "rp_a", HMACKey: key,
		Transport: transport, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	claims := Claims{Version: 1, Audience: AudienceGateway, UserID: "usr_a", TenantID: "tenant_a", WorkspaceID: "ws_a", ResourcePlane: domain.ResourcePlaneID("rp_a"),
		TicketID: strings.Repeat("a", 48), Permissions: []Permission{PermissionShell}, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix()}
	active, err := lookup.IsActive(context.Background(), claims)
	if err != nil || !active {
		t.Fatalf("lookup result=%v err=%v", active, err)
	}
	claims.ResourcePlane = "rp_b"
	if active, err := lookup.IsActive(context.Background(), claims); err == nil || active {
		t.Fatalf("cross-Plane lookup result=%v err=%v", active, err)
	}
}

func TestHTTPActiveSessionLookupFailsClosedOnInvalidConfigAndResponse(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	for _, config := range []HTTPActiveSessionLookupConfig{
		{Endpoint: "http://control.example", ResourcePlaneID: "rp_a", HMACKey: key},
		{Endpoint: "https://control.example", ResourcePlaneID: "rp_a", HMACKey: []byte("short")},
		{Endpoint: "https://user:pass@control.example", ResourcePlaneID: "rp_a", HMACKey: key},
	} {
		if _, err := NewHTTPActiveSessionLookup(config); err == nil {
			t.Fatalf("accepted invalid lookup config %+v", config)
		}
	}
	lookup, err := NewHTTPActiveSessionLookup(HTTPActiveSessionLookupConfig{Endpoint: "https://control.example", ResourcePlaneID: "rp_a", HMACKey: key,
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			header := make(http.Header)
			header.Set("Content-Type", "application/json")
			return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(`{"active":"yes"}`))}, nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	claims := Claims{Version: 1, Audience: AudienceGateway, UserID: "usr_a", TenantID: "tenant_a", WorkspaceID: "ws_a", ResourcePlane: "rp_a",
		TicketID: strings.Repeat("a", 48), Permissions: []Permission{PermissionShell}, IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Minute).Unix()}
	if active, err := lookup.IsActive(context.Background(), claims); err == nil || active {
		t.Fatalf("malformed response returned active=%v error=%v", active, err)
	}
}
