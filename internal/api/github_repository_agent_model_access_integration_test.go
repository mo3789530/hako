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

func TestRepositoryAgentModelAccessRequiresOwnerFactoryAndNativeAgentCredential(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_agent_model_access','Agent model access',now()),('tenant_agent_model_other','Other',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_agent_model_owner','agent-model-owner','',now()),('usr_agent_model_member','agent-model-member','',now())`,
		`INSERT INTO tenant_members (tenant_id,user_id,role,joined_at) VALUES ('tenant_agent_model_access','usr_agent_model_owner','owner',now()),('tenant_agent_model_access','usr_agent_model_member','member',now())`,
		`INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_agent_model_access',7101,'acme','active','usr_agent_model_owner',now(),now())`,
		`INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at) VALUES (7101,'tenant_agent_model_access',now())`,
		`INSERT INTO tenant_github_repositories (tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,factory_enabled,factory_enabled_at,synchronized_at) VALUES ('tenant_agent_model_access',7101,7102,'acme','api','main',true,now(),now())`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	owner := signTenantAPIToken(t, key, issuer, "agent-model-owner", "openid hako/api")
	member := signTenantAPIToken(t, key, issuer, "agent-model-member", "openid hako/api")
	modelPath := "/v1/tenants/tenant_agent_model_access/github/installations/7101/repositories/7102/agent-model-access"
	factoryPath := "/v1/tenants/tenant_agent_model_access/github/installations/7101/repositories/7102/factory"
	call := func(token, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	if res := call(owner, modelPath, `{"enabled":true}`); res.Code != http.StatusNotFound {
		t.Fatalf("enable without Native Agent credential returned %d: %s", res.Code, res.Body.String())
	}
	if res := call(member, modelPath, `{"enabled":true}`); res.Code != http.StatusNotFound {
		t.Fatalf("Tenant member enabled model access, returned %d", res.Code)
	}
	if res := call(owner, strings.Replace(modelPath, "tenant_agent_model_access", "tenant_agent_model_other", 1), `{"enabled":true}`); res.Code != http.StatusNotFound {
		t.Fatalf("cross-Tenant model access returned %d", res.Code)
	}
	for _, query := range []string{
		`INSERT INTO tenant_model_credential_bindings (id,tenant_id,provider,purpose,secret_arn,reader_role_arn,status,created_at,updated_at)
		 VALUES ('binding_agent_model','tenant_agent_model_access','anthropic','native_agent','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/model/agent-AbCdEf','arn:aws:iam::123456789012:role/hako/model-agent-reader','approved',now(),now())`,
		`INSERT INTO tenant_model_credentials (id,tenant_id,provider,purpose,binding_id,secret_version_id,status,credential_generation,created_by,updated_by,created_at,updated_at)
		 VALUES ('credential_agent_model','tenant_agent_model_access','anthropic','native_agent','binding_agent_model','11111111111111111111111111111111','active',1,'usr_agent_model_owner','usr_agent_model_owner',now(),now())`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		body string
		want bool
	}{
		{`{"enabled":true}`, true},
		{`{"enabled":false}`, false},
		{`{"enabled":true}`, true},
	} {
		res := call(owner, modelPath, test.body)
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"execution_enabled":false`) || !strings.Contains(res.Body.String(), `"enabled":`+map[bool]string{true: "true", false: "false"}[test.want]) {
			t.Fatalf("configure model access %v returned %d: %s", test.want, res.Code, res.Body.String())
		}
	}
	var enabled, hasTimestamp bool
	if err := pool.QueryRow(ctx, `SELECT agent_model_access_enabled,agent_model_access_enabled_at IS NOT NULL FROM tenant_github_repositories WHERE tenant_id='tenant_agent_model_access' AND installation_id=7101 AND github_repository_id=7102`).Scan(&enabled, &hasTimestamp); err != nil || !enabled || !hasTimestamp {
		t.Fatalf("model access after enable = enabled:%v timestamp:%v error:%v", enabled, hasTimestamp, err)
	}
	if res := call(owner, factoryPath, `{"enabled":false}`); res.Code != http.StatusOK {
		t.Fatalf("disable Factory returned %d: %s", res.Code, res.Body.String())
	}
	if err := pool.QueryRow(ctx, `SELECT agent_model_access_enabled,agent_model_access_enabled_at IS NOT NULL FROM tenant_github_repositories WHERE tenant_id='tenant_agent_model_access' AND installation_id=7101 AND github_repository_id=7102`).Scan(&enabled, &hasTimestamp); err != nil || enabled || hasTimestamp {
		t.Fatalf("Factory opt-out did not clear model access = enabled:%v timestamp:%v error:%v", enabled, hasTimestamp, err)
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id='tenant_agent_model_access' AND action='tenant.github_repository.agent_model_access.configure'`).Scan(&auditCount); err != nil || auditCount != 3 {
		t.Fatalf("model access audit count = %d, error=%v", auditCount, err)
	}
}
