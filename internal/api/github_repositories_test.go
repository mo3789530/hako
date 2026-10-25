package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/agenttask"

	"github.com/mo3789530/hako/internal/store/githubregistry"
)

func TestSetGitHubRepositoryFactoryEnabledUsesTenantScopedPut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/tenants/tenant_factory_api/github/installations/7001/repositories/7002/factory" ||
			r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("request = %s %s, authorization present=%t", r.Method, r.URL.Path, r.Header.Get("Authorization") != "")
		}
		var body map[string]bool
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 || !body["enabled"] {
			t.Fatalf("request body = %+v, error = %v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"repository":{"tenant_id":"tenant_factory_api","installation_id":7001,"github_repository_id":7002,"owner_login":"acme","repository_name":"api","default_branch":"main","factory_enabled":true}}`))
	}))
	defer server.Close()

	result, err := SetGitHubRepositoryFactoryEnabled(context.Background(), server.URL, "test-token", "tenant_factory_api", 7001, 7002, true, server.Client())
	if err != nil || !result.Repository.FactoryEnabled || result.Repository.GitHubRepositoryID != 7002 {
		t.Fatalf("SetGitHubRepositoryFactoryEnabled() = %+v, error = %v", result, err)
	}
}

func TestSetGitHubRepositoryPreviewEnabledKeepsRuntimeDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/tenants/tenant_preview_api/github/installations/7001/repositories/7002/preview" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("Authorization header = %q", r.Header.Get("Authorization"))
		}
		var body map[string]bool
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !body["enabled"] {
			t.Fatalf("request body = %v, error = %v", body, err)
		}
		_, _ = w.Write([]byte(`{"repository":{"tenant_id":"tenant_preview_api","installation_id":7001,"github_repository_id":7002,"preview_enabled":true},"runtime_enabled":false}`))
	}))
	defer server.Close()
	result, err := SetGitHubRepositoryPreviewEnabled(context.Background(), server.URL, "test-token", "tenant_preview_api", 7001, 7002, true, server.Client())
	if err != nil || !result.Repository.PreviewEnabled || result.RuntimeEnabled {
		t.Fatalf("SetGitHubRepositoryPreviewEnabled() = %+v, error = %v", result, err)
	}
}

func TestListGitHubRepositoryPreviewsUsesBoundedTenantScopedEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tenants/tenant_preview_api/github/installations/7001/repositories/7002/previews" ||
			r.URL.Query().Get("limit") != "25" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("request = %s %s?%s, authorization present=%t", r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization") != "")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"runtime_enabled":false,"previews":[{"tenant_id":"tenant_preview_api","installation_id":7001,"repository_id":7002,"pull_request_number":17,"head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","head_ref":"feature/api","generation":1,"desired_state":"present","state":"awaiting_runtime","updated_at":"2026-10-24T00:00:00Z"}]}`))
	}))
	defer server.Close()
	result, err := ListGitHubRepositoryPreviews(context.Background(), server.URL, "test-token", "tenant_preview_api", 7001, 7002, 25, server.Client())
	if err != nil || result.RuntimeEnabled || len(result.Previews) != 1 || result.Previews[0].PullRequestNumber != 17 {
		t.Fatalf("ListGitHubRepositoryPreviews() = %+v, error=%v", result, err)
	}
}

