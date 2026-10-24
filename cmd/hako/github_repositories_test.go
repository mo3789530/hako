package main

import (
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

func TestParseGitHubRepositoryAgentWritePolicyCLI(t *testing.T) {
	got, err := parseGitHubRepositoryAgentWritePolicyCLI([]string{"set", "--tenant", " tenant_a ", "--installation", "4", "--repository", "8", "--policy-file", "policy.json"})
	if err != nil || got.tenantID != "tenant_a" || got.installationID != 4 || got.repositoryID != 8 || got.policyFile != "policy.json" {
		t.Fatalf("parse Agent write policy CLI = %+v error=%v", got, err)
	}
	for _, args := range [][]string{
		{}, {"enable", "--tenant", "tenant_a", "--installation", "4", "--repository", "8", "--policy-file", "p.json"},
		{"set", "--tenant", "tenant_a", "--installation", "0", "--repository", "8", "--policy-file", "p.json"},
		{"set", "--tenant", "tenant_a", "--installation", "4", "--repository", "8"},
	} {
		if _, err := parseGitHubRepositoryAgentWritePolicyCLI(args); err == nil {
			t.Errorf("accepted invalid Agent write policy CLI args %q", args)
		}
	}
}

func TestParseGitHubRepositoryAgentModelAccessCLI(t *testing.T) {
	got, err := parseGitHubRepositoryAgentModelAccessCLI([]string{"set", "--tenant", " tenant_a ", "--installation", "4", "--repository", "8", "--enabled", "true"})
	if err != nil || got.tenantID != "tenant_a" || got.installationID != 4 || got.repositoryID != 8 || !got.enabled {
		t.Fatalf("parse Agent model access CLI = %+v error=%v", got, err)
	}
	disabled, err := parseGitHubRepositoryAgentModelAccessCLI([]string{"set", "--tenant", "tenant_a", "--installation", "4", "--repository", "8", "--enabled", "false"})
	if err != nil || disabled.enabled {
		t.Fatalf("parse disabled Agent model access CLI = %+v error=%v", disabled, err)
	}
	for _, args := range [][]string{
		{}, {"enable", "--tenant", "tenant_a", "--installation", "4", "--repository", "8", "--enabled", "true"},
		{"set", "--tenant", "tenant_a", "--installation", "0", "--repository", "8", "--enabled", "true"},
		{"set", "--tenant", "tenant_a", "--installation", "4", "--repository", "8", "--enabled", "yes"},
	} {
		if _, err := parseGitHubRepositoryAgentModelAccessCLI(args); err == nil {
			t.Errorf("accepted invalid Agent model access CLI args %q", args)
		}
	}
}

func TestParseGitHubRepositoryPreviewsCLI(t *testing.T) {
	got, err := parseGitHubRepositoryPreviewsCLI([]string{"list", "--tenant", " tenant_a ", "--installation", "4", "--repository", "8", "--limit", "25"})
	if err != nil || got.tenantID != "tenant_a" || got.installationID != 4 || got.repositoryID != 8 || got.limit != 25 {
		t.Fatalf("parse Preview list CLI = %+v error=%v", got, err)
	}
	defaults, err := parseGitHubRepositoryPreviewsCLI([]string{"list", "--tenant", "tenant_a", "--installation", "4", "--repository", "8"})
	if err != nil || defaults.limit != 50 {
		t.Fatalf("parse Preview list defaults = %+v error=%v", defaults, err)
	}
	for _, args := range [][]string{
		{}, {"enable", "--tenant", "tenant_a", "--installation", "4", "--repository", "8"},
		{"list", "--tenant", "tenant_a", "--installation", "0", "--repository", "8"},
		{"list", "--tenant", "tenant_a", "--installation", "4", "--repository", "8", "--limit", "101"},
	} {
		if _, err := parseGitHubRepositoryPreviewsCLI(args); err == nil {
			t.Errorf("accepted invalid Preview list CLI args %q", args)
		}
	}
}

func TestParseGitHubRepositoryPreviewSessionCLI(t *testing.T) {
	got, err := parseGitHubRepositoryPreviewSessionCLI([]string{"create", "--tenant", " tenant_a ", "--installation", "4", "--repository", "8", "--pull-request", "19", "--permissions", "vscode,shell"}, "create")
	if err != nil || got.tenantID != "tenant_a" || got.installationID != 4 || got.repositoryID != 8 || got.pullRequest != 19 || len(got.permissions) != 2 {
		t.Fatalf("parse Preview session create = %+v error=%v", got, err)
	}
	revoke, err := parseGitHubRepositoryPreviewSessionCLI([]string{"revoke", "--tenant", "tenant_a", "--installation", "4", "--repository", "8", "--pull-request", "19", "--session", strings.Repeat("a", 48)}, "revoke")
	if err != nil || revoke.sessionID != strings.Repeat("a", 48) {
		t.Fatalf("parse Preview session revoke = %+v error=%v", revoke, err)
	}
	for _, test := range []struct {
		action string
		args   []string
	}{
		{"create", []string{"create", "--tenant", "tenant_a", "--installation", "4", "--repository", "8", "--pull-request", "19", "--permissions", "vscode,,shell"}},
		{"create", []string{"create", "--tenant", "tenant_a", "--installation", "4", "--repository", "8", "--pull-request", "19", "--permissions", "admin"}},
		{"revoke", []string{"revoke", "--tenant", "tenant_a", "--installation", "4", "--repository", "8", "--pull-request", "19"}},
	} {
		if _, err := parseGitHubRepositoryPreviewSessionCLI(test.args, test.action); err == nil {
			t.Errorf("Preview session parser accepted %q", test.args)
		}
	}
}

func TestParseGitHubRepositoryFactoryAndPreviewActions(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "factory enable trims tenant", args: []string{"enable", "--tenant", " tenant_a ", "--installation", "4", "--repository", "8"}, want: true},
		{name: "factory disable", args: []string{"disable", "--tenant", "tenant_a", "--installation", "4", "--repository", "8"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseGitHubRepositoryFactoryCLI(test.args)
			if err != nil || got.tenantID != "tenant_a" || got.installationID != 4 || got.repositoryID != 8 || got.enabled != test.want {
				t.Fatalf("parse factory action = %+v, %v", got, err)
			}
		})
	}
	for _, args := range [][]string{
		{}, {"list", "--tenant", "tenant_a", "--installation", "4", "--repository", "8"},
		{"enable", "--tenant", " ", "--installation", "4", "--repository", "8"},
		{"enable", "--tenant", "tenant_a", "--installation", "0", "--repository", "8"},
		{"enable", "--tenant", "tenant_a", "--installation", "4", "--repository", "-1"},
		{"enable", "--tenant", "tenant_a", "--installation", "4", "--repository", "8", "extra"},
	} {
		if _, err := parseGitHubRepositoryFactoryCLI(args); err == nil {
			t.Errorf("factory parser accepted invalid args %q", args)
		}
	}
	if _, err := manageGitHubRepositoryPreviewCLI(nil); err == nil {
		t.Fatal("Preview management accepted a missing action")
	}
	if _, err := manageGitHubRepositoryFactoryCLI(nil); err == nil {
		t.Fatal("factory management accepted a missing action")
	}
}

