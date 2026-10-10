//go:build integration

package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestTenantAppMetadataAPIIsDraftOnlyOwnerScopedAndDefaultOff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_apps','Apps',now()),('tenant_other_apps','Other',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_apps_owner','apps-owner','',now()),('usr_apps_admin','apps-admin','',now()),('usr_apps_member','apps-member','',now())`,
		`INSERT INTO tenant_members (tenant_id,user_id,role,joined_at) VALUES ('tenant_apps','usr_apps_owner','owner',now()),('tenant_apps','usr_apps_admin','admin',now()),('tenant_apps','usr_apps_member','member',now())`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at) VALUES ('binding_apps','tenant_apps',123,'approved','private-ref','oauth-ref','webhook-ref',now()),('binding_other','tenant_other_apps',456,'approved','other-private-ref','other-oauth-ref','other-webhook-ref',now())`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal("generate fake Cognito key failed")
	}
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "tenant-api-key", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1})}}})
	}))
	defer keys.Close()
	issuer := keys.URL + "/pool"
	verifier, err := auth.NewCognitoVerifier(auth.CognitoVerifierConfig{Issuer: issuer, ClientID: "hako-cli"})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(verifier, pool, WorkspaceCreateConfig{GitHubAppManagementEnabled: true})
	ownerToken := signTenantAPIToken(t, key, issuer, "apps-owner", "openid hako/api")
	adminToken := signTenantAPIToken(t, key, issuer, "apps-admin", "openid hako/api")
	memberToken := signTenantAPIToken(t, key, issuer, "apps-member", "openid hako/api")
	call := func(token, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", "app-create-request")
		if strings.Contains(body, `"github_app_id":456`) {
			req.Header.Set("Idempotency-Key", "other-app-request")
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	base := "/v1/tenants/tenant_apps/github/apps"
	body := `{"github_app_id":123,"slug":"hako-tenant","client_id":"Iv1.client","credential_binding_id":"binding_apps"}`
	created := call(ownerToken, http.MethodPost, base, body)
	if created.Code != http.StatusCreated {
		t.Fatalf("create metadata returned HTTP %d", created.Code)
	}
	var wire GitHubAppMetadataResult
	if json.Unmarshal(created.Body.Bytes(), &wire) != nil || wire.Integration == nil || wire.Integration.Status != "draft" {
		t.Fatal("created integration is not an inert draft")
	}
	id := wire.Integration.ID
	if strings.Contains(created.Body.String(), "private-ref") || strings.Contains(created.Body.String(), "binding_apps") {
		t.Fatal("response exposed credential binding references")
	}
	if call(ownerToken, http.MethodPost, base, body).Code != http.StatusOK {
		t.Fatal("idempotent create retry failed")
	}
	listed := call(ownerToken, http.MethodGet, base, "")
	var listWire GitHubAppMetadataResult
	if listed.Code != http.StatusOK || json.Unmarshal(listed.Body.Bytes(), &listWire) != nil || listWire.Limit != 50 {
		t.Fatal("App list did not use the documented default page size")
	}
	changed := strings.Replace(body, "hako-tenant", "hako-changed", 1)
	if call(ownerToken, http.MethodPost, base, changed).Code != http.StatusConflict {
		t.Fatal("changed idempotent request accepted")
	}
	for _, test := range []struct {
		token, method, path, body string
		want                      int
	}{
		{ownerToken, http.MethodGet, base, "", 200},
		{adminToken, http.MethodGet, base + "/" + id, "", 200},
		{memberToken, http.MethodGet, base, "", 404},
		{memberToken, http.MethodPost, base, body, 404},
		{ownerToken, http.MethodGet, "/v1/tenants/tenant_other_apps/github/apps", "", 404},
		{ownerToken, http.MethodPost, base, `{"github_app_id":456,"slug":"other","client_id":"client","credential_binding_id":"binding_other"}`, 404},
		{ownerToken, http.MethodPost, base, `{"github_app_id":123,"private_key_secret_arn":"arbitrary"}`, 400},
		{ownerToken, http.MethodPost, base + "/" + id + "/verify", `{"expected_revision":1}`, 409},
		{ownerToken, http.MethodPost, base + "/" + id + "/select-default", `{"expected_revision":1}`, 409},
		{ownerToken, http.MethodPost, base + "/" + id + "/disable", `{"expected_revision":99}`, 409},
		{adminToken, http.MethodPost, base + "/" + id + "/disable", `{"expected_revision":1}`, 200},
	} {
		t.Run(fmt.Sprintf("%s-%s-%d", test.method, test.path, test.want), func(t *testing.T) {
			if got := call(test.token, test.method, test.path, test.body); got.Code != test.want {
				t.Fatalf("metadata request HTTP %d; want %d", got.Code, test.want)
			}
		})
	}
	var defaults, active int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tenant_github_default_integrations`).Scan(&defaults); err != nil || defaults != 0 {
		t.Fatal("metadata API changed defaults")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tenant_github_integrations WHERE status = 'active'`).Scan(&active); err != nil || active != 0 {
		t.Fatal("metadata API activated an App")
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action IN ('tenant.github_app.add','tenant.github_app.disable')`).Scan(&auditCount); err != nil || auditCount != 2 {
		t.Fatal("creation/disable audit transaction or replay dedup failed")
	}
	// Default configuration does not expose management routes at all.
	defaultHandler := NewHandler(verifier, pool)
	req := httptest.NewRequest(http.MethodGet, base, nil)
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	res := httptest.NewRecorder()
	defaultHandler.ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatal("App management unexpectedly enabled by default")
	}
}
