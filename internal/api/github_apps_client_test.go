package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mo3789530/hako/internal/store/githubregistry"
)

func TestGitHubAppMetadataClientScopesRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tenants/tenant_one/github/apps" || r.Method != http.MethodPost || r.Header.Get("Idempotency-Key") != "request-one" || r.Header.Get("Authorization") != "Bearer api-test-token" {
			t.Error("incorrect App metadata request")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["credential_binding_id"] != "binding" || body["private_key_secret_arn"] != nil {
			t.Error("App request unexpectedly included Secret input")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"integration": map[string]any{"id": "gint_one", "tenant_id": "tenant_one", "status": "draft"}})
	}))
	defer server.Close()
	input := GitHubAppMetadataRequest{Action: "add", TenantID: "tenant_one", IdempotencyKey: "request-one", Draft: githubregistry.AppDraftRequest{GitHubAppID: 123, Slug: "app", ClientID: "Iv1.client", CredentialBindingID: "binding"}}
	result, err := ManageGitHubApp(context.Background(), server.URL, "api-test-token", input, server.Client())
	if err != nil || result.Integration == nil || result.Integration.Status != "draft" {
		t.Fatal("App metadata client failed")
	}
	for _, bad := range []GitHubAppMetadataRequest{{Action: "unknown", TenantID: "tenant_one"}, {Action: "show", TenantID: "tenant_one", IntegrationID: "../other"}, {Action: "disable", TenantID: "tenant_one", IntegrationID: "gint_one"}, {Action: "list", TenantID: "tenant_one", Limit: 101}} {
		if _, err := ManageGitHubApp(context.Background(), server.URL, "api-test-token", bad, server.Client()); err == nil {
			t.Fatal("invalid metadata client request accepted")
		}
	}
}