func TestParseGitHubRepositoryActionsRunnerCLI(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "enable", args: []string{"enable", "--tenant", " tenant_a ", "--installation", "4", "--repository", "8"}, want: true},
		{name: "disable", args: []string{"disable", "--tenant", "tenant_a", "--installation", "4", "--repository", "8"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseGitHubRepositoryActionsRunnerCLI(test.args)
			if err != nil || got.tenantID != "tenant_a" || got.installationID != 4 || got.repositoryID != 8 || got.enabled != test.want {
				t.Fatalf("parse Actions Runner action = %+v, %v", got, err)
			}
		})
	}
	for _, args := range [][]string{
		{}, {"policy", "--tenant", "tenant_a", "--installation", "4", "--repository", "8"},
		{"enable", "--tenant", "tenant_a", "--installation", "0", "--repository", "8"},
		{"disable", "--tenant", "tenant_a", "--installation", "4", "--repository", "0"},
		{"enable", "--tenant", "tenant_a", "--installation", "4", "--repository", "8", "extra"},
	} {
		if _, err := parseGitHubRepositoryActionsRunnerCLI(args); err == nil {
			t.Errorf("Actions Runner parser accepted invalid args %q", args)
		}
	}
	if _, err := manageGitHubRepositoryActionsRunnerCLI(nil); err == nil {
		t.Fatal("Actions Runner management accepted a missing action")
	}
}

