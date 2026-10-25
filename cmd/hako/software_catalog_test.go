package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func TestSoftwareCatalogCLIFailsClosedOnOperationalErrors(t *testing.T) {
	validCredentials := fmt.Sprintf(`{"access_token":"catalog-access","token_type":"Bearer","expires_in":3600,"stored_at":%q}`,
		time.Now().UTC().Format(time.RFC3339))
	previousGetter := cliCredentialGetter
	t.Cleanup(func() { cliCredentialGetter = previousGetter })
	listArgs := []string{"list", "--tenant", "tenant_catalog"}
	for _, test := range []struct {
		name        string
		args        []string
		credentials string
		apiURL      string
		output      io.Writer
		want        string
	}{
		{name: "invalid action", args: []string{"unknown", "--tenant", "tenant_catalog"}, credentials: validCredentials, apiURL: "http://127.0.0.1:1", output: io.Discard, want: "list or set"},
		{name: "missing tenant", args: []string{"list"}, credentials: validCredentials, apiURL: "http://127.0.0.1:1", output: io.Discard, want: "--tenant is required"},
		{name: "nil output", args: listArgs, credentials: validCredentials, apiURL: "http://127.0.0.1:1", want: "output writer is required"},
		{name: "missing login", args: listArgs, apiURL: "http://127.0.0.1:1", output: io.Discard, want: "saved Hako login is invalid"},
		{name: "missing API URL", args: listArgs, credentials: validCredentials, output: io.Discard, want: "invalid Hako API URL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HAKO_API_URL", test.apiURL)
			cliCredentialGetter = workspaceShellCredentials(test.credentials)
			if err := runSoftwareCatalogCLI(test.args, test.output); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("runSoftwareCatalogCLI error = %v, want %q", err, test.want)
			}
		})
	}

	var calls int
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tenants/tenant_catalog/software-catalog" {
			t.Errorf("unexpected Catalog API request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"error":{"code":"tenant_forbidden","message":"Tenant membership required"}}`)
	}))
	t.Setenv("HAKO_API_URL", denied.URL)
	cliCredentialGetter = workspaceShellCredentials(validCredentials)
	if err := runSoftwareCatalogCLI(listArgs, io.Discard); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("Catalog Tenant denial = %v, want authorization error", err)
	}
	denied.Close()
	if calls != 1 {
		t.Fatalf("denied Catalog API calls = %d, want 1", calls)
	}

	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[]}`)
	}))
	defer allowed.Close()
	t.Setenv("HAKO_API_URL", allowed.URL)
	if err := runSoftwareCatalogCLI(listArgs, softwareCatalogFailingWriter{}); err == nil || !strings.Contains(err.Error(), "synthetic output failure") {
		t.Fatalf("Catalog output failure = %v, want writer error", err)
	}
}

type softwareCatalogFailingWriter struct{}

func (softwareCatalogFailingWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic output failure")
}

func TestSoftwareCatalogDeployPolicyCLIKeepsExecutionDormant(t *testing.T) {
	previousGetter := cliCredentialGetter
	cliCredentialGetter = workspaceShellCredentials(`{"access_token":"deploy-policy-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	t.Cleanup(func() { cliCredentialGetter = previousGetter })
	policy := `{"enabled":true,"audience":"sts.amazonaws.com","allowed_refs":["refs/heads/main"],"environments":["production"],"allowed_workflow_refs":["acme/payments/.github/workflows/deploy.yml@refs/heads/main"],"require_human_approval":true}`
	verification := `{"valid":true,"execution_enabled":false,"environment_approval_verified":true,"repository":"acme/payments","repository_id":"99","ref":"refs/heads/main","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","environment":"production","workflow_ref":"acme/payments/.github/workflows/deploy.yml@refs/heads/main","event_name":"push","run_id":"1234","run_attempt":"1"}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer deploy-policy-access" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		base := "/v1/tenants/tenant_catalog/software-catalog/payments-api/deploy-policy"
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == base:
			_, _ = fmt.Fprintf(w, `{"service_id":"payments-api","policy":%s,"execution_enabled":false}`, policy)
		case r.Method == http.MethodPut && r.URL.Path == base:
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["audience"] != "sts.amazonaws.com" {
				t.Errorf("policy payload = %#v, error=%v", request, err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprintf(w, `{"service_id":"payments-api","policy":%s,"execution_enabled":false}`, policy)
		case r.Method == http.MethodPost && r.URL.Path == base+"/verify":
			var request struct {
				Token string `json:"token"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Token != "synthetic-github-oidc-token" {
				t.Errorf("verification token = %q, error=%v", request.Token, err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprint(w, verification)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)

	policyPath := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policyPath, []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(t.TempDir(), "oidc-token")
	if err := os.WriteFile(tokenPath, []byte("synthetic-github-oidc-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"show", "--tenant", "tenant_catalog", "--service", "payments-api"},
		{"set", "--tenant", "tenant_catalog", "--service", "payments-api", "--file", policyPath},
		{"verify", "--tenant", "tenant_catalog", "--service", "payments-api", "--token-file", tokenPath},
	} {
		var output bytes.Buffer
		if err := runSoftwareCatalogDeployPolicyCLI(args, &output); err != nil {
			t.Fatalf("runSoftwareCatalogDeployPolicyCLI(%q): %v", args, err)
		}
		if strings.Contains(output.String(), "synthetic-github-oidc-token") || strings.Contains(output.String(), `"execution_enabled":true`) {
			t.Fatalf("deploy policy output leaked a token or enabled execution: %q", output.String())
		}
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
