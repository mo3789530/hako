package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/api"
)

func TestParseGitHubAppCLIAllowsReferencesNotSecretInputs(t *testing.T) {
	input, err := parseGitHubAppCLI([]string{"add", "--tenant", "tenant_one", "--app-id", "123", "--slug", "app", "--client-id", "Iv1.client", "--credential-binding", "binding"})
	if err != nil || input.IdempotencyKey == "" || input.Draft.CredentialBindingID != "binding" {
		t.Fatal("App CLI parsing failed")
	}
	for _, args := range [][]string{{}, {"list"}, {"show", "--tenant", "tenant_one"}, {"disable", "--tenant", "tenant_one", "--integration", "gint_one"}, {"add", "--tenant", "tenant_one", "--private-key", "raw-key"}, {"unknown", "--tenant", "tenant_one"}} {
		if _, err := parseGitHubAppCLI(args); err == nil {
			t.Fatal("invalid action or Secret flag accepted")
		}
	}
	input, err = parseGitHubAppCLI([]string{"disable", "--tenant", "tenant_one", "--integration", "gint_one", "--revision", "1"})
	if err != nil || input.ExpectedRevision != 1 {
		t.Fatal("revision-bound disable parsing failed")
	}
	input, err = parseGitHubAppCLI([]string{"setup", "--tenant", "tenant_one", "--integration", "gint_one"})
	if err != nil || input.Action != "setup" || input.IntegrationID != "gint_one" {
		t.Fatal("Tenant GitHub App setup parsing failed")
	}
	input, err = parseGitHubAppCLI([]string{"show", "--tenant", "tenant_one", "--integration", "gint_one"})
	if err != nil || input.Action != "show" || input.ExpectedRevision != 0 {
		t.Fatal("Tenant GitHub App show parsing failed")
	}
	input, err = parseGitHubAppCLI([]string{"verify", "--tenant", "tenant_one", "--integration", "gint_one", "--revision", "2"})
	if err != nil || input.Action != "verify" || input.ExpectedRevision != 2 {
		t.Fatal("revision-bound Tenant GitHub App verification parsing failed")
	}
	input, err = parseGitHubAppCLI([]string{"deliveries", "--tenant", "tenant_one", "--integration", "gint_one", "--limit", "25", "--offset", "50"})
	if err != nil || input.Action != "deliveries" || input.Limit != 25 || input.Offset != 50 {
		t.Fatal("failed webhook delivery CLI parsing failed")
	}
	if _, err := parseGitHubAppCLI([]string{"deliveries", "--tenant", "tenant_one"}); err == nil {
		t.Fatal("failed webhook delivery CLI without integration was accepted")
	}
	input, err = parseGitHubAppCLI([]string{"migration-plan", "--tenant", "tenant_one", "--integration", "gint_one", "--revision", "4", "--generation", "8", "--installations", "12,13"})
	if err != nil || len(input.InstallationIDs) != 2 || input.InstallationIDs[1] != 13 {
		t.Fatal("explicit Tenant migration dry-run parsing failed")
	}
	if _, err := parseGitHubAppCLI([]string{"migration-backfill", "--tenant", "tenant_one", "--integration", "gint_one", "--revision", "4", "--generation", "8", "--installations", "12"}); err == nil {
		t.Fatal("migration backfill without --apply was accepted")
	}
	input, err = parseGitHubAppCLI([]string{"migration-backfill", "--tenant", "tenant_one", "--integration", "gint_one", "--revision", "4", "--generation", "8", "--installations", "12", "--apply"})
	if err != nil || !input.Confirm || input.Action != "migration-backfill" {
		t.Fatal("explicitly confirmed migration backfill parsing failed")
	}
	for _, installations := range []string{"12,12", "12,nope", ",", "0"} {
		if _, err := parseGitHubAppCLI([]string{"migration-plan", "--tenant", "tenant_one", "--integration", "gint_one", "--revision", "4", "--generation", "8", "--installations", installations}); err == nil {
			t.Fatalf("invalid Installation list %q was accepted", installations)
		}
	}
	tooManyInstallations := make([]string, api.MaxGitHubAppMigrationInstallations+1)
	for index := range tooManyInstallations {
		tooManyInstallations[index] = fmt.Sprint(index + 1)
	}
	if _, err := parseGitHubAppCLI([]string{"migration-plan", "--tenant", "tenant_one", "--integration", "gint_one", "--revision", "4", "--generation", "8", "--installations", strings.Join(tooManyInstallations, ",")}); err == nil {
		t.Fatal("Installation list over the API limit was accepted")
	}
	input, err = parseGitHubAppCLI([]string{"select-default", "--tenant", "tenant_one", "--integration", "gint_one", "--revision", "4", "--generation", "8"})
	if err != nil || input.ExpectedGeneration != 8 {
		t.Fatal("revision and generation-bound default selection parsing failed")
	}
}

