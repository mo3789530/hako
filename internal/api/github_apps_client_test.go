package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestTenantGitHubAppMigrationClientRequiresExplicitScopeAndConfirmation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tenants/tenant_one/github/apps/gint_one/migration" || r.Header.Get("Authorization") != "Bearer api-test-token" {
			t.Error("migration request was not scoped to the requested Tenant integration")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["action"] != "plan" || body["expected_revision"] != float64(4) || body["expected_generation"] != float64(9) || len(body["installation_ids"].([]any)) != 2 {
			t.Error("migration plan did not contain explicit revision and Installation IDs")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GitHubAppMetadataResult{Migration: &githubregistry.TenantAppMigrationPlan{CanBackfill: true}})
	}))
	defer server.Close()
	input := GitHubAppMetadataRequest{Action: "migration-plan", TenantID: "tenant_one", IntegrationID: "gint_one", ExpectedRevision: 4, ExpectedGeneration: 9, InstallationIDs: []int64{10, 11}}
	result, err := ManageGitHubApp(context.Background(), server.URL, "api-test-token", input, server.Client())
	if err != nil || result.Migration == nil || !result.Migration.CanBackfill {
		t.Fatalf("migration plan client failed: %+v %v", result, err)
	}
	input.Action, input.Confirm = "migration-backfill", false
	if _, err := ManageGitHubApp(context.Background(), server.URL, "api-test-token", input, server.Client()); err == nil {
		t.Fatal("migration backfill without explicit confirmation was accepted")
	}
	input.Confirm = true
	input.InstallationIDs = []int64{10, 10}
	if _, err := ManageGitHubApp(context.Background(), server.URL, "api-test-token", input, server.Client()); err == nil {
		t.Fatal("migration backfill with duplicate IDs was accepted")
	}
}

func TestBeginTenantGitHubAppSetupClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tenants/tenant_one/github/apps/gint_one/setup" || r.Header.Get("Authorization") != "Bearer api-test-token" {
			t.Error("incorrect setup request")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 0 {
			t.Error("setup request body was not empty")
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(GitHubAppSetupResult{SetupURL: "https://github.com/apps/example/installations/new?state=abc", ExpiresAt: time.Now().Add(time.Minute)})
	}))
	defer server.Close()
	result, err := BeginTenantGitHubAppSetup(context.Background(), server.URL, "api-test-token", "tenant_one", "gint_one", server.Client())
	if err != nil || !strings.HasPrefix(result.SetupURL, "https://github.com/apps/") || result.ExpiresAt.IsZero() {
		t.Fatalf("setup request failed: %+v %v", result, err)
	}
	for _, args := range [][3]string{{"", "token", "gint_one"}, {"tenant_one", "", "gint_one"}, {"tenant_one", "token", "../other"}} {
		if _, err := BeginTenantGitHubAppSetup(context.Background(), server.URL, args[1], args[0], args[2], server.Client()); err == nil {
			t.Fatal("invalid setup request accepted")
		}
	}
}

func TestTenantGitHubFailedDeliveryListClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tenants/tenant_one/github/apps/gint_one/deliveries" || r.URL.Query().Get("limit") != "25" || r.URL.Query().Get("offset") != "50" || r.Header.Get("Authorization") != "Bearer api-test-token" {
			t.Errorf("incorrect failed-delivery request: %s %s", r.Method, r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GitHubAppMetadataResult{Deliveries: []GitHubWebhookDeliverySummary{{DeliveryID: "delivery-one", EventType: "push", Action: "", ReplayCount: 2}}, Total: 1, Limit: 25, Offset: 50})
	}))
	defer server.Close()
	input := GitHubAppMetadataRequest{Action: "deliveries", TenantID: "tenant_one", IntegrationID: "gint_one", Limit: 25, Offset: 50}
	result, err := ManageGitHubApp(context.Background(), server.URL, "api-test-token", input, server.Client())
	if err != nil || result.Total != 1 || len(result.Deliveries) != 1 || result.Deliveries[0].DeliveryID != "delivery-one" || result.Deliveries[0].ReplayCount != 2 {
		t.Fatalf("failed-delivery list failed: %+v %v", result, err)
	}
	input.Offset = 1_000_001
	if _, err := ManageGitHubApp(context.Background(), server.URL, "api-test-token", input, server.Client()); err == nil {
		t.Fatal("out-of-range failed-delivery page was accepted")
	}
}
