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
	"sync"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/dsql"
	webhookstore "github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestPlatformWebhookDeadLetterAPIRequiresPlatformAdminRedactsAndAudits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	seed := []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_dlq_api','DLQ Tenant',now()),('tenant_dlq_second','Second DLQ Tenant',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_dlq_operator','dlq-operator','',now()),('usr_dlq_member','dlq-member','',now()),('usr_dlq_owner','dlq-owner','',now())`,
		`INSERT INTO tenant_members (tenant_id,user_id,role,joined_at) VALUES ('tenant_dlq_api','usr_dlq_owner','owner',now())`,
		`INSERT INTO github_app_registrations (id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_dlq_api',9821,'tenant_owned','tenant_dlq_api','dlq-app','client',now())`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at) VALUES ('binding_dlq_api','tenant_dlq_api',9821,'approved','private','oauth','webhook',now())`,
		`INSERT INTO tenant_github_integrations (id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_dlq_api','tenant_dlq_api','registration_dlq_api',9821,'binding_dlq_api','active',1,2,'dlq-api-key','dlq-api-hash','usr_dlq_owner',now(),now())`,
		`INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_dlq_api',9822,'dlq-owner','active','usr_dlq_owner',now(),now())`,
		`INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at) VALUES (9822,'tenant_dlq_api',now())`,
		`INSERT INTO tenant_github_app_installation_bindings (tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_dlq_api','integration_dlq_api',9822,now())`,
		`INSERT INTO github_app_registrations (id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_dlq_second',9921,'tenant_owned','tenant_dlq_second','second-dlq-app','client-2',now())`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at) VALUES ('binding_dlq_second','tenant_dlq_second',9921,'approved','private-2','oauth-2','webhook-2',now())`,
		`INSERT INTO tenant_github_integrations (id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_dlq_second','tenant_dlq_second','registration_dlq_second',9921,'binding_dlq_second','active',1,1,'dlq-second-key','dlq-second-hash','usr_dlq_operator',now(),now())`,
		`INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_dlq_second',9922,'second-owner','active','usr_dlq_operator',now(),now())`,
		`INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at) VALUES (9922,'tenant_dlq_second',now())`,
		`INSERT INTO tenant_github_app_installation_bindings (tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_dlq_second','integration_dlq_second',9922,now())`,
	}
	for _, statement := range seed {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	delivery := githubwebhook.VerifiedDelivery{IntegrationID: "integration_dlq_api", CredentialGeneration: 2,
		DeliveryID: "c2c2c3d4-e5f6-4789-8abc-def012345678", Event: "push",
		Payload: []byte(`{"ref":"refs/heads/main","after":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","installation":{"id":9822},"repository":{"id":9823,"name":"api","owner":{"login":"dlq-owner"}},"sender":{"login":"octocat"}}`)}
	if inserted, err := (webhookstore.Inbox{Pool: pool}).Record(ctx, delivery, time.Now().UTC()); err != nil || !inserted {
		t.Fatalf("record failed-delivery fixture: inserted=%v err=%v", inserted, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='failed' WHERE integration_id=$1 AND delivery_id=$2`, delivery.IntegrationID, delivery.DeliveryID); err != nil {
		t.Fatal(err)
	}
	secondDelivery := githubwebhook.VerifiedDelivery{IntegrationID: "integration_dlq_second", CredentialGeneration: 1,
		DeliveryID: "d2c2c3d4-e5f6-4789-8abc-def012345678", Event: "push",
		Payload: []byte(`{"ref":"refs/heads/main","after":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","installation":{"id":9922},"repository":{"id":9923,"name":"second-api","owner":{"login":"second-owner"}}}`)}
	if inserted, err := (webhookstore.Inbox{Pool: pool}).Record(ctx, secondDelivery, time.Now().Add(-time.Hour).UTC()); err != nil || !inserted {
		t.Fatalf("record second Tenant delivery: inserted=%v err=%v", inserted, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='failed' WHERE integration_id=$1 AND delivery_id=$2`, secondDelivery.IntegrationID, secondDelivery.DeliveryID); err != nil {
		t.Fatal(err)
	}
	sharedDelivery := githubwebhook.VerifiedDelivery{DeliveryID: "e2c2c3d4-e5f6-4789-8abc-def012345678", Event: "push",
		Payload: []byte(`{"ref":"refs/heads/main","after":"cccccccccccccccccccccccccccccccccccccccc","installation":{"id":9925},"repository":{"id":9924,"name":"shared-api","owner":{"login":"shared-owner"}}}`)}
	if inserted, err := (webhookstore.Inbox{Pool: pool}).Record(ctx, sharedDelivery, time.Now().UTC()); err != nil || !inserted {
		t.Fatalf("record legacy shared delivery: inserted=%v err=%v", inserted, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='failed' WHERE integration_id='shared' AND delivery_id=$1`, sharedDelivery.DeliveryID); err != nil {
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
	handler := NewHandler(verifier, pool)
	admin := signAPITokenWithGroups(t, key, issuer, "dlq-operator", "openid hako/api", []string{"hako-admin"})
	member := signAPITokenWithGroups(t, key, issuer, "dlq-member", "openid hako/api", nil)
	owner := signAPITokenWithGroups(t, key, issuer, "dlq-owner", "openid hako/api", nil)
	listPath := "/v1/admin/github/webhook-deliveries?limit=10&offset=0"
	replayPath := "/v1/admin/github/webhook-deliveries/" + delivery.IntegrationID + "/" + delivery.DeliveryID + "/replay"
	request := func(method, path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	for _, token := range []string{member, owner} {
		if response := request(http.MethodGet, listPath, token, ""); response.Code != http.StatusForbidden {
			t.Fatalf("non-platform caller list status=%d body=%s", response.Code, response.Body.String())
		}
		if response := request(http.MethodPost, replayPath, token, `{"reason":"operator reviewed transient failure"}`); response.Code != http.StatusForbidden {
			t.Fatalf("non-platform caller replay status=%d body=%s", response.Code, response.Body.String())
		}
	}
	listed := request(http.MethodGet, listPath, admin, "")
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), string(delivery.Payload)) || strings.Contains(listed.Body.String(), "normalized_event_json") || strings.Contains(listed.Body.String(), "payload_json") {
		t.Fatalf("platform listing status=%d exposed payload or failed: %s", listed.Code, listed.Body.String())
	}
	var result platformFailedDeliveriesResponse
	if err := json.Unmarshal(listed.Body.Bytes(), &result); err != nil || result.Total != 2 || len(result.Deliveries) != 2 {
		t.Fatalf("platform list result=%+v err=%v", result, err)
	}
	item := result.Deliveries[0]
	if item.TenantID != "tenant_dlq_api" || item.IntegrationID != delivery.IntegrationID || item.InstallationID == nil || *item.InstallationID != 9822 || item.RepositoryID == nil || *item.RepositoryID != 9823 || !item.ReplayEligible {
		t.Fatalf("platform delivery metadata=%+v", item)
	}
	if result.Deliveries[1].TenantID != "tenant_dlq_second" || result.Deliveries[1].IntegrationID != secondDelivery.IntegrationID {
		t.Fatalf("cross-Tenant failed delivery missing from platform page: %+v", result.Deliveries)
	}
	paged := request(http.MethodGet, "/v1/admin/github/webhook-deliveries?limit=1&offset=0", admin, "")
	var page platformFailedDeliveriesResponse
	if paged.Code != http.StatusOK || json.Unmarshal(paged.Body.Bytes(), &page) != nil || page.Total != 2 || len(page.Deliveries) != 1 {
		t.Fatalf("platform pagination status=%d page=%+v body=%s", paged.Code, page, paged.Body.String())
	}
	var replayWG sync.WaitGroup
	replayResults := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		replayWG.Add(1)
		go func() {
			defer replayWG.Done()
			replayResults <- request(http.MethodPost, replayPath, admin, `{"reason":"operator reviewed transient failure"}`)
		}()
	}
	replayWG.Wait()
	close(replayResults)
	accepted, conflict := 0, 0
	for replay := range replayResults {
		if strings.Contains(replay.Body.String(), string(delivery.Payload)) {
			t.Fatalf("platform replay exposed webhook payload: %s", replay.Body.String())
		}
		switch replay.Code {
		case http.StatusAccepted:
			accepted++
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("concurrent platform replay status=%d body=%s", replay.Code, replay.Body.String())
		}
	}
	if accepted != 1 || conflict != 1 {
		t.Fatalf("concurrent platform replays accepted=%d conflict=%d, want one each", accepted, conflict)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT processing_status FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2`, delivery.IntegrationID, delivery.DeliveryID).Scan(&status); err != nil || status != "received" {
		t.Fatalf("status after replay=%q err=%v", status, err)
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM platform_audit_events WHERE actor_user_id='usr_dlq_operator' AND action='github.webhook.delivery.replay_requested' AND target_id=$1`, delivery.IntegrationID+"/"+delivery.DeliveryID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("platform replay audit count=%d err=%v", auditCount, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='failed',payload_expires_at=now()-interval '1 second' WHERE integration_id=$1 AND delivery_id=$2`, delivery.IntegrationID, delivery.DeliveryID); err != nil {
		t.Fatal(err)
	}
	expiredList := request(http.MethodGet, listPath, admin, "")
	if expiredList.Code != http.StatusOK || !strings.Contains(expiredList.Body.String(), `"replay_eligible":false`) {
		t.Fatalf("expired delivery listing status=%d body=%s", expiredList.Code, expiredList.Body.String())
	}
	if expiredReplay := request(http.MethodPost, replayPath, admin, `{"reason":"operator reviewed expired payload"}`); expiredReplay.Code != http.StatusConflict {
		t.Fatalf("expired delivery replay status=%d body=%s", expiredReplay.Code, expiredReplay.Body.String())
	}
	if _, err := pool.Exec(ctx, `UPDATE github_webhook_deliveries SET payload_expires_at=now()+interval '1 day' WHERE integration_id=$1 AND delivery_id=$2`, delivery.IntegrationID, delivery.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_installations SET suspended_at=now() WHERE tenant_id='tenant_dlq_api' AND installation_id=9822`); err != nil {
		t.Fatal(err)
	}
	if staleReplay := request(http.MethodPost, replayPath, admin, `{"reason":"operator reviewed suspended integration"}`); staleReplay.Code != http.StatusConflict {
		t.Fatalf("suspended Installation replay status=%d body=%s", staleReplay.Code, staleReplay.Body.String())
	}
	if err := pool.QueryRow(ctx, `SELECT processing_status FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2`, delivery.IntegrationID, delivery.DeliveryID).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("ineligible replay changed delivery state to %q, err=%v", status, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_installations SET suspended_at=NULL WHERE tenant_id='tenant_dlq_api' AND installation_id=9822`); err != nil {
		t.Fatal(err)
	}
	for replayNumber := 2; replayNumber <= webhookstore.MaxWebhookReplaysPerHour; replayNumber++ {
		if _, err := pool.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='failed' WHERE integration_id=$1 AND delivery_id=$2`, delivery.IntegrationID, delivery.DeliveryID); err != nil {
			t.Fatal(err)
		}
		if replay := request(http.MethodPost, replayPath, admin, `{"reason":"operator retry approved failure"}`); replay.Code != http.StatusAccepted {
			t.Fatalf("allowed replay %d status=%d body=%s", replayNumber, replay.Code, replay.Body.String())
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='failed' WHERE integration_id=$1 AND delivery_id=$2`, delivery.IntegrationID, delivery.DeliveryID); err != nil {
		t.Fatal(err)
	}
	limited := request(http.MethodPost, replayPath, admin, `{"reason":"operator retry approved failure"}`)
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("rate-limited replay status=%d body=%s", limited.Code, limited.Body.String())
	}
	if _, err := pool.Exec(ctx, `UPDATE github_webhook_deliveries SET payload_expires_at=now()+interval '1 day' WHERE integration_id=$1 AND delivery_id=$2`, delivery.IntegrationID, delivery.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE github_webhook_deliveries SET replay_window_started_at=now()-interval '2 hours',replay_count=0 WHERE integration_id=$1 AND delivery_id=$2`, delivery.IntegrationID, delivery.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE platform_audit_events`); err != nil {
		t.Fatal(err)
	}
	if failedAudit := request(http.MethodPost, replayPath, admin, `{"reason":"operator reviewed audit rollback"}`); failedAudit.Code != http.StatusServiceUnavailable {
		t.Fatalf("audit persistence failure status=%d body=%s", failedAudit.Code, failedAudit.Body.String())
	}
	if err := pool.QueryRow(ctx, `SELECT processing_status FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2`, delivery.IntegrationID, delivery.DeliveryID).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("replay state was not rolled back after audit failure: status=%q err=%v", status, err)
	}
}
