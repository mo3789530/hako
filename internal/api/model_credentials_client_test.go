package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mo3789530/hako/internal/store/modelcredentials"
)

func TestManageModelCredentialUsesTenantScopedNoStoreRequests(t *testing.T) {
	version := "11111111111111111111111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("Authorization header = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/tenants/tenant_api/model-credentials":
			_, _ = fmt.Fprint(w, `{"items":[{"id":"mcred_client01","tenant_id":"tenant_api","provider":"anthropic","purpose":"native_agent","binding_id":"binding_api","secret_version_id":"`+version+`","status":"active","credential_generation":1,"created_at":"2026-10-17T00:00:00Z","updated_at":"2026-10-17T00:00:00Z"}]}`)
		case "POST /v1/tenants/tenant_api/model-credentials":
			var request ModelCredentialRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Provider != modelcredentials.ProviderAnthropic || request.Purpose != modelcredentials.PurposeNativeAgent || request.BindingID != "binding_api" || request.SecretVersionID != version {
				t.Errorf("create body = %+v, error=%v", request, err)
			}
			_, _ = fmt.Fprintf(w, `{"credential":{"id":"mcred_client01","tenant_id":"tenant_api","provider":"anthropic","purpose":"native_agent","binding_id":"binding_api","secret_version_id":"%s","status":"active","credential_generation":1,"created_at":"2026-10-17T00:00:00Z","updated_at":"2026-10-17T00:00:00Z"}}`, version)
		case "POST /v1/tenants/tenant_api/model-credentials/anthropic/rotate":
			_, _ = fmt.Fprintf(w, `{"credential":{"id":"mcred_client01","tenant_id":"tenant_api","provider":"anthropic","purpose":"claude_action","binding_id":"binding_api","secret_version_id":"%s","status":"active","credential_generation":2,"created_at":"2026-10-17T00:00:00Z","updated_at":"2026-10-17T00:00:00Z"}}`, version)
		case "POST /v1/tenants/tenant_api/model-credentials/anthropic/disable":
			_, _ = fmt.Fprintf(w, `{"credential":{"id":"mcred_client01","tenant_id":"tenant_api","provider":"anthropic","purpose":"claude_action","binding_id":"binding_api","secret_version_id":"%s","status":"disabled","credential_generation":3,"created_at":"2026-10-17T00:00:00Z","updated_at":"2026-10-17T00:00:00Z"}}`, version)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	commands := []ModelCredentialCommand{
		{Action: "list", TenantID: "tenant_api"},
		{Action: "add", TenantID: "tenant_api", Provider: modelcredentials.ProviderAnthropic, Purpose: modelcredentials.PurposeNativeAgent, BindingID: "binding_api", SecretVersionID: version},
		{Action: "rotate", TenantID: "tenant_api", Provider: modelcredentials.ProviderAnthropic, SecretVersionID: version, ExpectedGeneration: 1},
		{Action: "disable", TenantID: "tenant_api", Provider: modelcredentials.ProviderAnthropic, ExpectedGeneration: 2},
	}
	for _, command := range commands {
		if _, err := ManageModelCredential(context.Background(), server.URL, "token", command, server.Client()); err != nil {
			t.Errorf("ManageModelCredential(%s) error = %v", command.Action, err)
		}
	}
}

func TestManageModelCredentialRejectsCacheableAndCrossTenantResponse(t *testing.T) {
	for _, test := range []struct {
		name, header, body string
	}{
		{name: "cacheable", header: "public, max-age=10", body: `{"items":[]}`},
		{name: "cross tenant", header: "no-store", body: `{"items":[{"id":"mcred_other01","tenant_id":"tenant_other","provider":"anthropic"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", test.header)
				_, _ = fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			if _, err := ManageModelCredential(context.Background(), server.URL, "token", ModelCredentialCommand{Action: "list", TenantID: "tenant_api"}, server.Client()); err == nil {
				t.Fatal("unsafe model credential response was accepted")
			}
		})
	}
}