func TestListGitHubRepositoryPreviewsRejectsInvalidInputsAndRuntimeClaim(t *testing.T) {
	for _, test := range []struct {
		name, tenant string
		limit        int
		wantError    string
	}{
		{name: "tenant", tenant: "", limit: 10, wantError: "valid Tenant"},
		{name: "limit", tenant: "tenant_preview_api", limit: 101, wantError: "valid Tenant"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ListGitHubRepositoryPreviews(context.Background(), "https://api.example", "token", test.tenant, 7001, 7002, test.limit, nil)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want containing %q", err, test.wantError)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"runtime_enabled":true,"previews":[]}`))
	}))
	defer server.Close()
	if _, err := ListGitHubRepositoryPreviews(context.Background(), server.URL, "token", "tenant_preview_api", 7001, 7002, 10, server.Client()); err == nil {
		t.Fatal("accepted response claiming Preview runtime is enabled")
	}
	invalidGenerationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"runtime_enabled":false,"previews":[{"tenant_id":"tenant_preview_api","installation_id":7001,"repository_id":7002,"pull_request_number":17,"head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","head_ref":"feature/api","generation":0,"desired_state":"present","state":"awaiting_runtime","updated_at":"2026-10-24T00:00:00Z"}]}`))
	}))
	defer invalidGenerationServer.Close()
	if _, err := ListGitHubRepositoryPreviews(context.Background(), invalidGenerationServer.URL, "token", "tenant_preview_api", 7001, 7002, 10, invalidGenerationServer.Client()); err == nil {
		t.Fatal("accepted Preview response with a non-positive lifecycle generation")
	}
}

func TestSetGitHubRepositoryAgentWritePolicyUsesTenantScopedDormantEndpoint(t *testing.T) {
	policy := agenttask.WritePolicy{Enabled: true, BranchPrefixes: []string{"hako/agent/"}, AllowPullRequests: true,
		RequiredChecks: []string{"hako/lint", "hako/test"}, RequireChecksToPass: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/tenants/tenant_factory_api/github/installations/7001/repositories/7002/agent-write-policy" ||
			r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected Agent write policy request: %s %s", r.Method, r.URL.Path)
		}
		var got agenttask.WritePolicy
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil || got.Validate() != nil || got.Enabled != policy.Enabled {
			t.Errorf("invalid Agent write policy request: %+v error=%v", got, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"write_policy": policy, "execution_enabled": false})
	}))
	defer server.Close()
	result, err := SetGitHubRepositoryAgentWritePolicy(context.Background(), server.URL, "test-token", "tenant_factory_api", 7001, 7002, policy, server.Client())
	if err != nil || result.ExecutionEnabled || !reflect.DeepEqual(result.Policy, policy) {
		t.Fatalf("SetGitHubRepositoryAgentWritePolicy() = %+v, error=%v", result, err)
	}
}

func TestSetGitHubRepositoryAgentModelAccessUsesTenantScopedDormantEndpoint(t *testing.T) {
	enabledAt := time.Date(2026, 10, 24, 1, 2, 3, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/tenants/tenant_factory_api/github/installations/7001/repositories/7002/agent-model-access" ||
			r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected Agent model access request: %s %s", r.Method, r.URL.Path)
		}
		var got repositoryFactoryRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil || got.Enabled == nil || !*got.Enabled {
			t.Errorf("unexpected Agent model access request body: %+v, error=%v", got, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GitHubRepositoryAgentModelAccessResult{
			Access: githubregistry.AgentModelAccess{Enabled: true, EnabledAt: &enabledAt}, ExecutionEnabled: false,
		})
	}))
	defer server.Close()
	result, err := SetGitHubRepositoryAgentModelAccess(context.Background(), server.URL, "test-token", "tenant_factory_api", 7001, 7002, true, server.Client())
	if err != nil || result.ExecutionEnabled || !result.Access.Enabled || result.Access.EnabledAt == nil || !result.Access.EnabledAt.Equal(enabledAt) {
		t.Fatalf("SetGitHubRepositoryAgentModelAccess() = %+v, error=%v", result, err)
	}
	if _, err := SetGitHubRepositoryAgentModelAccess(nil, server.URL, "test-token", "tenant_factory_api", 7001, 7002, true, server.Client()); err == nil {
		t.Fatal("accepted nil request context")
	}
}

func TestSetGitHubRepositoryFactoryEnabledRejectsInvalidIdentityAndResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"repository":{"tenant_id":"tenant_other","installation_id":7001,"github_repository_id":7002,"factory_enabled":true}}`))
	}))
	defer server.Close()
	for _, test := range []struct {
		name, tenant string
		installation int64
		repository   int64
		wantErr      string
	}{
		{name: "tenant", tenant: "", installation: 7001, repository: 7002, wantErr: "valid Tenant"},
		{name: "installation", tenant: "tenant_factory_api", installation: 0, repository: 7002, wantErr: "valid Tenant"},
		{name: "repository", tenant: "tenant_factory_api", installation: 7001, repository: -1, wantErr: "valid Tenant"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := SetGitHubRepositoryFactoryEnabled(context.Background(), server.URL, "token", test.tenant, test.installation, test.repository, true, server.Client())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
	_, err := SetGitHubRepositoryFactoryEnabled(context.Background(), server.URL, "token", "tenant_factory_api", 7001, 7002, true, server.Client())
	if err == nil || !strings.Contains(err.Error(), "invalid GitHub Repository factory response") {
		t.Fatalf("mismatched response error = %v", err)
	}
}

func TestSetGitHubRepositoryActionsRunnerEnabledUsesTenantScopedPut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/tenants/tenant_factory_api/github/installations/7001/repositories/7002/actions-runner" ||
			r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("request = %s %s, authorization present=%t", r.Method, r.URL.Path, r.Header.Get("Authorization") != "")
		}
		var body map[string]bool
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 || !body["enabled"] {
			t.Fatalf("request body = %+v, error = %v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"repository":{"tenant_id":"tenant_factory_api","installation_id":7001,"github_repository_id":7002,"owner_login":"acme","repository_name":"api","default_branch":"main","actions_runner_enabled":true}}`))
	}))
	defer server.Close()

	result, err := SetGitHubRepositoryActionsRunnerEnabled(context.Background(), server.URL, "test-token", "tenant_factory_api", 7001, 7002, true, server.Client())
	if err != nil || !result.Repository.ActionsRunnerEnabled || result.Repository.GitHubRepositoryID != 7002 {
		t.Fatalf("SetGitHubRepositoryActionsRunnerEnabled() = %+v, error = %v", result, err)
	}
}

