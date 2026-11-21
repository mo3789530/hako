package workspacesession

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

func TestHTTPWorkspacePortAuthorizerSignsIdentityBoundRequest(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	now := time.Unix(1_800_000_000, 0).UTC()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Scheme != "https" ||
			request.URL.Path != "/v1/internal/resource-planes/rp_a/workspace-ports/authorize" || request.URL.RawQuery != "" {
			t.Fatalf("unexpected workspace port request URL=%s method=%s", request.URL, request.Method)
		}
		if got := request.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("content type=%q", got)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Claims Claims `json:"claims"`
			Port   uint16 `json:"port"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Port != 5432 || payload.Claims.TicketID != strings.Repeat("a", 48) || payload.Claims.UserID != "usr_a" ||
			payload.Claims.TenantID != "tenant_a" || payload.Claims.WorkspaceID != "ws_a" || payload.Claims.ResourcePlane != "rp_a" {
			t.Fatalf("request was not bound to expected claims and port: %+v", payload)
		}
		timestamp := request.Header.Get(activeLookupTimestampHeader)
		requestID := request.Header.Get(activeLookupRequestIDHeader)
		if !activeLookupRequestIDPattern.MatchString(requestID) || !VerifyActiveLookupSignature(key, request.Method,
			request.URL.EscapedPath(), timestamp, requestID, body, request.Header.Get(activeLookupSignatureHeader)) {
			t.Fatal("workspace port request signature invalid")
		}
		header := make(http.Header)
		header.Set("Content-Type", "application/json; charset=utf-8")
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(`{"allowed":true}`))}, nil
	})
	authorizer, err := NewHTTPWorkspacePortAuthorizer(HTTPWorkspacePortAuthorizerConfig{
		Endpoint: "https://control.example", ResourcePlaneID: "rp_a", HMACKey: key,
		Transport: transport, Now: func() time.Time { return now }, Random: strings.NewReader(strings.Repeat("n", 16)),
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := validPortClaims(now)
	allowed, err := authorizer.AllowPort(context.Background(), claims, 5432)
	if err != nil || !allowed {
		t.Fatalf("AllowPort() = %v, %v", allowed, err)
	}
}

func TestHTTPWorkspacePortAuthorizerFailsClosed(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	key := []byte(strings.Repeat("k", 32))
	validResponse := func(status int, contentType, body string) roundTripFunc {
		return func(*http.Request) (*http.Response, error) {
			header := make(http.Header)
			header.Set("Content-Type", contentType)
			return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
	}

	for _, config := range []HTTPWorkspacePortAuthorizerConfig{
		{Endpoint: "http://control.example", ResourcePlaneID: "rp_a", HMACKey: key},
		{Endpoint: "https://control.example/path", ResourcePlaneID: "rp_a", HMACKey: key},
		{Endpoint: "https://control.example", ResourcePlaneID: "bad/plane", HMACKey: key},
		{Endpoint: "https://control.example", ResourcePlaneID: "rp_a", HMACKey: []byte("short")},
		{Endpoint: "https://user:pass@control.example", ResourcePlaneID: "rp_a", HMACKey: key},
	} {
		if _, err := NewHTTPWorkspacePortAuthorizer(config); err == nil {
			t.Fatalf("accepted invalid authorizer config %+v", config)
		}
	}

	baseConfig := HTTPWorkspacePortAuthorizerConfig{
		Endpoint: "https://control.example", ResourcePlaneID: "rp_a", HMACKey: key,
		Now: func() time.Time { return now }, Random: strings.NewReader(strings.Repeat("n", 16)),
	}
	baseClaims := validPortClaims(now)
	invalidClaims := []struct {
		name string
		edit func(*Claims)
	}{
		{"wrong plane", func(c *Claims) { c.ResourcePlane = "rp_b" }},
		{"missing user", func(c *Claims) { c.UserID = "" }},
		{"missing tenant", func(c *Claims) { c.TenantID = "" }},
		{"missing workspace", func(c *Claims) { c.WorkspaceID = "" }},
		{"wrong version", func(c *Claims) { c.Version++ }},
		{"wrong audience", func(c *Claims) { c.Audience = "other" }},
		{"bad ticket id", func(c *Claims) { c.TicketID = "bad" }},
		{"missing issue time", func(c *Claims) { c.IssuedAt = 0 }},
		{"expiry before issue", func(c *Claims) { c.ExpiresAt = c.IssuedAt }},
		{"ttl too long", func(c *Claims) { c.ExpiresAt = c.IssuedAt + int64(TicketMaxTTL/time.Second) + 1 }},
		{"expired", func(c *Claims) { c.ExpiresAt = now.Unix() }},
		{"invalid permission", func(c *Claims) { c.Permissions = []Permission{"admin"} }},
		{"no port forward", func(c *Claims) { c.Permissions = []Permission{PermissionShell} }},
	}
	for _, test := range invalidClaims {
		t.Run(test.name, func(t *testing.T) {
			authorizer, err := NewHTTPWorkspacePortAuthorizer(baseConfig)
			if err != nil {
				t.Fatal(err)
			}
			claims := baseClaims
			test.edit(&claims)
			if allowed, err := authorizer.AllowPort(context.Background(), claims, 5432); err == nil || allowed {
				t.Fatalf("invalid claims returned allowed=%v err=%v", allowed, err)
			}
		})
	}

	authorizer, err := NewHTTPWorkspacePortAuthorizer(baseConfig)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, err := authorizer.AllowPort(context.Background(), baseClaims, 0); err == nil || allowed {
		t.Fatalf("zero port returned allowed=%v err=%v", allowed, err)
	}
	if allowed, err := (*HTTPWorkspacePortAuthorizer)(nil).AllowPort(context.Background(), baseClaims, 5432); err == nil || allowed {
		t.Fatalf("nil receiver returned allowed=%v err=%v", allowed, err)
	}
	if allowed, err := authorizer.AllowPort(nil, baseClaims, 5432); err == nil || allowed {
		t.Fatalf("nil context returned allowed=%v err=%v", allowed, err)
	}

	for _, test := range []struct {
		name      string
		transport http.RoundTripper
	}{
		{"transport error", roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("private transport detail") })},
		{"upstream denial", validResponse(http.StatusForbidden, "application/json", `{"allowed":true}`)},
		{"wrong content type", validResponse(http.StatusOK, "text/plain", `{"allowed":true}`)},
		{"invalid media type", validResponse(http.StatusOK, "%%%", `{"allowed":true}`)},
		{"empty response", validResponse(http.StatusOK, "application/json", "")},
		{"oversized response", validResponse(http.StatusOK, "application/json", strings.Repeat("x", 256))},
		{"malformed json", validResponse(http.StatusOK, "application/json", `{"allowed":"yes"}`)},
		{"unknown field", validResponse(http.StatusOK, "application/json", `{"allowed":true,"extra":1}`)},
		{"trailing json", validResponse(http.StatusOK, "application/json", `{"allowed":true}{}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := baseConfig
			config.Transport = test.transport
			config.Random = strings.NewReader(strings.Repeat("n", 16))
			configured, err := NewHTTPWorkspacePortAuthorizer(config)
			if err != nil {
				t.Fatal(err)
			}
			if allowed, err := configured.AllowPort(context.Background(), baseClaims, 5432); err == nil || allowed {
				t.Fatalf("invalid upstream response returned allowed=%v err=%v", allowed, err)
			}
		})
	}

	deniedConfig := baseConfig
	deniedConfig.Transport = validResponse(http.StatusOK, "application/json", `{"allowed":false}`)
	deniedConfig.Random = strings.NewReader(strings.Repeat("n", 16))
	denied, err := NewHTTPWorkspacePortAuthorizer(deniedConfig)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, err := denied.AllowPort(context.Background(), baseClaims, 5432); err != nil || allowed {
		t.Fatalf("valid denial returned allowed=%v err=%v", allowed, err)
	}

	randomConfig := baseConfig
	randomConfig.Random = strings.NewReader("")
	randomFailure, err := NewHTTPWorkspacePortAuthorizer(randomConfig)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, err := randomFailure.AllowPort(context.Background(), baseClaims, 5432); err == nil || allowed {
		t.Fatalf("nonce failure returned allowed=%v err=%v", allowed, err)
	}
}

func validPortClaims(now time.Time) Claims {
	return Claims{
		Version: 1, Audience: AudienceGateway, TicketID: strings.Repeat("a", 48), UserID: domain.UserID("usr_a"),
		TenantID: domain.TenantID("tenant_a"), WorkspaceID: domain.WorkspaceID("ws_a"), ResourcePlane: domain.ResourcePlaneID("rp_a"),
		Permissions: []Permission{PermissionPortForward}, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}
}