func TestParseGitHubRepositoryActionsRunnerWorkflowPolicyCLI(t *testing.T) {
	got, err := parseGitHubRepositoryActionsRunnerWorkflowPolicyCLI([]string{"policy", "--tenant", " tenant_a ", "--installation", "4", "--repository", "8", "--policy-file", " policy.json "})
	if err != nil || got.tenantID != "tenant_a" || got.installationID != 4 || got.repositoryID != 8 || got.policyFile != "policy.json" {
		t.Fatalf("parse workflow policy = %+v, %v", got, err)
	}
	for _, args := range [][]string{
		{}, {"set", "--tenant", "tenant_a", "--installation", "4", "--repository", "8", "--policy-file", "p.json"},
		{"policy", "--tenant", " ", "--installation", "4", "--repository", "8", "--policy-file", "p.json"},
		{"policy", "--tenant", "tenant_a", "--installation", "0", "--repository", "8", "--policy-file", "p.json"},
		{"policy", "--tenant", "tenant_a", "--installation", "4", "--repository", "8", "--policy-file", " "},
		{"policy", "--tenant", "tenant_a", "--installation", "4", "--repository", "8", "--policy-file", "p.json", "extra"},
	} {
		if _, err := parseGitHubRepositoryActionsRunnerWorkflowPolicyCLI(args); err == nil {
			t.Errorf("workflow policy parser accepted invalid args %q", args)
		}
	}
	if _, err := manageGitHubRepositoryActionsRunnerWorkflowPolicyCLI(nil); err == nil {
		t.Fatal("workflow policy management accepted a missing action")
	}
}

type repositoryCLICredentials string

func (c repositoryCLICredentials) Get(string, string) (string, error) { return string(c), nil }

