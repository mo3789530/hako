//go:build integration

package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestRepositoryFactorySettingRequiresTenantOwnerAndActiveRegisteredRepository(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_factory_api','Factory API',now()),('tenant_factory_other','Other',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_factory_owner','factory-owner','',now()),('usr_factory_admin','factory-admin','',now()),('usr_factory_member','factory-member','',now())`,
		`INSERT INTO tenant_members (tenant_id,user_id,role,joined_at) VALUES ('tenant_factory_api','usr_factory_owner','owner',now()),('tenant_factory_api','usr_factory_admin','admin',now()),('tenant_factory_api','usr_factory_member','member',now())`,
		`INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_factory_api',7001,'acme','active','usr_factory_owner',now(),now())`,
		`INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at) VALUES (7001,'tenant_factory_api',now())`,
		`INSERT INTO tenant_github_repositories (tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at) VALUES ('tenant_factory_api',7001,7002,'acme','api','main',now())`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "tenant-api-key",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
		}}})
	}))
	defer keys.Close()
	issuer := keys.URL + "/pool"
	verifier, err := auth.NewCognitoVerifier(auth.CognitoVerifierConfig{Issuer: issuer, ClientID: "hako-cli"})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(verifier, pool, WorkspaceCreateConfig{GitHubAppManagementEnabled: true, WebhookWorkloadTriggersEnabled: true, WorkloadRunsEnabled: true})
	path := "/v1/tenants/tenant_factory_api/github/installations/7001/repositories/7002/factory"
	call := func(route, subject, tenant, body string) *httptest.ResponseRecorder {
		token := signTenantAPIToken(t, key, issuer, subject, "openid hako/api")
		request := httptest.NewRequest(http.MethodPut, strings.Replace(route, "tenant_factory_api", tenant, 1), strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	for _, test := range []struct {
		subject, tenant, body string
		want                  int
	}{
		{"factory-owner", "tenant_factory_api", `{"enabled":true}`, http.StatusOK},
		{"factory-admin", "tenant_factory_api", `{"enabled":false}`, http.StatusOK},
		{"factory-member", "tenant_factory_api", `{"enabled":true}`, http.StatusNotFound},
		{"factory-owner", "tenant_factory_other", `{"enabled":true}`, http.StatusNotFound},
		{"factory-owner", "tenant_factory_api", `{}`, http.StatusBadRequest},
		{"factory-owner", "tenant_factory_api", `{"enabled":"true"}`, http.StatusBadRequest},
	} {
		response := call(path, test.subject, test.tenant, test.body)
		if response.Code != test.want {
			t.Fatalf("factory setting for %s/%s returned HTTP %d, want %d: %s", test.subject, test.tenant, response.Code, test.want, response.Body.String())
		}
	}
	var enabled bool
	if err := pool.QueryRow(ctx, `SELECT factory_enabled FROM tenant_github_repositories WHERE tenant_id='tenant_factory_api' AND installation_id=7001 AND github_repository_id=7002`).Scan(&enabled); err != nil || enabled {
		t.Fatalf("final factory setting = %v, error = %v", enabled, err)
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id='tenant_factory_api' AND action='tenant.github_repository.factory.configure'`).Scan(&auditCount); err != nil || auditCount != 2 {
		t.Fatalf("factory configuration audit count = %d, error = %v", auditCount, err)
	}
	runnerPath := "/v1/tenants/tenant_factory_api/github/installations/7001/repositories/7002/actions-runner"
	for _, test := range []struct {
		subject, tenant, body string
		want                  int
	}{
		{"factory-owner", "tenant_factory_api", `{"enabled":true}`, http.StatusOK},
		{"factory-admin", "tenant_factory_api", `{"enabled":false}`, http.StatusOK},
		{"factory-member", "tenant_factory_api", `{"enabled":true}`, http.StatusNotFound},
		{"factory-owner", "tenant_factory_other", `{"enabled":true}`, http.StatusNotFound},
		{"factory-owner", "tenant_factory_api", `{}`, http.StatusBadRequest},
	} {
		response := call(runnerPath, test.subject, test.tenant, test.body)
		if response.Code != test.want {
			t.Fatalf("Actions runner setting for %s/%s returned HTTP %d, want %d: %s", test.subject, test.tenant, response.Code, test.want, response.Body.String())
		}
	}
	if err := pool.QueryRow(ctx, `SELECT actions_runner_enabled FROM tenant_github_repositories WHERE tenant_id='tenant_factory_api' AND installation_id=7001 AND github_repository_id=7002`).Scan(&enabled); err != nil || enabled {
		t.Fatalf("final Actions runner setting = %v, error = %v", enabled, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id='tenant_factory_api' AND action='tenant.github_repository.actions_runner.configure'`).Scan(&auditCount); err != nil || auditCount != 2 {
		t.Fatalf("Actions runner configuration audit count = %d, error = %v", auditCount, err)
	}
	if response := call(runnerPath, "factory-owner", "tenant_factory_api", `{"enabled":true}`); response.Code != http.StatusOK {
		t.Fatalf("enable runner before setting workflow policy returned HTTP %d: %s", response.Code, response.Body.String())
	}
	var runnerEnabled, runnerHasEnabledAt bool
	if err := pool.QueryRow(ctx, `SELECT actions_runner_enabled,actions_runner_enabled_at IS NOT NULL FROM tenant_github_repositories WHERE tenant_id='tenant_factory_api' AND installation_id=7001 AND github_repository_id=7002`).Scan(&runnerEnabled, &runnerHasEnabledAt); err != nil || !runnerEnabled || !runnerHasEnabledAt {
		t.Fatalf("runner opt-in before policy = enabled:%v timestamp:%v err:%v", runnerEnabled, runnerHasEnabledAt, err)
	}
	workflowPolicyPath := runnerPath + "/workflow-policy"
	workflowPolicyBody := `{"workflow_path":".github/workflows/claude.yml","allowed_refs":["refs/heads/main"],"allowed_events":["push","workflow_dispatch"],"required_labels":["self-hosted","hako"],"allowed_labels":["self-hosted","hako","arm64"]}`
	for _, test := range []struct {
		subject, tenant, body string
		want                  int
	}{
		{"factory-owner", "tenant_factory_api", workflowPolicyBody, http.StatusOK},
		{"factory-member", "tenant_factory_api", workflowPolicyBody, http.StatusNotFound},
		{"factory-owner", "tenant_factory_other", workflowPolicyBody, http.StatusNotFound},
		{"factory-owner", "tenant_factory_api", `{"workflow_path":"../../evil.yml"}`, http.StatusBadRequest},
		{"factory-owner", "tenant_factory_api", `{"workflow_path":".github/workflows/claude.yml","allowed_refs":["refs/heads/main"],"allowed_events":["pull_request_target"],"required_labels":["hako"],"allowed_labels":["hako"]}`, http.StatusBadRequest},
	} {
		response := call(workflowPolicyPath, test.subject, test.tenant, test.body)
		if response.Code != test.want {
			t.Fatalf("workflow policy for %s/%s returned HTTP %d, want %d: %s", test.subject, test.tenant, response.Code, test.want, response.Body.String())
		}
	}
	var workflowPath string
	if err := pool.QueryRow(ctx, `SELECT actions_runner_workflow_policy->>'workflow_path' FROM tenant_github_repositories WHERE tenant_id='tenant_factory_api' AND installation_id=7001 AND github_repository_id=7002`).Scan(&workflowPath); err != nil || workflowPath != ".github/workflows/claude.yml" {
		t.Fatalf("persisted workflow policy path = %q, error = %v", workflowPath, err)
	}
	if response := call(runnerPath, "factory-admin", "tenant_factory_api", `{"enabled":false}`); response.Code != http.StatusOK {
		t.Fatalf("runner opt-out after workflow policy returned HTTP %d: %s", response.Code, response.Body.String())
	}
	var policyIsNull bool
	if err := pool.QueryRow(ctx, `SELECT actions_runner_workflow_policy IS NULL FROM tenant_github_repositories WHERE tenant_id='tenant_factory_api' AND installation_id=7001 AND github_repository_id=7002`).Scan(&policyIsNull); err != nil || !policyIsNull {
		t.Fatalf("runner workflow policy was not cleared after opt-out (null=%v, err=%v)", policyIsNull, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id='tenant_factory_api' AND action='tenant.github_repository.actions_runner.workflow_policy.configure'`).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("workflow policy audit count = %d, error = %v", auditCount, err)
	}
}
