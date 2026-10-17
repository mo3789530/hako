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

func TestTenantModelCredentialAPIIsOwnerScopedAndNeverReturnsSecretReferences(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_model_api_a','Model A',now()),('tenant_model_api_b','Model B',now())`,
		`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_model_api_owner','model-api-owner','',now()),('usr_model_api_member','model-api-member','',now())`,
		`INSERT INTO tenant_members(tenant_id,user_id,role,joined_at) VALUES ('tenant_model_api_a','usr_model_api_owner','owner',now()),('tenant_model_api_a','usr_model_api_member','member',now())`,
		`INSERT INTO tenant_model_credential_bindings(id,tenant_id,provider,secret_arn,reader_role_arn,status,created_at,updated_at)
		 VALUES ('binding_model_api_a','tenant_model_api_a','anthropic','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/model/a-AbCdEf',
		 'arn:aws:iam::123456789012:role/hako/model-reader-a','approved',now(),now()),
		 ('binding_model_api_b','tenant_model_api_b','anthropic','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/model/b-GhIjKl',
		 'arn:aws:iam::123456789012:role/hako/model-reader-b','approved',now(),now())`,
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
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "tenant-api-key",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1})}}})
	}))
	defer keys.Close()
	issuer := keys.URL + "/pool"
	verifier, err := auth.NewCognitoVerifier(auth.CognitoVerifierConfig{Issuer: issuer, ClientID: "hako-cli"})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(verifier, pool)
	owner := signTenantAPIToken(t, key, issuer, "model-api-owner", "openid hako/api")
	member := signTenantAPIToken(t, key, issuer, "model-api-member", "openid hako/api")
	call := func(token, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	base := "/v1/tenants/tenant_model_api_a/model-credentials"
	version1 := "11111111111111111111111111111111"
	version2 := "22222222222222222222222222222222"
	created := call(owner, http.MethodPost, base, `{"provider":"anthropic","binding_id":"binding_model_api_a","secret_version_id":"`+version1+`"}`)
	if created.Code != http.StatusOK || created.Header().Get("Cache-Control") != "no-store" ||
		strings.Contains(created.Body.String(), "hako/model/a-") || strings.Contains(created.Body.String(), "model-reader-a") {
		t.Fatalf("create response exposed secret binding metadata or failed: status=%d body=%s", created.Code, created.Body.String())
	}
	for _, test := range []struct {
		token, method, path, body string
		want                      int
	}{
		{owner, http.MethodGet, base, "", 200},
		{member, http.MethodGet, base, "", 404},
		{owner, http.MethodPost, base, `{"provider":"anthropic","binding_id":"binding_model_api_b","secret_version_id":"` + version1 + `"}`, 404},
		{owner, http.MethodPost, base, `{"provider":"anthropic","binding_id":"binding_model_api_a","secret":"sk-ant-forbidden"}`, 400},
		{owner, http.MethodPost, base + "/anthropic/rotate", `{"secret_version_id":"` + version2 + `","expected_generation":1}`, 200},
		{owner, http.MethodPost, base + "/anthropic/rotate", `{"secret_version_id":"` + version1 + `","expected_generation":1}`, 409},
		{owner, http.MethodPost, base + "/anthropic/disable", `{"expected_generation":2}`, 200},
	} {
		res := call(test.token, test.method, test.path, test.body)
		if res.Code != test.want {
			t.Fatalf("%s %s returned HTTP %d, want %d: %s", test.method, test.path, res.Code, test.want, res.Body.String())
		}
	}
	var active int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tenant_model_credentials WHERE tenant_id='tenant_model_api_a' AND status='active'`).Scan(&active); err != nil || active != 0 {
		t.Fatalf("disable did not revoke active metadata: active=%d err=%v", active, err)
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action LIKE 'tenant.model_credential.%'`).Scan(&auditCount); err != nil || auditCount != 3 {
		t.Fatalf("credential changes were not audited atomically: count=%d err=%v", auditCount, err)
	}
}
