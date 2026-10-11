package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
