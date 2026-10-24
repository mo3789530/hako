package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSoftwareCatalogCLI(t *testing.T) {
	got, err := parseSoftwareCatalogCLI([]string{"set", "--tenant", " tenant_a ", "--service", "payments-api", "--file", "service.json"})
	if err != nil || got.tenantID != "tenant_a" || got.service.ServiceID != "payments-api" || got.file != "service.json" {
		t.Fatalf("parse catalog set = %+v, error=%v", got, err)
	}
	if got, err := parseSoftwareCatalogCLI([]string{"list", "--tenant", "tenant_a"}); err != nil || got.action != "list" {
		t.Fatalf("parse catalog list = %+v, error=%v", got, err)
	}
	for _, args := range [][]string{
		{}, {"delete", "--tenant", "tenant_a", "--service", "api"}, {"list"},
		{"list", "--tenant", "tenant_a", "--service", "api"}, {"set", "--tenant", "tenant_a", "--service", "api"},
		{"set", "--tenant", "tenant_a", "--service", "api", "--file", "s.json", "unexpected"},
	} {
		if _, err := parseSoftwareCatalogCLI(args); err == nil {
			t.Errorf("accepted invalid catalog CLI args %q", args)
		}
	}
}

func TestSoftwareCatalogCLIUsesAuthenticatedTenantScopedAPI(t *testing.T) {
	previousGetter := cliCredentialGetter
	cliCredentialGetter = workspaceShellCredentials(`{"access_token":"catalog-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	t.Cleanup(func() { cliCredentialGetter = previousGetter })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer catalog-access" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			if r.URL.Path != "/v1/tenants/tenant_catalog/software-catalog" {
				t.Errorf("list route = %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
				return
			}
			_, _ = fmt.Fprint(w, `{"items":[{"tenant_id":"tenant_catalog","service_id":"payments-api","owner_team":"payments","installation_id":41,"github_repository_id":99,"repository":"acme/payments","runtime":"lambda-microvm","language":"go","dependencies":["redis","users"]}]}`)
		case http.MethodPut:
			if r.URL.Path != "/v1/tenants/tenant_catalog/software-catalog/payments-api" {
				t.Errorf("upsert route = %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
				return
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode upsert body: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if body["runtime"] != "lambda-microvm" || body["language"] != "go" || body["production_url"] != "https://payments.example" {
				t.Errorf("upsert body = %#v", body)
				http.Error(w, "bad metadata", http.StatusBadRequest)
				return
			}
			if _, exists := body["secret"]; exists {
				t.Error("catalog request unexpectedly contains a secret field")
			}
			_, _ = fmt.Fprint(w, `{"tenant_id":"tenant_catalog","service_id":"payments-api","owner_team":"payments","installation_id":41,"github_repository_id":99,"repository":"acme/payments","runtime":"lambda-microvm","language":"go","dependencies":["redis","users"],"production_url":"https://payments.example"}`)
		default:
			t.Errorf("unexpected method %s", r.Method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)

	var listed bytes.Buffer
	if err := runSoftwareCatalogCLI([]string{"list", "--tenant", "tenant_catalog"}, &listed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listed.String(), `"service_id":"payments-api"`) {
		t.Fatalf("catalog list output = %q", listed.String())
	}

	metadataPath := filepath.Join(t.TempDir(), "catalog.json")
	metadata := `{"owner_team":"payments","installation_id":41,"github_repository_id":99,"runtime":"lambda-microvm","language":"go","dependencies":["redis","users"],"production_url":"https://payments.example"}`
	if err := os.WriteFile(metadataPath, []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	var stored bytes.Buffer
	if err := runSoftwareCatalogCLI([]string{"set", "--tenant", "tenant_catalog", "--service", "payments-api", "--file", metadataPath}, &stored); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored.String(), `"repository":"acme/payments"`) {
		t.Fatalf("catalog set output = %q", stored.String())
	}
}

func TestSoftwareCatalogCLIRejectsUnknownOrInvalidMetadataBeforeAPI(t *testing.T) {
	tests := []string{
		`{"owner_team":"payments","installation_id":41,"github_repository_id":99,"runtime":"ec2","language":"go"}`,
		`{"owner_team":"payments","installation_id":41,"github_repository_id":99,"runtime":"lambda-microvm","language":"go","secret":"must-not-be-accepted"}`,
	}
	for i, content := range tests {
		t.Run(fmt.Sprintf("invalid-%d", i), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "catalog.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HAKO_API_URL", "http://127.0.0.1:1")
			previousGetter := cliCredentialGetter
			cliCredentialGetter = workspaceShellCredentials(`{"access_token":"catalog-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
			t.Cleanup(func() { cliCredentialGetter = previousGetter })
			if err := runSoftwareCatalogCLI([]string{"set", "--tenant", "tenant_catalog", "--service", "payments-api", "--file", path}, &bytes.Buffer{}); err == nil {
				t.Fatal("invalid metadata was accepted")
			}
		})
	}
}

func TestParseSoftwareCatalogDeployPolicyCLI(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"show", "--tenant", "tenant_a", "--service", "api"}, "show"},
		{[]string{"set", "--tenant", "tenant_a", "--service", "api", "--file", "policy.json"}, "set"},
		{[]string{"verify", "--tenant", "tenant_a", "--service", "api", "--token-file", "oidc.jwt"}, "verify"},
	} {
		got, err := parseSoftwareCatalogDeployPolicyCLI(tc.args)
		if err != nil || got.action != tc.want {
			t.Errorf("parse deploy policy args %q = %+v, %v", tc.args, got, err)
		}
	}
	for _, args := range [][]string{
		{}, {"disable", "--tenant", "tenant_a", "--service", "api"},
		{"show", "--tenant", "tenant_a", "--service", "api", "--token-file", "token"},
		{"set", "--tenant", "tenant_a", "--service", "api"},
		{"verify", "--tenant", "tenant_a", "--service", "api"},
		{"verify", "--tenant", "tenant_a", "--service", "api", "--file", "policy.json", "--token-file", "token"},
	} {
		if _, err := parseSoftwareCatalogDeployPolicyCLI(args); err == nil {
			t.Errorf("accepted invalid deploy policy args %q", args)
		}
	}
}
