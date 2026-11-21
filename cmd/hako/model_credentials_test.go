package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseModelCredentialCLI(t *testing.T) {
	version := "0123456789abcdef0123456789abcdef"
	for _, args := range [][]string{
		{"list", "--tenant", "tenant_a"},
		{"add", "--tenant", "tenant_a", "--binding", "binding_a", "--version-id", version},
		{"add", "--tenant", "tenant_a", "--purpose", "native_agent", "--binding", "binding_agent", "--version-id", version},
		{"rotate", "--tenant", "tenant_a", "--version-id", version, "--generation", "2"},
		{"rotate", "--tenant", "tenant_a", "--purpose", "native_agent", "--version-id", version, "--generation", "2"},
		{"disable", "--tenant", "tenant_a", "--generation", "2"},
		{"disable", "--tenant", "tenant_a", "--purpose", "native_agent", "--generation", "2"},
	} {
		if _, err := parseModelCredentialCLI(args); err != nil {
			t.Errorf("parseModelCredentialCLI(%v) error = %v", args, err)
		}
	}
	for _, args := range [][]string{
		{},
		{"list"},
		{"unknown", "--tenant", "tenant_a"},
		{"add", "--tenant", "tenant_a", "--binding", "binding_a", "--version-id", "plain-secret-value"},
		{"rotate", "--tenant", "tenant_a", "--version-id", version},
		{"disable", "--tenant", "tenant_a"},
		{"add", "--tenant", "tenant_a", "--binding", "binding_a", "--version-id", version, "--secret", "sk-ant-plaintext"},
		{"add", "--tenant", "tenant_a", "--purpose", "workspace", "--binding", "binding_a", "--version-id", version},
	} {
		if _, err := parseModelCredentialCLI(args); err == nil {
			t.Errorf("parseModelCredentialCLI(%v) accepted invalid/secret-bearing arguments", args)
		}
	}
}

func TestManageModelCredentialCLIFailsClosedOnInvalidConfigurationAndAPIRejection(t *testing.T) {
	validCredentials := fmt.Sprintf(`{"access_token":"model-credential-access","token_type":"Bearer","expires_in":3600,"stored_at":%q}`,
		time.Now().UTC().Format(time.RFC3339))
	previousGetter := cliCredentialGetter
	t.Cleanup(func() { cliCredentialGetter = previousGetter })

	for _, test := range []struct {
		name        string
		args        []string
		credentials string
		apiURL      string
		want        string
	}{
		{name: "invalid action", args: []string{"unknown", "--tenant", "tenant_model"}, credentials: validCredentials, apiURL: "https://api.example.test", want: "supported actions"},
		{name: "missing login", args: []string{"list", "--tenant", "tenant_model"}, apiURL: "https://api.example.test", want: "saved Hako login is invalid"},
		{name: "missing API URL", args: []string{"list", "--tenant", "tenant_model"}, credentials: validCredentials, want: "HAKO_API_URL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HAKO_API_URL", test.apiURL)
			cliCredentialGetter = workspaceShellCredentials(test.credentials)
			if _, err := manageModelCredentialCLI(test.args); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("manageModelCredentialCLI error = %v, want %q", err, test.want)
			}
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tenants/tenant_model/model-credentials" {
			t.Errorf("unexpected API request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"error":{"code":"tenant_forbidden","message":"Tenant Owner or Admin required"}}`)
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	cliCredentialGetter = workspaceShellCredentials(validCredentials)
	if _, err := manageModelCredentialCLI([]string{"list", "--tenant", "tenant_model"}); err == nil || !strings.Contains(err.Error(), "tenant_forbidden") {
		t.Fatalf("Tenant API denial = %v, want sanitized authorization error", err)
	}
}

func TestManageModelCredentialCLIUsesTenantScopedMetadataAPI(t *testing.T) {
	previousGetter := cliCredentialGetter
	cliCredentialGetter = workspaceShellCredentials(`{"access_token":"model-credential-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	t.Cleanup(func() { cliCredentialGetter = previousGetter })
	const version = "0123456789abcdef0123456789abcdef"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer model-credential-access" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		base := "/v1/tenants/tenant_model/model-credentials"
		if r.Method == http.MethodGet && r.URL.Path != base || r.Method == http.MethodPost &&
			r.URL.Path != base && r.URL.Path != base+"/anthropic/rotate" && r.URL.Path != base+"/anthropic/disable" {
			t.Errorf("credential route = %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			_, _ = fmt.Fprint(w, `{"items":[{"id":"mcred_model01","tenant_id":"tenant_model","provider":"anthropic","purpose":"native_agent","binding_id":"binding_model","secret_version_id":"`+version+`","status":"active","credential_generation":1,"created_at":"2026-10-24T00:00:00Z","updated_at":"2026-10-24T00:00:00Z"}]}`)
		case http.MethodPost:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read credential request: %v", err)
			}
			requestBody := string(body)
			if strings.Contains(requestBody, "sk-ant-") || strings.Contains(requestBody, "plaintext-secret") {
				t.Errorf("credential request contains a secret value: %q", requestBody)
			}
			if !strings.Contains(requestBody, `"purpose":"native_agent"`) {
				t.Errorf("credential request lost purpose/version metadata: %q", requestBody)
			}
			if !strings.Contains(r.URL.Path, "/disable") && !strings.Contains(requestBody, version) {
				t.Errorf("credential request lost Secret version metadata: %q", requestBody)
			}
			status, generation := "active", 2
			if strings.Contains(r.URL.Path, "/disable") {
				status, generation = "disabled", 3
			}
			_, _ = fmt.Fprintf(w, `{"credential":{"id":"mcred_model01","tenant_id":"tenant_model","provider":"anthropic","purpose":"native_agent","binding_id":"binding_model","secret_version_id":"%s","status":"%s","credential_generation":%d,"created_at":"2026-10-24T00:00:00Z","updated_at":"2026-10-24T00:00:00Z"}}`, version, status, generation)
		default:
			t.Errorf("unexpected method %s", r.Method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)

	for _, args := range [][]string{
		{"list", "--tenant", "tenant_model"},
		{"add", "--tenant", "tenant_model", "--purpose", "native_agent", "--binding", "binding_model", "--version-id", version},
		{"rotate", "--tenant", "tenant_model", "--purpose", "native_agent", "--generation", "1", "--version-id", version},
		{"disable", "--tenant", "tenant_model", "--purpose", "native_agent", "--generation", "2"},
	} {
		result, err := manageModelCredentialCLI(args)
		if err != nil {
			t.Fatalf("manageModelCredentialCLI(%q): %v", args, err)
		}
		if strings.Contains(fmt.Sprintf("%+v", result), "plaintext-secret") {
			t.Fatalf("model credential result exposed a plaintext value: %+v", result)
		}
	}
}
