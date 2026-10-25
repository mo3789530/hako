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
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/dsql"
	webhookstore "github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestTenantWebhookReplayAPIIsAdminOnlyAndDoesNotExposeRawPayload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_replay_api','Replay API',now()),('tenant_replay_other','Other Tenant',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_replay_owner','replay-owner','',now()),('usr_replay_admin','replay-admin','',now()),('usr_replay_member','replay-member','',now())`,
		`INSERT INTO tenant_members (tenant_id,user_id,role,joined_at) VALUES ('tenant_replay_api','usr_replay_owner','owner',now()),('tenant_replay_api','usr_replay_admin','admin',now()),('tenant_replay_api','usr_replay_member','member',now())`,
		`INSERT INTO github_app_registrations (id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_replay_api',9521,'tenant_owned','tenant_replay_api','replay-app','client',now())`,
		`INSERT INTO github_app_registrations (id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_replay_other',9524,'tenant_owned','tenant_replay_other','other-app','other-client',now())`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at) VALUES ('binding_replay_api','tenant_replay_api',9521,'approved','private','oauth','webhook',now())`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at) VALUES ('binding_replay_other','tenant_replay_other',9524,'approved','other-private','other-oauth','other-webhook',now())`,
		`INSERT INTO tenant_github_integrations (id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_replay_api','tenant_replay_api','registration_replay_api',9521,'binding_replay_api','active',2,3,'replay-api-key','replay-api-hash','usr_replay_owner',now(),now())`,
		`INSERT INTO tenant_github_integrations (id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_replay_other','tenant_replay_other','registration_replay_other',9524,'binding_replay_other','active',1,1,'replay-other-key','replay-other-hash','usr_replay_owner',now(),now())`,
		`INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_replay_api',9522,'replay-owner','active','usr_replay_owner',now(),now())`,
		`INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at) VALUES (9522,'tenant_replay_api',now())`,
		`INSERT INTO tenant_github_app_installation_bindings (tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_replay_api','integration_replay_api',9522,now())`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	delivery := githubwebhook.VerifiedDelivery{IntegrationID: "integration_replay_api", CredentialGeneration: 3,
		DeliveryID: "b2b2c3d4-e5f6-4789-8abc-def012345678", Event: "installation_repositories", Action: "added",
		Payload: []byte(`{"action":"added","installation":{"id":9522},"repositories_added":[{"id":9523,"name":"replay-api","default_branch":"main","owner":{"login":"replay-owner"}}]}`)}
	if inserted, err := (webhookstore.Inbox{Pool: pool}).Record(ctx, delivery, time.Now().UTC()); err != nil || !inserted {
		t.Fatalf("record Tenant webhook delivery: inserted=%v err=%v", inserted, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='failed' WHERE integration_id=$1 AND delivery_id=$2`, delivery.IntegrationID, delivery.DeliveryID); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
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
	handler := NewHandler(verifier, pool, WorkspaceCreateConfig{GitHubAppManagementEnabled: true})
	ownerToken := signTenantAPIToken(t, key, issuer, "replay-owner", "openid hako/api")
	adminToken := signTenantAPIToken(t, key, issuer, "replay-admin", "openid hako/api")
	memberToken := signTenantAPIToken(t, key, issuer, "replay-member", "openid hako/api")
	path := "/v1/tenants/tenant_replay_api/github/apps/integration_replay_api/deliveries/" + delivery.DeliveryID + "/replay"
	listPath := "/v1/tenants/tenant_replay_api/github/apps/integration_replay_api/deliveries?limit=10&offset=0"
	callBody := func(token, requestPath, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, requestPath, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	call := func(token string) *httptest.ResponseRecorder {
		return callBody(token, path, `{"reason":"retry reviewed processor failure"}`)
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: `{`},
		{name: "missing reason", body: `{}`},
		{name: "short reason", body: `{"reason":"short"}`},
		{name: "oversized reason", body: `{"reason":"` + strings.Repeat("x", webhookstore.MaxWebhookReplayReasonBytes+1) + `"}`},
	} {
		t.Run("invalid replay "+test.name, func(t *testing.T) {
			response := callBody(ownerToken, path, test.body)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"invalid_request"`) {
				t.Fatalf("invalid replay request HTTP %d: %s", response.Code, response.Body.String())
			}
		})
	}
	if response := call(memberToken); response.Code != http.StatusNotFound {
		t.Fatalf("non-admin replay HTTP %d, want 404", response.Code)
	}
	otherDeliveryPath := "/v1/tenants/tenant_replay_api/github/apps/integration_replay_other/deliveries/" + delivery.DeliveryID + "/replay"
	if response := callBody(ownerToken, otherDeliveryPath, `{"reason":"retry reviewed processor failure"}`); response.Code != http.StatusNotFound {
		t.Fatalf("foreign Tenant integration replay HTTP %d, want hidden 404", response.Code)
	}
	listAs := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, listPath, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	if response := listAs(memberToken); response.Code != http.StatusNotFound {
		t.Fatalf("non-admin failed-delivery list HTTP %d, want 404", response.Code)
	}
	crossTenantPath := "/v1/tenants/tenant_replay_api/github/apps/integration_replay_other/deliveries"
	crossTenantRequest := httptest.NewRequest(http.MethodGet, crossTenantPath, nil)
	crossTenantRequest.Header.Set("Authorization", "Bearer "+ownerToken)
	crossTenantResponse := httptest.NewRecorder()
	handler.ServeHTTP(crossTenantResponse, crossTenantRequest)
	if crossTenantResponse.Code != http.StatusNotFound {
		t.Fatalf("cross-Tenant integration listing HTTP %d, want 404", crossTenantResponse.Code)
	}
	listed := listAs(ownerToken)
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), string(delivery.Payload)) || strings.Contains(listed.Body.String(), "normalized_event_json") || strings.Contains(listed.Body.String(), "payload_json") {
		t.Fatalf("failed-delivery list HTTP %d exposed payload or failed: %s", listed.Code, listed.Body.String())
	}
	var listResult struct {
		Deliveries []struct {
			DeliveryID  string `json:"delivery_id"`
			ReplayCount int    `json:"replay_count"`
		} `json:"deliveries"`
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &listResult); err != nil || listResult.Total != 1 || len(listResult.Deliveries) != 1 || listResult.Deliveries[0].DeliveryID != delivery.DeliveryID {
		t.Fatalf("failed-delivery list result = %+v err=%v", listResult, err)
	}
	response := call(ownerToken)
	if response.Code != http.StatusAccepted || strings.Contains(response.Body.String(), string(delivery.Payload)) || strings.Contains(response.Body.String(), "payload_json") {
		t.Fatalf("owner replay response HTTP %d exposed payload or failed: %s", response.Code, response.Body.String())
	}
	if _, err := pool.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='failed' WHERE integration_id=$1 AND delivery_id=$2`, delivery.IntegrationID, delivery.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if response := call(adminToken); response.Code != http.StatusAccepted {
		t.Fatalf("Tenant Admin replay HTTP %d, want 202: %s", response.Code, response.Body.String())
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT processing_status FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2`, delivery.IntegrationID, delivery.DeliveryID).Scan(&status); err != nil || status != "received" {
		t.Fatalf("delivery state after replay = %q err=%v", status, err)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id='tenant_replay_api' AND actor_user_id IN ('usr_replay_owner','usr_replay_admin')
		AND action='github.webhook.replay_requested' AND target_id=$1`, delivery.DeliveryID).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("owner/admin replay audit count = %d err=%v", audits, err)
	}
	if response := call(ownerToken); response.Code != http.StatusConflict {
		t.Fatalf("duplicate replay of non-failed delivery HTTP %d, want 409", response.Code)
	}
	if _, err := pool.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='failed',replay_count=$3,replay_window_started_at=now() WHERE integration_id=$1 AND delivery_id=$2`, delivery.IntegrationID, delivery.DeliveryID, webhookstore.MaxWebhookReplaysPerHour); err != nil {
		t.Fatal(err)
	}
	if response := call(adminToken); response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "3600" {
		t.Fatalf("rate-limited replay HTTP %d Retry-After=%q, want 429/3600: %s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
}
