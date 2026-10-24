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
	"github.com/mo3789530/hako/internal/deploypolicy"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestTenantSoftwareCatalogRBACRepositoryAndDependencyFences(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_catalog_a','Catalog A',now()),('tenant_catalog_b','Catalog B',now())`,
		`INSERT INTO users(id,cognito_subject,email,created_at) VALUES
		 ('usr_catalog_owner_a','catalog-owner-a','',now()),('usr_catalog_member_a','catalog-member-a','',now()),('usr_catalog_owner_b','catalog-owner-b','',now())`,
		`INSERT INTO tenant_members(tenant_id,user_id,role,joined_at) VALUES
		 ('tenant_catalog_a','usr_catalog_owner_a','owner',now()),('tenant_catalog_a','usr_catalog_member_a','member',now()),
		 ('tenant_catalog_b','usr_catalog_owner_b','owner',now())`,
		`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES
		 ('tenant_catalog_a',1101,'acme','active','usr_catalog_owner_a',now(),now()),
		 ('tenant_catalog_b',2202,'other','active','usr_catalog_owner_b',now(),now())`,
		`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES
		 (1101,'tenant_catalog_a',now()),(2202,'tenant_catalog_b',now())`,
		`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at) VALUES
		 ('tenant_catalog_a',1101,11001,'acme','payments','main',now()),
		 ('tenant_catalog_a',1101,11002,'acme','users','main',now()),
		 ('tenant_catalog_b',2202,22001,'other','secret','main',now())`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatalf("seed catalog fixture: %v", err)
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "tenant-api-key",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1})}}})
	}))
	defer keys.Close()
	issuer := keys.URL + "/pool"
	verifier, err := auth.NewCognitoVerifier(auth.CognitoVerifierConfig{Issuer: issuer, ClientID: "hako-cli"})
	if err != nil {
		t.Fatal(err)
	}
	policyVerifier := catalogOIDCVerifier{}
	handler := NewHandler(verifier, pool, WorkspaceCreateConfig{GitHubOIDCPolicyVerifier: policyVerifier, GitHubEnvironmentApprovalVerifier: catalogEnvironmentApprovalVerifier{}})
	tokens := map[string]string{
		"owner_a":  signTenantAPIToken(t, key, issuer, "catalog-owner-a", "openid hako/api"),
		"member_a": signTenantAPIToken(t, key, issuer, "catalog-member-a", "openid hako/api"),
		"owner_b":  signTenantAPIToken(t, key, issuer, "catalog-owner-b", "openid hako/api"),
	}
	call := func(actor, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tokens[actor])
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	baseA := "/v1/tenants/tenant_catalog_a/software-catalog/"
	baseB := "/v1/tenants/tenant_catalog_b/software-catalog/"
	if res := call("owner_a", http.MethodGet, "/v1/tenants/tenant_catalog_a/membership", ""); res.Code != http.StatusOK {
		t.Fatalf("owner identity/membership setup failed: %d %s", res.Code, res.Body.String())
	}
	users := `{"owner_team":"identity","installation_id":1101,"github_repository_id":11002,"runtime":"lambda-microvm","language":"go","dependencies":[]}`
	payments := func(dependencies string) string {
		return `{"owner_team":"payments","installation_id":1101,"github_repository_id":11001,"runtime":"lambda-microvm","language":"go","dependencies":` + dependencies + `,"production_url":"https://payments.example.com"}`
	}
	if got := call("owner_a", http.MethodPut, baseA+"users-api", users); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"repository":"acme/users"`) {
		t.Fatalf("create catalog service = %d %s", got.Code, got.Body.String())
	}
	if got := call("owner_a", http.MethodPut, baseA+"payments-api", payments(`["users-api"]`)); got.Code != http.StatusOK {
		t.Fatalf("create service with same-Tenant dependency = %d %s", got.Code, got.Body.String())
	}
	policyPath := baseA + "payments-api/deploy-policy"
	validPolicy := `{"enabled":true,"audience":"sts.amazonaws.com","allowed_refs":["refs/heads/main"],"environments":["production"],"allowed_workflow_refs":["acme/payments/.github/workflows/deploy.yml@refs/heads/main"],"require_human_approval":true}`
	if got := call("member_a", http.MethodPut, policyPath, validPolicy); got.Code != http.StatusNotFound {
		t.Fatalf("Tenant member configured deploy policy: %d %s", got.Code, got.Body.String())
	}
	if got := call("owner_a", http.MethodPut, policyPath, `{"enabled":true,"audience":"sts.amazonaws.com","allowed_refs":["refs/heads/main"],"environments":["production"],"require_human_approval":false}`); got.Code != http.StatusBadRequest {
		t.Fatalf("policy without explicit approval accepted: %d %s", got.Code, got.Body.String())
	}
	if got := call("owner_a", http.MethodPut, policyPath, validPolicy); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"execution_enabled":false`) {
		t.Fatalf("Owner configured deploy policy: %d %s", got.Code, got.Body.String())
	}
	if got := call("member_a", http.MethodGet, policyPath, ""); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"audience":"sts.amazonaws.com"`) {
		t.Fatalf("Tenant member read deploy policy: %d %s", got.Code, got.Body.String())
	}
	if got := call("member_a", http.MethodPost, policyPath+"/verify", `{"token":"synthetic-valid-token"}`); got.Code != http.StatusOK ||
		!strings.Contains(got.Body.String(), `"valid":true`) || !strings.Contains(got.Body.String(), `"environment_approval_verified":true`) ||
		!strings.Contains(got.Body.String(), `"execution_enabled":false`) || strings.Contains(got.Body.String(), "synthetic-valid-token") {
		t.Fatalf("Tenant member verified dormant OIDC policy: %d %s", got.Code, got.Body.String())
	}
	withoutApprovalCheck := NewHandler(verifier, pool, WorkspaceCreateConfig{GitHubOIDCPolicyVerifier: policyVerifier})
	request := httptest.NewRequest(http.MethodPost, policyPath+"/verify", strings.NewReader(`{"token":"synthetic-valid-token"}`))
	request.Header.Set("Authorization", "Bearer "+tokens["member_a"])
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	withoutApprovalCheck.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), `"valid":true`) {
		t.Fatalf("OIDC accepted without Environment approval verifier: %d %s", response.Code, response.Body.String())
	}
	unavailableApprovalCheck := NewHandler(verifier, pool, WorkspaceCreateConfig{GitHubOIDCPolicyVerifier: policyVerifier,
		GitHubEnvironmentApprovalVerifier: catalogUnavailableEnvironmentApprovalVerifier{}})
	request = httptest.NewRequest(http.MethodPost, policyPath+"/verify", strings.NewReader(`{"token":"synthetic-valid-token"}`))
	request.Header.Set("Authorization", "Bearer "+tokens["member_a"])
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	unavailableApprovalCheck.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), `"valid":true`) {
		t.Fatalf("OIDC accepted while Environment approval verifier is unavailable: %d %s", response.Code, response.Body.String())
	}
	if got := call("member_a", http.MethodPost, policyPath+"/verify", `{"token":"invalid-token"}`); got.Code != http.StatusForbidden {
		t.Fatalf("invalid OIDC claims were not rejected: %d %s", got.Code, got.Body.String())
	}
	if got := call("owner_a", http.MethodPost, baseB+"foreign/deploy-policy/verify", `{"token":"synthetic-valid-token"}`); got.Code != http.StatusNotFound {
		t.Fatalf("cross-Tenant OIDC policy visible: %d %s", got.Code, got.Body.String())
	}
	if got := call("owner_a", http.MethodGet, baseB+"foreign/deploy-policy", ""); got.Code != http.StatusNotFound {
		t.Fatalf("cross-Tenant deploy policy visible: %d %s", got.Code, got.Body.String())
	}
	for _, test := range []struct {
		name, actor, path, body string
		want                    int
	}{
		{"member cannot write", "member_a", baseA + "users-api", users, http.StatusNotFound},
		{"cross-Tenant repository hidden", "owner_a", baseA + "foreign", `{"owner_team":"payments","installation_id":2202,"github_repository_id":22001,"runtime":"lambda-microvm","language":"go","dependencies":[]}`, http.StatusNotFound},
		{"missing dependency rejected", "owner_a", baseA + "broken-api", payments(`["missing"]`), http.StatusBadRequest},
		{"cross-Tenant catalog hidden", "owner_a", baseB, "", http.StatusNotFound},
		{"EC2 runtime rejected", "owner_a", baseA + "ec2-api", `{"owner_team":"ops","installation_id":1101,"github_repository_id":11001,"runtime":"ec2","language":"go","dependencies":[]}`, http.StatusBadRequest},
		{"secret field rejected", "owner_a", baseA + "payments-api", `{"owner_team":"payments","installation_id":1101,"github_repository_id":11001,"runtime":"lambda-microvm","language":"go","dependencies":[],"secret":"do-not-store"}`, http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			res := call(test.actor, http.MethodPut, test.path, test.body)
			if res.Code != test.want {
				t.Fatalf("HTTP %d, want %d: %s", res.Code, test.want, res.Body.String())
			}
		})
	}
	listed := call("member_a", http.MethodGet, strings.TrimSuffix(baseA, "/"), "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"service_id":"payments-api"`) || strings.Contains(listed.Body.String(), "do-not-store") {
		t.Fatalf("Tenant member list = %d %s", listed.Code, listed.Body.String())
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id='tenant_catalog_a' AND action IN ('tenant.software_catalog.upsert','tenant.software_catalog.deploy_policy.set')`).Scan(&auditCount); err != nil || auditCount != 3 {
		t.Fatalf("catalog audit count = %d, error=%v", auditCount, err)
	}
}

type catalogOIDCVerifier struct{}

type catalogEnvironmentApprovalVerifier struct{}

type catalogUnavailableEnvironmentApprovalVerifier struct{}

func (catalogUnavailableEnvironmentApprovalVerifier) Verify(context.Context, domain.TenantID, string, int64, int64, string) error {
	return deploypolicy.ErrUnavailable
}

func (catalogEnvironmentApprovalVerifier) Verify(_ context.Context, tenantID domain.TenantID, serviceID string, installationID, repositoryID int64, environment string) error {
	if tenantID != "tenant_catalog_a" || serviceID != "payments-api" || installationID != 1101 || repositoryID != 11001 || environment != "production" {
		return deploypolicy.ErrInvalid
	}
	return nil
}

func (catalogOIDCVerifier) Verify(_ context.Context, token string, policy deploypolicy.Policy, repository string, repositoryID int64) (deploypolicy.Claims, error) {
	if token != "synthetic-valid-token" || policy.Validate() != nil || repository != "acme/payments" || repositoryID != 11001 {
		return deploypolicy.Claims{}, deploypolicy.ErrInvalidToken
	}
	return deploypolicy.Claims{Repository: repository, RepositoryID: "11001", Ref: "refs/heads/main", Environment: "production",
		WorkflowRef: "acme/payments/.github/workflows/deploy.yml@refs/heads/main", EventName: "workflow_dispatch", RunID: "run-1", RunAttempt: "1"}, nil
}