func TestSetGitHubRepositoryActionsRunnerEnabledRejectsMismatchedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"repository":{"tenant_id":"tenant_other","installation_id":7001,"github_repository_id":7002,"actions_runner_enabled":true}}`))
	}))
	defer server.Close()
	_, err := SetGitHubRepositoryActionsRunnerEnabled(context.Background(), server.URL, "token", "tenant_factory_api", 7001, 7002, true, server.Client())
	if err == nil || !strings.Contains(err.Error(), "invalid GitHub Repository Actions runner response") {
		t.Fatalf("mismatched response error = %v", err)
	}
}

func TestSetGitHubRepositoryActionsRunnerWorkflowPolicyUsesTenantScopedAPI(t *testing.T) {
	policy := githubregistry.ActionsRunnerWorkflowPolicy{WorkflowPath: ".github/workflows/claude.yml", AllowedRefs: []string{"refs/heads/main"},
		AllowedEvents: []string{"push"}, RequiredLabels: []string{"self-hosted", "hako"}, AllowedLabels: []string{"self-hosted", "hako"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/tenants/tenant_factory_api/github/installations/7001/repositories/7002/actions-runner/workflow-policy" ||
			r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected workflow-policy request: %s %s", r.Method, r.URL.Path)
		}
		var received githubregistry.ActionsRunnerWorkflowPolicy
		if json.NewDecoder(r.Body).Decode(&received) != nil || received.WorkflowPath != policy.WorkflowPath {
			t.Error("workflow policy was not encoded as expected")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"workflow_policy": policy})
	}))
	defer server.Close()
	result, err := SetGitHubRepositoryActionsRunnerWorkflowPolicy(context.Background(), server.URL, "test-token", "tenant_factory_api", 7001, 7002, policy, server.Client())
	if err != nil || result.Policy.WorkflowPath != policy.WorkflowPath {
		t.Fatalf("SetGitHubRepositoryActionsRunnerWorkflowPolicy() = %+v, error = %v", result, err)
	}
}

func TestGetGitHubRepositoryActionsRunnerWorkflowPolicyUsesTenantScopedAPI(t *testing.T) {
	policy := githubregistry.ActionsRunnerWorkflowPolicy{WorkflowPath: ".github/workflows/claude.yml", AllowedRefs: []string{"refs/heads/main"},
		AllowedEvents: []string{"push"}, RequiredLabels: []string{"self-hosted", "hako"}, AllowedLabels: []string{"self-hosted", "hako"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tenants/tenant_factory_api/github/installations/7001/repositories/7002/actions-runner/workflow-policy" ||
			r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected workflow-policy request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"workflow_policy": policy})
	}))
	defer server.Close()
	result, err := GetGitHubRepositoryActionsRunnerWorkflowPolicy(context.Background(), server.URL, "test-token", "tenant_factory_api", 7001, 7002, server.Client())
	if err != nil || !sameRunnerWorkflowPolicy(result.Policy, policy) {
		t.Fatalf("GetGitHubRepositoryActionsRunnerWorkflowPolicy() = %+v, error = %v", result, err)
	}
}