func TestManageGitHubRepositoryCLIsCallTenantScopedAPI(t *testing.T) {
	policy := `{"workflow_path":".github/workflows/verify.yml","allowed_refs":["refs/heads/main"],"allowed_events":["push"],"required_labels":["self-hosted"],"allowed_labels":["self-hosted","hako"]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cli-repository-test" ||
			(r.Method == http.MethodPut && r.Header.Get("Content-Type") != "application/json") ||
			(r.Method != http.MethodPut && r.Method != http.MethodGet) {
			t.Errorf("request method/auth/content-type = %s/%q/%q", r.Method, r.Header.Get("Authorization"), r.Header.Get("Content-Type"))
		}
		if !strings.HasPrefix(r.URL.Path, "/v1/tenants/tenant_cli/github/installations/42/repositories/7/") {
			t.Errorf("request escaped Tenant/repository scope: %s", r.URL.Path)
		}
		if r.URL.Path == "/v1/tenants/tenant_cli/github/installations/42/repositories/7/previews" {
			if r.Method != http.MethodGet || r.URL.Query().Get("limit") != "25" {
				t.Errorf("Preview list method/limit = %s/%s", r.Method, r.URL.Query().Get("limit"))
			}
			_, _ = fmt.Fprint(w, `{"previews":[],"runtime_enabled":false}`)
			return
		}
		switch r.URL.Path {
		case "/v1/tenants/tenant_cli/github/installations/42/repositories/7/factory":
			var body struct {
				Enabled bool `json:"enabled"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || !body.Enabled {
				t.Error("Factory enable request body was invalid")
			}
			_, _ = fmt.Fprint(w, `{"repository":{"tenant_id":"tenant_cli","installation_id":42,"github_repository_id":7,"factory_enabled":true}}`)
		case "/v1/tenants/tenant_cli/github/installations/42/repositories/7/preview":
			var body struct {
				Enabled bool `json:"enabled"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Enabled {
				t.Error("Preview disable request body was invalid")
			}
			_, _ = fmt.Fprint(w, `{"repository":{"tenant_id":"tenant_cli","installation_id":42,"github_repository_id":7,"preview_enabled":false},"runtime_enabled":false}`)
		case "/v1/tenants/tenant_cli/github/installations/42/repositories/7/actions-runner":
			var body struct {
				Enabled bool `json:"enabled"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || !body.Enabled {
				t.Error("Actions Runner enable request body was invalid")
			}
			_, _ = fmt.Fprint(w, `{"repository":{"tenant_id":"tenant_cli","installation_id":42,"github_repository_id":7,"actions_runner_enabled":true}}`)
		case "/v1/tenants/tenant_cli/github/installations/42/repositories/7/actions-runner/workflow-policy":
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["workflow_path"] != ".github/workflows/verify.yml" {
				t.Error("workflow policy body was invalid")
			}
			_, _ = fmt.Fprintf(w, `{"workflow_policy":%s}`, policy)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	previous := cliCredentialGetter
	cliCredentialGetter = repositoryCLICredentials(fmt.Sprintf(`{"access_token":"cli-repository-test","token_type":"Bearer","expires_in":3600,"stored_at":%q}`,
		time.Now().UTC().Format(time.RFC3339)))
	t.Cleanup(func() { cliCredentialGetter = previous })

	if _, err := manageGitHubRepositoryFactoryCLI([]string{"enable", "--tenant", "tenant_cli", "--installation", "42", "--repository", "7"}); err != nil {
		t.Fatalf("Factory CLI request failed: %v", err)
	}
	preview, err := manageGitHubRepositoryPreviewCLI([]string{"disable", "--tenant", "tenant_cli", "--installation", "42", "--repository", "7"})
	if err != nil || preview.RuntimeEnabled || preview.Repository.PreviewEnabled {
		t.Fatalf("Preview CLI result = %+v, %v", preview, err)
	}
	previews, err := listGitHubRepositoryPreviewsCLI([]string{"list", "--tenant", "tenant_cli", "--installation", "42", "--repository", "7", "--limit", "25"})
	if err != nil || previews.RuntimeEnabled || len(previews.Previews) != 0 {
		t.Fatalf("Preview list CLI result = %+v, %v", previews, err)
	}
	if _, err := manageGitHubRepositoryActionsRunnerCLI([]string{"enable", "--tenant", "tenant_cli", "--installation", "42", "--repository", "7"}); err != nil {
		t.Fatalf("Actions Runner CLI request failed: %v", err)
	}
	policyFile := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policyFile, []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manageGitHubRepositoryActionsRunnerWorkflowPolicyCLI([]string{"policy", "--tenant", "tenant_cli", "--installation", "42", "--repository", "7", "--policy-file", policyFile}); err != nil {
		t.Fatalf("workflow policy CLI request failed: %v", err)
	}
}

func TestGitHubRepositoryPreviewSessionCLIsCallTenantScopedAPI(t *testing.T) {
	var created, revoked bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer preview-session-cli" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tenants/tenant_cli/github/installations/42/repositories/7/previews/19/sessions":
			created = true
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"session_id":"0123456789abcdef0123456789abcdef0123456789abcdef","workspace_id":"prvws_0123456789abcdef0123456789abcdef","ticket":"ticket-secret","expires_at":%q,"permissions":["vscode"]}`,
				time.Now().Add(time.Minute).UTC().Format(time.RFC3339))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/tenants/tenant_cli/github/installations/42/repositories/7/previews/19/sessions/0123456789abcdef0123456789abcdef0123456789abcdef":
			revoked = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Preview session request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	previous := cliCredentialGetter
	cliCredentialGetter = repositoryCLICredentials(fmt.Sprintf(`{"access_token":"preview-session-cli","token_type":"Bearer","expires_in":3600,"stored_at":%q}`,
		time.Now().UTC().Format(time.RFC3339)))
	t.Cleanup(func() { cliCredentialGetter = previous })
	args := []string{"--tenant", "tenant_cli", "--installation", "42", "--repository", "7", "--pull-request", "19"}
	createdSession, err := createGitHubRepositoryPreviewSessionCLI(append([]string{"create"}, append(args, "--permissions", "vscode")...))
	if err != nil || !created || createdSession.WorkspaceID != "prvws_0123456789abcdef0123456789abcdef" || createdSession.Ticket != "ticket-secret" {
		t.Fatalf("Preview session create = %+v, created=%v, error=%v", createdSession, created, err)
	}
	if err := revokeGitHubRepositoryPreviewSessionCLI(append(args, "--session", string(createdSession.SessionID))); err != nil || !revoked {
		t.Fatalf("Preview session revoke revoked=%v, error=%v", revoked, err)
	}
}

func TestManageNativeAgentPolicyCLIsKeepExecutionDisabled(t *testing.T) {
	const writePolicy = `{"enabled":true,"branch_prefixes":["hako/"],"allow_pull_requests":true,"required_checks":["hako/test"],"require_checks_to_pass":true}`
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPut || r.Header.Get("Authorization") != "Bearer cli-agent-policy-test" ||
			r.Header.Get("Content-Type") != "application/json" ||
			!strings.HasPrefix(r.URL.Path, "/v1/tenants/tenant_agent_cli/github/installations/42/repositories/7/") {
			t.Errorf("Agent policy request was outside expected Tenant/API scope: %s %s", r.Method, r.URL.String())
		}
		switch r.URL.Path {
		case "/v1/tenants/tenant_agent_cli/github/installations/42/repositories/7/agent-write-policy":
			var got map[string]any
			if json.NewDecoder(r.Body).Decode(&got) != nil || got["enabled"] != true {
				t.Error("Agent write policy request body was invalid")
			}
			_, _ = fmt.Fprintf(w, `{"write_policy":%s,"execution_enabled":false}`, writePolicy)
		case "/v1/tenants/tenant_agent_cli/github/installations/42/repositories/7/agent-model-access":
			var got struct {
				Enabled *bool `json:"enabled"`
			}
			if json.NewDecoder(r.Body).Decode(&got) != nil || got.Enabled == nil || !*got.Enabled {
				t.Error("Agent model access request body was invalid")
			}
			_, _ = fmt.Fprint(w, `{"agent_model_access":{"enabled":true,"enabled_at":"2026-10-24T12:00:00Z"},"execution_enabled":false}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	previous := cliCredentialGetter
	cliCredentialGetter = repositoryCLICredentials(fmt.Sprintf(`{"access_token":"cli-agent-policy-test","token_type":"Bearer","expires_in":3600,"stored_at":%q}`,
		time.Now().UTC().Format(time.RFC3339)))
	t.Cleanup(func() { cliCredentialGetter = previous })

	policyFile := filepath.Join(t.TempDir(), "write-policy.json")
	if err := os.WriteFile(policyFile, []byte(writePolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	writeResult, err := manageGitHubRepositoryAgentWritePolicyCLI([]string{"set", "--tenant", "tenant_agent_cli", "--installation", "42", "--repository", "7", "--policy-file", policyFile})
	if err != nil || writeResult.ExecutionEnabled || !writeResult.Policy.Enabled {
		t.Fatalf("Agent write policy result = %+v, %v", writeResult, err)
	}
	modelResult, err := manageGitHubRepositoryAgentModelAccessCLI([]string{"set", "--tenant", "tenant_agent_cli", "--installation", "42", "--repository", "7", "--enabled", "true"})
	if err != nil || modelResult.ExecutionEnabled || !modelResult.Access.Enabled || modelResult.Access.EnabledAt == nil {
		t.Fatalf("Agent model access result = %+v, %v", modelResult, err)
	}
	if calls != 2 {
		t.Fatalf("expected exactly two Tenant-scoped configuration requests, got %d", calls)
	}

	if err := os.WriteFile(policyFile, []byte(`{"enabled":false,"unexpected":"field"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manageGitHubRepositoryAgentWritePolicyCLI([]string{"set", "--tenant", "tenant_agent_cli", "--installation", "42", "--repository", "7", "--policy-file", policyFile}); err == nil {
		t.Fatal("unknown Agent write policy field was accepted")
	}
	if calls != 2 {
		t.Fatalf("invalid policy file reached API; request count=%d", calls)
	}
}
