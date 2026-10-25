//go:build integration

package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/githubregistry"
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
		{ownerToken, http.MethodPost, base + "/" + id + "/select-default", `{"expected_revision":1,"expected_generation":1}`, 409},
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

func TestTenantAppVerifyRunsProofOutsideTransactionAndRechecksRevision(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_verify_apps','Verify',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_verify_apps','verify-owner','',now())`,
		`INSERT INTO tenant_members (tenant_id,user_id,role,joined_at) VALUES ('tenant_verify_apps','usr_verify_apps','owner',now())`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id) VALUES ('binding_verify_ok','tenant_verify_apps',123,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:proof-ok','oauth-ref','webhook-ref',now(),'arn:aws:iam::123456789012:role/hako/proof-reader','0123456789abcdef0123456789abcdef'),('binding_verify_fail','tenant_verify_apps',456,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:proof-fail','oauth-ref','webhook-ref',now(),'arn:aws:iam::123456789012:role/hako/proof-reader','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'),('binding_verify_race','tenant_verify_apps',789,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:proof-race','oauth-ref','webhook-ref',now(),'arn:aws:iam::123456789012:role/hako/proof-reader','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb')`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal("generate fake Cognito key failed")
	}
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "tenant-api-key", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1})}}})
	}))
	defer keys.Close()
	issuer := keys.URL + "/pool"
	verifier, err := auth.NewCognitoVerifier(auth.CognitoVerifierConfig{Issuer: issuer, ClientID: "hako-cli"})
	if err != nil {
		t.Fatal(err)
	}
	var proofCalls int
	proof := TenantGitHubAppProofVerifierFunc(func(ctx context.Context, credential githubregistry.AppProofCredential) error {
		proofCalls++
		if _, ok := ctx.Deadline(); !ok {
			t.Error("GitHub proof request has no caller deadline")
		}
		switch credential.GitHubAppID {
		case 123:
			if credential.PrivateKeyReaderRoleARN != "arn:aws:iam::123456789012:role/hako/proof-reader" || credential.PrivateKeyVersionID != "0123456789abcdef0123456789abcdef" {
				t.Error("verification did not receive the exact approved reader/VersionId pin")
			}
			return nil
		case 456:
			return errors.New("sensitive GitHub/secret error must be hidden")
		case 789:
			_, err := pool.Exec(ctx, `UPDATE tenant_github_integrations SET credential_generation = credential_generation + 1 WHERE github_app_id = 789`)
			return err
		default:
			return errors.New("unexpected App ID")
		}
	})
	handler := NewHandler(verifier, pool, WorkspaceCreateConfig{GitHubAppManagementEnabled: true, GitHubAppProofVerifier: proof})
	token := signTenantAPIToken(t, key, issuer, "verify-owner", "openid hako/api")
	withoutProofHandler := NewHandler(verifier, pool, WorkspaceCreateConfig{GitHubAppManagementEnabled: true})
	unconfiguredReq := httptest.NewRequest(http.MethodPost, "/v1/tenants/tenant_verify_apps/github/apps/unconfigured/verify", strings.NewReader(`{"expected_revision":1}`))
	unconfiguredReq.Header.Set("Content-Type", "application/json")
	unconfiguredReq.Header.Set("Authorization", "Bearer "+token)
	unconfiguredRes := httptest.NewRecorder()
	withoutProofHandler.ServeHTTP(unconfiguredRes, unconfiguredReq)
	if unconfiguredRes.Code != http.StatusConflict || !strings.Contains(unconfiguredRes.Body.String(), `"code":"github_app_not_ready"`) || strings.Contains(unconfiguredRes.Body.String(), "not implemented") {
		t.Fatalf("missing trusted proof broker response = HTTP %d: %s", unconfiguredRes.Code, unconfiguredRes.Body.String())
	}
	call := func(method, path, body, idempotency string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		if idempotency != "" {
			req.Header.Set("Idempotency-Key", idempotency)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	base := "/v1/tenants/tenant_verify_apps/github/apps"
	add := func(appID int, binding string) string {
		body := fmt.Sprintf(`{"github_app_id":%d,"slug":"app-%d","client_id":"client-%d","credential_binding_id":"%s"}`, appID, appID, appID, binding)
		res := call(http.MethodPost, base, body, fmt.Sprintf("verify-add-%d", appID))
		if res.Code != http.StatusCreated {
			t.Fatalf("App draft creation failed with HTTP %d: %s", res.Code, res.Body.String())
		}
		var wire GitHubAppMetadataResult
		if err := json.Unmarshal(res.Body.Bytes(), &wire); err != nil || wire.Integration == nil {
			t.Fatal("App create response missing integration")
		}
		return wire.Integration.ID
	}
	okID := add(123, "binding_verify_ok")
	failID := add(456, "binding_verify_fail")
	raceID := add(789, "binding_verify_race")
	verify := func(id string, revision int) *httptest.ResponseRecorder {
		return call(http.MethodPost, base+"/"+id+"/verify", fmt.Sprintf(`{"expected_revision":%d}`, revision), "")
	}
	ready := verify(okID, 1)
	var readyWire GitHubAppMetadataResult
	if ready.Code != http.StatusOK || json.Unmarshal(ready.Body.Bytes(), &readyWire) != nil || readyWire.Integration == nil || readyWire.Integration.Status != "ready" {
		t.Fatalf("successful App proof did not transition draft to ready (HTTP %d)", ready.Code)
	}
	if replay := verify(okID, 1); replay.Code != http.StatusOK || proofCalls != 1 {
		t.Fatal("retry of completed verification repeated external proof")
	}
	failure := verify(failID, 1)
	if failure.Code != http.StatusBadGateway || strings.Contains(failure.Body.String(), "sensitive GitHub") {
		t.Fatal("external verifier error was leaked or misclassified")
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM tenant_github_integrations WHERE id = $1`, failID).Scan(&status); err != nil || status != "draft" {
		t.Fatal("failed App proof changed persistent status")
	}
	if raced := verify(raceID, 1); raced.Code != http.StatusConflict {
		t.Fatal("configuration generation changed during external verification but stale result was accepted")
	}
	if got := verify(okID, 99); got.Code != http.StatusConflict {
		t.Fatal("stale verification revision was accepted")
	}
}