func TestManageGitHubAppCLIUsesTenantScopedMetadataAPI(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer cli-github-app-test" {
			t.Errorf("missing CLI Bearer token: %q", r.Header.Get("Authorization"))
		}
		const prefix = "/v1/tenants/tenant_app_cli/github/apps"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == prefix:
			if r.URL.Query().Get("limit") != "10" || r.URL.Query().Get("offset") != "2" {
				t.Errorf("App list pagination = %s", r.URL.RawQuery)
			}
			_, _ = fmt.Fprint(w, `{"items":[],"total":0,"limit":10,"offset":2}`)
		case r.Method == http.MethodGet && r.URL.Path == prefix+"/gint_app_cli":
			_, _ = fmt.Fprint(w, `{"integration":{"id":"gint_app_cli","tenant_id":"tenant_app_cli","status":"active"}}`)
		case r.Method == http.MethodPost && r.URL.Path == prefix:
			if r.Header.Get("Idempotency-Key") != "app-add-key-1234" {
				t.Errorf("App add idempotency key = %q", r.Header.Get("Idempotency-Key"))
			}
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["github_app_id"] != float64(123) || body["credential_binding_id"] != "binding_app_cli" {
				t.Errorf("App add body = %+v", body)
			}
			for key := range body {
				if strings.Contains(strings.ToLower(key), "private") || strings.Contains(strings.ToLower(key), "secret") {
					t.Errorf("App add sent an unexpected credential field %q", key)
				}
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{}`)
		case r.Method == http.MethodPost && r.URL.Path == prefix+"/gint_app_cli/disable":
			var body map[string]int64
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["expected_revision"] != 3 {
				t.Errorf("App disable revision body = %+v", body)
			}
			_, _ = fmt.Fprint(w, `{}`)
		case r.Method == http.MethodPost && r.URL.Path == prefix+"/gint_app_cli/verify":
			var body map[string]int64
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["expected_revision"] != 5 {
				t.Errorf("App verify revision body = %+v", body)
			}
			_, _ = fmt.Fprint(w, `{}`)
		case r.Method == http.MethodPost && r.URL.Path == prefix+"/gint_app_cli/select-default":
			var body map[string]int64
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["expected_revision"] != 4 || body["expected_generation"] != 2 {
				t.Errorf("App default selection fence = %+v", body)
			}
			_, _ = fmt.Fprint(w, `{}`)
		case r.Method == http.MethodPost && r.URL.Path == prefix+"/gint_app_cli/setup":
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"setup_url":"https://github.com/apps/acme-app/installations/new?state=opaque-state","expires_at":"2026-10-24T13:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == prefix+"/gint_app_cli/deliveries":
			if r.URL.Query().Get("limit") != "5" || r.URL.Query().Get("offset") != "7" {
				t.Errorf("Delivery pagination = %s", r.URL.RawQuery)
			}
			_, _ = fmt.Fprint(w, `{"deliveries":[],"total":0,"limit":5,"offset":7}`)
		case r.Method == http.MethodPost && r.URL.Path == prefix+"/gint_app_cli/migration":
			var body struct {
				Action             string  `json:"action"`
				ExpectedRevision   int64   `json:"expected_revision"`
				ExpectedGeneration int64   `json:"expected_generation"`
				InstallationIDs    []int64 `json:"installation_ids"`
				Confirm            bool    `json:"confirm"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.ExpectedRevision != 4 || body.ExpectedGeneration != 9 || len(body.InstallationIDs) != 2 || body.InstallationIDs[0] != 12 || body.InstallationIDs[1] != 13 {
				t.Errorf("migration request body = %+v", body)
			}
			if body.Action == "plan" && body.Confirm {
				t.Error("migration dry-run unexpectedly requested confirmation")
			}
			if body.Action == "backfill" && !body.Confirm {
				t.Error("migration backfill omitted confirmation")
			}
			if body.Action != "plan" && body.Action != "backfill" {
				t.Errorf("unexpected migration action %q", body.Action)
			}
			_, _ = fmt.Fprint(w, `{"migration":{"tenant_id":"tenant_app_cli","integration_id":"gint_app_cli","can_backfill":true}}`)
		default:
			t.Errorf("unexpected Tenant GitHub App API request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	previous := cliCredentialGetter
	cliCredentialGetter = repositoryCLICredentials(fmt.Sprintf(`{"access_token":"cli-github-app-test","token_type":"Bearer","expires_in":3600,"stored_at":%q}`,
		time.Now().UTC().Format(time.RFC3339)))
	t.Cleanup(func() { cliCredentialGetter = previous })
	previousBrowser := cliBrowserOpener
	openedURL := ""
	cliBrowserOpener = func(value string) error { openedURL = value; return nil }
	t.Cleanup(func() { cliBrowserOpener = previousBrowser })

	if _, key, err := manageGitHubAppCLI([]string{"list", "--tenant", "tenant_app_cli", "--limit", "10", "--offset", "2"}); err != nil || key != "" {
		t.Fatalf("Tenant App list = key %q, error %v", key, err)
	}
	if _, _, err := manageGitHubAppCLI([]string{"show", "--tenant", "tenant_app_cli", "--integration", "gint_app_cli"}); err != nil {
		t.Fatalf("Tenant App show = %v", err)
	}
	if _, key, err := manageGitHubAppCLI([]string{"add", "--tenant", "tenant_app_cli", "--app-id", "123", "--slug", "acme-app", "--client-id", "Iv1.client", "--credential-binding", "binding_app_cli", "--idempotency-key", "app-add-key-1234"}); err != nil || key != "app-add-key-1234" {
		t.Fatalf("Tenant App add = key %q, error %v", key, err)
	}
	if _, _, err := manageGitHubAppCLI([]string{"disable", "--tenant", "tenant_app_cli", "--integration", "gint_app_cli", "--revision", "3"}); err != nil {
		t.Fatalf("Tenant App disable = %v", err)
	}
	if _, _, err := manageGitHubAppCLI([]string{"verify", "--tenant", "tenant_app_cli", "--integration", "gint_app_cli", "--revision", "5"}); err != nil {
		t.Fatalf("Tenant App verify = %v", err)
	}
	if _, _, err := manageGitHubAppCLI([]string{"select-default", "--tenant", "tenant_app_cli", "--integration", "gint_app_cli", "--revision", "4", "--generation", "2"}); err != nil {
		t.Fatalf("Tenant App select-default = %v", err)
	}
	setup, _, err := manageGitHubAppCLI([]string{"setup", "--tenant", "tenant_app_cli", "--integration", "gint_app_cli"})
	if err != nil || setup.SetupURL != openedURL || !strings.HasPrefix(openedURL, "https://github.com/apps/acme-app/installations/new?state=") {
		t.Fatalf("Tenant App setup = %+v, opened URL %q, error %v", setup, openedURL, err)
	}
	if _, _, err := manageGitHubAppCLI([]string{"deliveries", "--tenant", "tenant_app_cli", "--integration", "gint_app_cli", "--limit", "5", "--offset", "7"}); err != nil {
		t.Fatalf("Tenant App deliveries = %v", err)
	}
	for _, action := range []string{"migration-plan", "migration-backfill"} {
		args := []string{action, "--tenant", "tenant_app_cli", "--integration", "gint_app_cli", "--revision", "4", "--generation", "9", "--installations", "12,13"}
		if action == "migration-backfill" {
			args = append(args, "--apply")
		}
		if _, _, err := manageGitHubAppCLI(args); err != nil {
			t.Fatalf("Tenant App %s = %v", action, err)
		}
	}
	if calls != 10 {
		t.Fatalf("Tenant App CLI made %d requests, want 10", calls)
	}
}

func TestManageGitHubAppCLIRejectsConfigurationAndProviderFailures(t *testing.T) {
	validCredentials := fmt.Sprintf(`{"access_token":"cli-github-app-test","token_type":"Bearer","expires_in":3600,"stored_at":%q}`,
		time.Now().UTC().Format(time.RFC3339))
	previousGetter := cliCredentialGetter
	previousBrowser := cliBrowserOpener
	t.Cleanup(func() {
		cliCredentialGetter = previousGetter
		cliBrowserOpener = previousBrowser
	})

	for _, test := range []struct {
		name        string
		args        []string
		credentials string
		apiURL      string
		want        string
	}{
		{name: "invalid action", args: []string{"invalid", "--tenant", "tenant_cli"}, credentials: validCredentials, apiURL: "https://api.example.test", want: "supported actions"},
		{name: "missing login", args: []string{"list", "--tenant", "tenant_cli"}, apiURL: "https://api.example.test", want: "saved Hako login is invalid"},
		{name: "missing setup login", args: []string{"setup", "--tenant", "tenant_cli", "--integration", "gint_cli"}, apiURL: "https://api.example.test", want: "saved Hako login is invalid"},
		{name: "missing setup API URL", args: []string{"setup", "--tenant", "tenant_cli", "--integration", "gint_cli"}, credentials: validCredentials, want: "HAKO_API_URL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HAKO_API_URL", test.apiURL)
			cliCredentialGetter = repositoryCLICredentials(test.credentials)
			if _, _, err := manageGitHubAppCLI(test.args); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("manageGitHubAppCLI error = %v, want %q", err, test.want)
			}
		})
	}

	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tenants/tenant_cli/github/apps" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		http.Error(w, "private denial details", http.StatusForbidden)
	}))
	t.Setenv("HAKO_API_URL", denied.URL)
	cliCredentialGetter = repositoryCLICredentials(validCredentials)
	if _, _, err := manageGitHubAppCLI([]string{"list", "--tenant", "tenant_cli"}); err == nil || strings.Contains(err.Error(), "private denial details") {
		t.Fatalf("GitHub App API denial was not safely propagated: %v", err)
	}
	denied.Close()

	setup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tenants/tenant_cli/github/apps/gint_cli/setup" {
			t.Errorf("unexpected setup request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"setup_url":"https://github.com/apps/acme-app/installations/new?state=opaque-state","expires_at":"2026-10-24T13:00:00Z"}`)
	}))
	defer setup.Close()
	t.Setenv("HAKO_API_URL", setup.URL)
	cliBrowserOpener = func(string) error { return errors.New("browser unavailable") }
	if _, _, err := manageGitHubAppCLI([]string{"setup", "--tenant", "tenant_cli", "--integration", "gint_cli"}); err == nil || !strings.Contains(err.Error(), "browser unavailable") {
		t.Fatalf("browser failure = %v, want opener error", err)
	}
}
