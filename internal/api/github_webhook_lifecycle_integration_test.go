//go:build integration

package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/awslabs/aurora-dsql-connectors/go/pgx/occretry"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	webhookstore "github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/testutil"
	"github.com/mo3789530/hako/internal/workloadjobs"
)

func TestWebhookLifecycleE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	seedWebhookLifecycleE2E(t, ctx, pool, now)

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
	secrets := lifecycleWebhookSecrets{
		"integration_lifecycle_a": {generation: 4, secret: "tenant-a-webhook-secret"},
		"integration_lifecycle_b": {generation: 7, secret: "tenant-b-webhook-secret"},
	}
	policy := transaction.DefaultPolicy()
	authorizer := githubregistry.CredentialAuthorizer{Pool: pool, Policy: policy}
	webhookHandler, err := githubwebhook.NewTenantHTTPHandler(secrets, authorizer, webhookstore.Inbox{Pool: pool, Policy: policy}, githubwebhook.DefaultMaxBodyBytes)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandlerWithGitHubApp(verifier, pool, nil, nil, WorkspaceCreateConfig{
		GitHubAppManagementEnabled: true, WebhookWorkloadTriggersEnabled: true, TenantGitHubWebhookHandler: webhookHandler,
	})
	ownerA := signTenantAPIToken(t, key, issuer, "lifecycle-owner-a", "openid hako/api")
	adminA := signTenantAPIToken(t, key, issuer, "lifecycle-admin-a", "openid hako/api")
	ownerB := signTenantAPIToken(t, key, issuer, "lifecycle-owner-b", "openid hako/api")
	operator := signAPITokenWithGroups(t, key, issuer, "lifecycle-operator", "openid hako/api", []string{"hako-admin"})
	apiCall := func(method, path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		for _, sensitive := range []string{
			secrets["integration_lifecycle_a"].secret, secrets["integration_lifecycle_b"].secret,
			ownerA, adminA, ownerB, operator, "raw-private-marker", "sha256=",
		} {
			if strings.Contains(res.Body.String(), sensitive) {
				t.Fatalf("API response for %s %s exposed sensitive material", method, path)
			}
		}
		return res
	}

	// Concurrent provider retries with one immutable Delivery ID create one inbox row.
	ownerDeliveryID := "a190f4c1-6da8-4c46-9b20-000000000001"
	ownerPayload := lifecyclePushPayload(8101, 8201, "tenant-a", strings.Repeat("a", 40))
	const deliveryAttempts = 6
	var deliveryWG sync.WaitGroup
	deliveryResponses := make(chan *httptest.ResponseRecorder, deliveryAttempts)
	for range deliveryAttempts {
		deliveryWG.Add(1)
		go func() {
			defer deliveryWG.Done()
			deliveryResponses <- sendLifecycleWebhook(handler, "integration_lifecycle_a", secrets["integration_lifecycle_a"].secret, "push", ownerDeliveryID, ownerPayload, false)
		}()
	}
	deliveryWG.Wait()
	close(deliveryResponses)
	newRows, duplicateRows := 0, 0
	for response := range deliveryResponses {
		if response.Code != http.StatusAccepted || strings.Contains(response.Body.String(), secrets["integration_lifecycle_a"].secret) || strings.Contains(response.Body.String(), "raw-private-marker") {
			t.Fatalf("signed webhook receipt HTTP %d leaked data or failed: %s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), `"duplicate":false`) {
			newRows++
		} else if strings.Contains(response.Body.String(), `"duplicate":true`) {
			duplicateRows++
		} else {
			t.Fatalf("webhook receipt omitted duplicate state: %s", response.Body.String())
		}
	}
	if newRows != 1 || duplicateRows != deliveryAttempts-1 {
		t.Fatalf("concurrent receipt results new=%d duplicate=%d", newRows, duplicateRows)
	}
	if conflict := sendLifecycleWebhook(handler, "integration_lifecycle_a", secrets["integration_lifecycle_a"].secret, "push", ownerDeliveryID,
		lifecyclePushPayload(8101, 8201, "tenant-a-changed", strings.Repeat("b", 40)), false); conflict.Code != http.StatusConflict {
		t.Fatalf("reused Delivery ID with changed body HTTP %d: %s", conflict.Code, conflict.Body.String())
	}
	if invalidMAC := sendLifecycleWebhook(handler, "integration_lifecycle_a", "wrong-secret", "push", "b190f4c1-6da8-4c46-9b20-000000000001", ownerPayload, false); invalidMAC.Code != http.StatusUnauthorized {
		t.Fatalf("tampered signature HTTP %d", invalidMAC.Code)
	}
	if unsupported := sendLifecycleWebhook(handler, "integration_lifecycle_a", secrets["integration_lifecycle_a"].secret, "issues", "c190f4c1-6da8-4c46-9b20-000000000001", ownerPayload, false); unsupported.Code != http.StatusBadRequest {
		t.Fatalf("unsupported GitHub event HTTP %d", unsupported.Code)
	}
	if unsupportedAction := sendLifecycleWebhook(handler, "integration_lifecycle_a", secrets["integration_lifecycle_a"].secret, "pull_request", "c290f4c1-6da8-4c46-9b20-000000000001", lifecyclePullRequestPayload(8101, 8201, "edited", "unsupported-action", strings.Repeat("2", 40)), false); unsupportedAction.Code != http.StatusUnauthorized {
		t.Fatalf("unsupported GitHub event action HTTP %d", unsupportedAction.Code)
	}
	if malformed := sendLifecycleWebhook(handler, "integration_lifecycle_a", secrets["integration_lifecycle_a"].secret, "push", "d190f4c1-6da8-4c46-9b20-000000000001", []byte(`{"ref":`), false); malformed.Code != http.StatusUnauthorized {
		t.Fatalf("signed malformed event HTTP %d", malformed.Code)
	}
	if tampered := sendLifecycleWebhook(handler, "integration_lifecycle_a", secrets["integration_lifecycle_a"].secret, "push", "d290f4c1-6da8-4c46-9b20-000000000001", ownerPayload, true); tampered.Code != http.StatusUnauthorized {
		t.Fatalf("tampered HMAC HTTP %d", tampered.Code)
	}

	// A delivery accepted while the Repository is disabled must not be revived by later opt-in.
	preOptInID := "e190f4c1-6da8-4c46-9b20-000000000001"
	if response := sendLifecycleWebhook(handler, "integration_lifecycle_a", secrets["integration_lifecycle_a"].secret, "push", preOptInID,
		lifecyclePushPayload(8101, 8202, "pre-opt-in", strings.Repeat("c", 40)), false); response.Code != http.StatusAccepted {
		t.Fatalf("pre-opt-in signed webhook HTTP %d: %s", response.Code, response.Body.String())
	}
	optIn := apiCall(http.MethodPut, "/v1/tenants/tenant_lifecycle_a/github/installations/8101/repositories/8202/factory", ownerA, `{"enabled":true}`)
	if optIn.Code != http.StatusOK {
		t.Fatalf("Owner opt-in HTTP %d: %s", optIn.Code, optIn.Body.String())
	}
	if response := sendLifecycleWebhook(handler, "integration_lifecycle_a", secrets["integration_lifecycle_a"].secret, "push", "f190f4c1-6da8-4c46-9b20-000000000001",
		lifecyclePushPayload(8101, 8203, "disabled", strings.Repeat("d", 40)), false); response.Code != http.StatusAccepted {
		t.Fatalf("disabled Repository signed webhook HTTP %d: %s", response.Code, response.Body.String())
	}
	pullRequestID := "0290f4c1-6da8-4c46-9b20-000000000001"
	pullRequestSHA := strings.Repeat("3", 40)
	if response := sendLifecycleWebhook(handler, "integration_lifecycle_a", secrets["integration_lifecycle_a"].secret, "pull_request", pullRequestID,
		lifecyclePullRequestPayload(8101, 8201, "opened", "review-branch", pullRequestSHA), false); response.Code != http.StatusAccepted {
		t.Fatalf("eligible pull-request webhook HTTP %d: %s", response.Code, response.Body.String())
	}
	missingRepositoryID := "0390f4c1-6da8-4c46-9b20-000000000001"
	if response := sendLifecycleWebhook(handler, "integration_lifecycle_a", secrets["integration_lifecycle_a"].secret, "push", missingRepositoryID,
		lifecyclePushPayload(8101, 8299, "deleted-repository", strings.Repeat("4", 40)), false); response.Code != http.StatusAccepted {
		t.Fatalf("deleted Repository webhook HTTP %d: %s", response.Code, response.Body.String())
	}
	workflowJobID := "0490f4c1-6da8-4c46-9b20-000000000001"
	if response := sendLifecycleWebhook(handler, "integration_lifecycle_a", secrets["integration_lifecycle_a"].secret, "workflow_job", workflowJobID,
		lifecycleWorkflowJobPayload(8101, 8201, strings.Repeat("6", 40)), false); response.Code != http.StatusAccepted {
		t.Fatalf("workflow_job ingress HTTP %d: %s", response.Code, response.Body.String())
	}

	// Both scoped replay paths are exercised; Tenant A cannot see Tenant B's failed delivery.
	platformDeliveryID := "1190f4c1-6da8-4c46-9b20-000000000001"
	adminDeliveryID := "1290f4c1-6da8-4c46-9b20-000000000001"
	platformPayload := lifecyclePushPayload(8102, 8204, "tenant-b", strings.Repeat("e", 40))
	if response := sendLifecycleWebhook(handler, "integration_lifecycle_b", secrets["integration_lifecycle_b"].secret, "push", platformDeliveryID, platformPayload, false); response.Code != http.StatusAccepted {
		t.Fatalf("Tenant B webhook HTTP %d: %s", response.Code, response.Body.String())
	}
	if response := sendLifecycleWebhook(handler, "integration_lifecycle_a", secrets["integration_lifecycle_a"].secret, "push", adminDeliveryID,
		lifecyclePushPayload(8101, 8201, "tenant-a-admin", strings.Repeat("5", 40)), false); response.Code != http.StatusAccepted {
		t.Fatalf("Tenant A Admin replay fixture HTTP %d: %s", response.Code, response.Body.String())
	}
	if _, err := pool.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='failed' WHERE delivery_id IN ($1,$2,$3)`, ownerDeliveryID, platformDeliveryID, adminDeliveryID); err != nil {
		t.Fatal(err)
	}
	ownerListPath := "/v1/tenants/tenant_lifecycle_a/github/apps/integration_lifecycle_a/deliveries?limit=20&offset=0"
	ownerList := apiCall(http.MethodGet, ownerListPath, ownerA, "")
	if ownerList.Code != http.StatusOK || strings.Contains(ownerList.Body.String(), platformDeliveryID) || strings.Contains(ownerList.Body.String(), "raw-private-marker") || strings.Contains(ownerList.Body.String(), "payload_json") {
		t.Fatalf("Tenant-scoped listing HTTP %d leaked data or other Tenant: %s", ownerList.Code, ownerList.Body.String())
	}
	adminList := apiCall(http.MethodGet, ownerListPath, adminA, "")
	if adminList.Code != http.StatusOK || !strings.Contains(adminList.Body.String(), adminDeliveryID) || strings.Contains(adminList.Body.String(), platformDeliveryID) {
		t.Fatalf("Tenant Admin listing scope HTTP %d: %s", adminList.Code, adminList.Body.String())
	}
	platformList := apiCall(http.MethodGet, "/v1/admin/github/webhook-deliveries?limit=20&offset=0", operator, "")
	if platformList.Code != http.StatusOK || !strings.Contains(platformList.Body.String(), ownerDeliveryID) || !strings.Contains(platformList.Body.String(), platformDeliveryID) || !strings.Contains(platformList.Body.String(), adminDeliveryID) || strings.Contains(platformList.Body.String(), "raw-private-marker") {
		t.Fatalf("platform listing HTTP %d failed redaction/scope: %s", platformList.Code, platformList.Body.String())
	}
	ownerBList := apiCall(http.MethodGet, "/v1/tenants/tenant_lifecycle_b/github/apps/integration_lifecycle_b/deliveries?limit=20&offset=0", ownerB, "")
	if ownerBList.Code != http.StatusOK || !strings.Contains(ownerBList.Body.String(), platformDeliveryID) || strings.Contains(ownerBList.Body.String(), ownerDeliveryID) {
		t.Fatalf("Tenant B listing crossed Tenant boundary: HTTP %d %s", ownerBList.Code, ownerBList.Body.String())
	}
	ownerReplayPath := "/v1/tenants/tenant_lifecycle_a/github/apps/integration_lifecycle_a/deliveries/" + ownerDeliveryID + "/replay"
	platformReplayPath := "/v1/admin/github/webhook-deliveries/integration_lifecycle_b/" + platformDeliveryID + "/replay"
	concurrentReplays(t, func() *httptest.ResponseRecorder {
		return apiCall(http.MethodPost, ownerReplayPath, ownerA, `{"reason":"local lifecycle E2E owner retry"}`)
	})
	adminReplayPath := "/v1/tenants/tenant_lifecycle_a/github/apps/integration_lifecycle_a/deliveries/" + adminDeliveryID + "/replay"
	adminReplay := apiCall(http.MethodPost, adminReplayPath, adminA, `{"reason":"local lifecycle E2E Tenant Admin retry"}`)
	if adminReplay.Code != http.StatusAccepted {
		t.Fatalf("Tenant Admin replay HTTP %d: %s", adminReplay.Code, adminReplay.Body.String())
	}
	concurrentReplays(t, func() *httptest.ResponseRecorder {
		return apiCall(http.MethodPost, platformReplayPath, operator, `{"reason":"local lifecycle E2E operator retry"}`)
	})

	// Exercise processor contention and its durable terminal acknowledgement.
	var processorWG sync.WaitGroup
	processorErrors := make(chan error, 2)
	retryPool := &webhookRetryBatchPool{inner: pool}
	for range 2 {
		processorWG.Add(1)
		go func() {
			defer processorWG.Done()
			_, err := webhookstore.ProcessBatchWithRepositoryTriggers(ctx, retryPool, policy, 20, time.Now().UTC(), workloadjobs.CreateWebhookRepositoryRun)
			processorErrors <- err
		}()
	}
	processorWG.Wait()
	close(processorErrors)
	for err := range processorErrors {
		if err != nil {
			t.Fatalf("concurrent webhook processor: %v", err)
		}
	}
	retryPool.mu.Lock()
	beginAttempts, injectedRetries := retryPool.beginAttempts, retryPool.injectedRetries
	retryPool.mu.Unlock()
	if beginAttempts < 3 || injectedRetries != 1 {
		t.Fatalf("processor retry was not exercised: Begin attempts=%d injected conflicts=%d", beginAttempts, injectedRetries)
	}
	assertWebhookDeliveryStatus(t, ctx, pool, "integration_lifecycle_a", ownerDeliveryID, "processed")
	assertWebhookDeliveryStatus(t, ctx, pool, "integration_lifecycle_b", platformDeliveryID, "processed")
	assertWebhookDeliveryStatus(t, ctx, pool, "integration_lifecycle_a", preOptInID, "processed")
	assertWebhookDeliveryStatus(t, ctx, pool, "integration_lifecycle_a", "f190f4c1-6da8-4c46-9b20-000000000001", "processed")
	assertWebhookDeliveryStatus(t, ctx, pool, "integration_lifecycle_a", missingRepositoryID, "failed")
	assertWebhookDeliveryStatus(t, ctx, pool, "integration_lifecycle_a", workflowJobID, "received")
	assertWebhookDeliveryStatus(t, ctx, pool, "integration_lifecycle_a", "d190f4c1-6da8-4c46-9b20-000000000001", "")
	for _, rejectedID := range []string{"c190f4c1-6da8-4c46-9b20-000000000001", "c290f4c1-6da8-4c46-9b20-000000000001", "d290f4c1-6da8-4c46-9b20-000000000001"} {
		assertWebhookDeliveryStatus(t, ctx, pool, "integration_lifecycle_a", rejectedID, "")
	}

	ownerRun := assertWebhookRun(t, ctx, pool, "tenant_lifecycle_a", "integration_lifecycle_a", ownerDeliveryID, 8101, 8201, strings.Repeat("a", 40), "refs/heads/main", 4)
	if !strings.HasPrefix(ownerRun.CorrelationID, "req_") || ownerRun.CorrelationID == "req_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
		t.Fatalf("Webhook Workload Run correlation must come from the server, not the caller: %q", ownerRun.CorrelationID)
	}
	var deliveryCorrelationID string
	if err := pool.QueryRow(ctx, `SELECT correlation_id FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2`, "integration_lifecycle_a", ownerDeliveryID).Scan(&deliveryCorrelationID); err != nil || ownerRun.CorrelationID != deliveryCorrelationID {
		t.Fatalf("Webhook Run correlation %q must match persisted delivery correlation %q (error=%v)", ownerRun.CorrelationID, deliveryCorrelationID, err)
	}
	platformRun := assertWebhookRun(t, ctx, pool, "tenant_lifecycle_b", "integration_lifecycle_b", platformDeliveryID, 8102, 8204, strings.Repeat("e", 40), "refs/heads/main", 7)
	_ = assertWebhookRun(t, ctx, pool, "tenant_lifecycle_a", "integration_lifecycle_a", adminDeliveryID, 8101, 8201, strings.Repeat("5", 40), "refs/heads/main", 4)
	_ = assertWebhookRun(t, ctx, pool, "tenant_lifecycle_a", "integration_lifecycle_a", pullRequestID, 8101, 8201, pullRequestSHA, "review-branch", 4)
	for _, id := range []string{preOptInID, "f190f4c1-6da8-4c46-9b20-000000000001", missingRepositoryID, workflowJobID} {
		key := workloads.WebhookDeliveryIdempotencyKey("integration_lifecycle_a", id)
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_runs WHERE idempotency_key=$1`, key).Scan(&count); err != nil || count != 0 {
			t.Fatalf("opt-in boundary Delivery ID %s created %d Runs (err=%v)", id, count, err)
		}
	}

	// The normalized inbox is the sole source of Run identity; terminal Run state
	// synchronously updates durable Check metadata and enqueues a new Outbox revision.
	terminalRun := transitionWebhookRunToSuccess(t, ctx, pool, ownerRun, now.Add(3*time.Second))
	if terminalRun.State != domain.WorkloadSucceeded {
		t.Fatalf("terminal Workload Run state=%s", terminalRun.State)
	}
	var checkStatus, conclusion string
	var revision int64
	if err := pool.QueryRow(ctx, `SELECT status,conclusion,revision FROM workload_github_checks WHERE tenant_id=$1 AND workload_run_id=$2`, ownerRun.TenantID, ownerRun.ID).Scan(&checkStatus, &conclusion, &revision); err != nil {
		t.Fatal(err)
	}
	if checkStatus != "completed" || conclusion != "success" || revision < 5 {
		t.Fatalf("terminal Check status=%s conclusion=%s revision=%d", checkStatus, conclusion, revision)
	}
	var runCount, checkCount, outboxCount int
	keyA := workloads.WebhookDeliveryIdempotencyKey("integration_lifecycle_a", ownerDeliveryID)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_runs WHERE idempotency_key=$1`, keyA).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_github_checks WHERE tenant_id=$1 AND workload_run_id=$2`, ownerRun.TenantID, ownerRun.ID).Scan(&checkCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='workload_run.github_check_requested'`, ownerRun.ID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if runCount != 1 || checkCount != 1 || outboxCount != 5 {
		t.Fatalf("duplicate/concurrent processing created runs=%d checks=%d Check outbox=%d", runCount, checkCount, outboxCount)
	}
	var correlatedCheckOutboxCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='workload_run.github_check_requested'
		AND payload_json::jsonb->>'correlation_id'=$2`, ownerRun.ID, ownerRun.CorrelationID).Scan(&correlatedCheckOutboxCount); err != nil || correlatedCheckOutboxCount != outboxCount {
		t.Fatalf("all Workload Check Outbox revisions must retain webhook correlation: matched=%d total=%d error=%v", correlatedCheckOutboxCount, outboxCount, err)
	}
	_ = platformRun

	// Expiration/purge is durable and only erases raw JSON. Normalized identity
	// and payload hash survive, while both Tenant and platform replay are denied.
	expiredID := "2190f4c1-6da8-4c46-9b20-000000000001"
	if response := sendLifecycleWebhook(handler, "integration_lifecycle_a", secrets["integration_lifecycle_a"].secret, "push", expiredID,
		lifecyclePushPayload(8101, 8201, "expired", strings.Repeat("f", 40)), false); response.Code != http.StatusAccepted {
		t.Fatalf("expired fixture receipt HTTP %d", response.Code)
	}
	if _, err := pool.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='failed',payload_expires_at=$3 WHERE integration_id=$1 AND delivery_id=$2`, "integration_lifecycle_a", expiredID, now.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	purged, err := webhookstore.PurgeExpiredRawPayloads(ctx, pool, policy, 100, now)
	if err != nil || purged != 1 {
		t.Fatalf("raw payload purge count=%d err=%v", purged, err)
	}
	var rawPayload, normalized string
	var payloadHash string
	var purgedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT payload_json,normalized_event_json,payload_sha256,payload_purged_at FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2`, "integration_lifecycle_a", expiredID).Scan(&rawPayload, &normalized, &payloadHash, &purgedAt); err != nil {
		t.Fatal(err)
	}
	if rawPayload != "" || normalized == "" || payloadHash == "" || purgedAt == nil || !strings.Contains(normalized, strings.Repeat("f", 40)) {
		t.Fatalf("retention erased normalized identity or retained raw body: raw=%q normalized=%q hash=%q purged=%v", rawPayload, normalized, payloadHash, purgedAt)
	}
	if denied := apiCall(http.MethodPost, "/v1/admin/github/webhook-deliveries/integration_lifecycle_a/"+expiredID+"/replay", operator, `{"reason":"expired payload must not replay"}`); denied.Code != http.StatusConflict {
		t.Fatalf("expired platform replay HTTP %d: %s", denied.Code, denied.Body.String())
	}
	if denied := apiCall(http.MethodPost, "/v1/tenants/tenant_lifecycle_a/github/apps/integration_lifecycle_a/deliveries/"+expiredID+"/replay", ownerA, `{"reason":"expired payload must not replay"}`); denied.Code != http.StatusConflict {
		t.Fatalf("expired Tenant replay HTTP %d: %s", denied.Code, denied.Body.String())
	}

	// Current authorization is revalidated at replay and processing time.
	staleID := "3190f4c1-6da8-4c46-9b20-000000000001"
	if response := sendLifecycleWebhook(handler, "integration_lifecycle_a", secrets["integration_lifecycle_a"].secret, "push", staleID,
		lifecyclePushPayload(8101, 8201, "stale-binding", strings.Repeat("1", 40)), false); response.Code != http.StatusAccepted {
		t.Fatalf("stale fixture receipt HTTP %d", response.Code)
	}
	if _, err := pool.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='failed' WHERE delivery_id=$1`, staleID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_installations SET suspended_at=$3 WHERE tenant_id=$1 AND installation_id=$2`, "tenant_lifecycle_a", int64(8101), now); err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []struct{ token, path string }{
		{ownerA, "/v1/tenants/tenant_lifecycle_a/github/apps/integration_lifecycle_a/deliveries/" + staleID + "/replay"},
		{operator, "/v1/admin/github/webhook-deliveries/integration_lifecycle_a/" + staleID + "/replay"},
	} {
		if denied := apiCall(http.MethodPost, attempt.path, attempt.token, `{"reason":"installation is suspended"}`); denied.Code != http.StatusConflict {
			t.Fatalf("stale Installation replay HTTP %d: %s", denied.Code, denied.Body.String())
		}
	}
	if _, err := webhookstore.ProcessBatchWithRepositoryTriggers(ctx, pool, policy, 20, now.Add(4*time.Second), workloadjobs.CreateWebhookRepositoryRun); err != nil {
		t.Fatalf("processor current-binding authorization: %v", err)
	}
	assertWebhookDeliveryStatus(t, ctx, pool, "integration_lifecycle_a", staleID, "failed")
	keyStale := workloads.WebhookDeliveryIdempotencyKey("integration_lifecycle_a", staleID)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_runs WHERE idempotency_key=$1`, keyStale).Scan(&runCount); err != nil || runCount != 0 {
		t.Fatalf("suspended Installation created %d Workload Runs (err=%v)", runCount, err)
	}

	// Verify audit separation and per-Tenant list visibility after replay.
	var tenantAudits, platformAudits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id='tenant_lifecycle_a' AND actor_user_id='usr_lifecycle_owner_a' AND action='github.webhook.replay_requested' AND target_id=$1`, ownerDeliveryID).Scan(&tenantAudits); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM platform_audit_events WHERE actor_user_id='usr_lifecycle_operator' AND action='github.webhook.delivery.replay_requested' AND target_id=$1`, "integration_lifecycle_b/"+platformDeliveryID).Scan(&platformAudits); err != nil {
		t.Fatal(err)
	}
	var adminAudits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id='tenant_lifecycle_a' AND actor_user_id='usr_lifecycle_admin_a' AND action='github.webhook.replay_requested' AND target_id=$1`, adminDeliveryID).Scan(&adminAudits); err != nil {
		t.Fatal(err)
	}
	if tenantAudits != 1 || adminAudits != 1 || platformAudits != 1 {
		t.Fatalf("separate replay audit counts Owner=%d Tenant Admin=%d platform=%d", tenantAudits, adminAudits, platformAudits)
	}
	ownerList = apiCall(http.MethodGet, ownerListPath, ownerA, "")
	if strings.Contains(ownerList.Body.String(), platformDeliveryID) {
		t.Fatalf("Tenant failed-delivery listing crossed Tenant boundary: %s", ownerList.Body.String())
	}
}

type lifecycleWebhookSecret struct {
	generation int64
	secret     string
}

type lifecycleWebhookSecrets map[string]lifecycleWebhookSecret

func (s lifecycleWebhookSecrets) ResolveWebhookCredential(_ context.Context, integrationID string) (githubwebhook.WebhookCredential, error) {
	credential, ok := s[integrationID]
	if !ok {
		return githubwebhook.WebhookCredential{}, githubregistry.ErrAppBindingDenied
	}
	return githubwebhook.WebhookCredential{IntegrationID: integrationID, Generation: credential.generation, CurrentSecret: []byte(credential.secret)}, nil
}

func seedWebhookLifecycleE2E(t *testing.T, ctx context.Context, pool *pgxpool.Pool, now time.Time) {
	t.Helper()
	queries := []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_lifecycle_a','Lifecycle A',$1),('tenant_lifecycle_b','Lifecycle B',$1)`,
		`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_lifecycle_owner_a','lifecycle-owner-a','',$1),('usr_lifecycle_admin_a','lifecycle-admin-a','',$1),('usr_lifecycle_owner_b','lifecycle-owner-b','',$1),('usr_lifecycle_operator','lifecycle-operator','',$1)`,
		`INSERT INTO tenant_members(tenant_id,user_id,role,joined_at) VALUES ('tenant_lifecycle_a','usr_lifecycle_owner_a','owner',$1),('tenant_lifecycle_a','usr_lifecycle_admin_a','admin',$1),('tenant_lifecycle_b','usr_lifecycle_owner_b','owner',$1)`,
		`INSERT INTO github_app_registrations(id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_lifecycle_a',8811,'tenant_owned','tenant_lifecycle_a','lifecycle-a','client-a',$1),('registration_lifecycle_b',8821,'tenant_owned','tenant_lifecycle_b','lifecycle-b','client-b',$1)`,
		`INSERT INTO github_credential_bindings(id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at) VALUES ('binding_lifecycle_a','tenant_lifecycle_a',8811,'approved','private-a','oauth-a','webhook-a',$1),('binding_lifecycle_b','tenant_lifecycle_b',8821,'approved','private-b','oauth-b','webhook-b',$1)`,
		`INSERT INTO tenant_github_integrations(id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_lifecycle_a','tenant_lifecycle_a','registration_lifecycle_a',8811,'binding_lifecycle_a','active',2,4,'lifecycle-a-key','` + strings.Repeat("a", 64) + `','usr_lifecycle_owner_a',$1,$1),('integration_lifecycle_b','tenant_lifecycle_b','registration_lifecycle_b',8821,'binding_lifecycle_b','active',3,7,'lifecycle-b-key','` + strings.Repeat("b", 64) + `','usr_lifecycle_owner_b',$1,$1)`,
		`INSERT INTO tenant_github_installations(tenant_id,installation_id,github_app_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_lifecycle_a',8101,8811,'tenant-a','active','usr_lifecycle_owner_a',$1,$1),('tenant_lifecycle_b',8102,8821,'tenant-b','active','usr_lifecycle_owner_b',$1,$1)`,
		`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (8101,'tenant_lifecycle_a',$1),(8102,'tenant_lifecycle_b',$1)`,
		`INSERT INTO tenant_github_app_installation_bindings(tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_lifecycle_a','integration_lifecycle_a',8101,$1),('tenant_lifecycle_b','integration_lifecycle_b',8102,$1)`,
		`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,factory_enabled,factory_enabled_at,synchronized_at) VALUES
		 ('tenant_lifecycle_a',8101,8201,'tenant-a','primary','main',true,$1,$1),
		 ('tenant_lifecycle_a',8101,8202,'tenant-a','pre-opt-in','main',false,NULL,$1),
		 ('tenant_lifecycle_a',8101,8203,'tenant-a','disabled','main',false,NULL,$1),
		 ('tenant_lifecycle_b',8102,8204,'tenant-b','platform-replay','main',true,$1,$1)`,
	}
	for _, query := range queries {
		if _, err := pool.Exec(ctx, query, now); err != nil {
			t.Fatalf("seed webhook lifecycle fixture: %v", err)
		}
	}
}

func lifecyclePushPayload(installationID, repositoryID int64, rawMarker, sha string) []byte {
	return []byte(fmt.Sprintf(`{"ref":"refs/heads/main","after":%q,"installation":{"id":%d,"app_id":%d},"repository":{"id":%d,"name":"repo-%d","owner":{"login":"acme"},"private_marker":%q},"sender":{"login":"octocat"}}`,
		sha, installationID, map[int64]int64{8101: 8811, 8102: 8821}[installationID], repositoryID, repositoryID, "raw-private-marker-"+rawMarker))
}

func sendLifecycleWebhook(handler http.Handler, integrationID, secret, event, deliveryID string, payload []byte, tamper bool) *httptest.ResponseRecorder {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	signature := hex.EncodeToString(mac.Sum(nil))
	if tamper {
		signature = strings.Repeat("0", len(signature))
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/integrations/github/apps/"+integrationID+"/webhook", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	req.Header.Set("X-Hako-Request-ID", "req_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	req.Header.Set("X-Hub-Signature-256", "sha256="+signature)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	return res
}

func concurrentReplays(t *testing.T, call func() *httptest.ResponseRecorder) {
	t.Helper()
	var group sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		group.Add(1)
		go func() { defer group.Done(); responses <- call() }()
	}
	group.Wait()
	close(responses)
	accepted, conflict := 0, 0
	for response := range responses {
		if strings.Contains(response.Body.String(), "raw-private-marker") || strings.Contains(response.Body.String(), "webhook-secret") {
			t.Fatalf("replay response exposed webhook data: %s", response.Body.String())
		}
		switch response.Code {
		case http.StatusAccepted:
			accepted++
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("concurrent replay HTTP %d: %s", response.Code, response.Body.String())
		}
	}
	if accepted != 1 || conflict != 1 {
		t.Fatalf("concurrent replay accepted=%d conflict=%d", accepted, conflict)
	}
}

func assertWebhookDeliveryStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, integrationID, deliveryID, want string) {
	t.Helper()
	var got string
	err := pool.QueryRow(ctx, `SELECT processing_status FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2`, integrationID, deliveryID).Scan(&got)
	if want == "" {
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("unexpected durable delivery %s/%s with status %s", integrationID, deliveryID, got)
		}
		return
	}
	if err != nil || got != want {
		t.Fatalf("delivery %s/%s status=%q want=%q err=%v", integrationID, deliveryID, got, want, err)
	}
}

func assertWebhookRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID, integrationID, deliveryID string,
	installationID, repositoryID int64, sha, ref string, generation int64) domain.WorkloadRun {
	t.Helper()
	key := workloads.WebhookDeliveryIdempotencyKey(integrationID, deliveryID)
	var run domain.WorkloadRun
	var trust string
	err := pool.QueryRow(ctx, `SELECT id,tenant_id,kind,github_installation_id,github_repository_id,github_integration_id,github_credential_generation,commit_sha,ref,runtime_class,trust_level,state,idempotency_key,COALESCE(correlation_id,'') FROM workload_runs WHERE idempotency_key=$1`, key).
		Scan(&run.ID, &run.TenantID, &run.Kind, &run.GitHubInstallation, &run.GitHubRepository, &run.GitHubIntegrationID, &run.GitHubCredentialGeneration, &run.CommitSHA, &run.Ref, &run.RuntimeClass, &trust, &run.State, &run.IdempotencyKey, &run.CorrelationID)
	if err != nil {
		t.Fatalf("load webhook Run from verified inbox: %v", err)
	}
	if run.TenantID != domain.TenantID(tenantID) || run.Kind != domain.WorkloadJob || run.GitHubIntegrationID != integrationID || run.GitHubCredentialGeneration != generation || run.GitHubInstallation != installationID ||
		run.GitHubRepository != repositoryID || run.CommitSHA != sha || run.Ref != ref || run.RuntimeClass != "standard" || trust != string(domain.WorkloadUntrusted) || run.State != domain.WorkloadPending || run.IdempotencyKey != key {
		t.Fatalf("Run identity/policy for %s did not come from inbox: %+v trust=%s", deliveryID, run, trust)
	}
	var checkCount, outboxCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_github_checks WHERE tenant_id=$1 AND workload_run_id=$2`, tenantID, run.ID).Scan(&checkCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='workload_run.github_check_requested'`, run.ID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if checkCount != 1 || outboxCount != 1 {
		t.Fatalf("webhook Run %s has checks=%d outbox=%d", run.ID, checkCount, outboxCount)
	}
	return run
}

func transitionWebhookRunToSuccess(t *testing.T, ctx context.Context, pool *pgxpool.Pool, run domain.WorkloadRun, now time.Time) domain.WorkloadRun {
	t.Helper()
	var terminal domain.WorkloadRun
	var err error
	for _, state := range []domain.WorkloadRunState{domain.WorkloadQueued, domain.WorkloadProvisioning, domain.WorkloadRunning, domain.WorkloadSucceeded} {
		terminal, err = transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
			current, err := workloads.Get(ctx, tx, run.TenantID, run.ID)
			if err != nil {
				return domain.WorkloadRun{}, err
			}
			return workloads.Transition(ctx, tx, workloads.TransitionInput{TenantID: run.TenantID, RunID: run.ID, ExpectedRevision: current.Revision, NextState: state, OccurredAt: now})
		})
		if err != nil {
			t.Fatalf("transition webhook Run to %s: %v", state, err)
		}
		now = now.Add(time.Second)
	}
	return terminal
}

func lifecyclePullRequestPayload(installationID, repositoryID int64, action, branch, sha string) []byte {
	return []byte(fmt.Sprintf(`{"action":%q,"installation":{"id":%d,"app_id":%d},"repository":{"id":%d,"name":"repo-%d","owner":{"login":"acme"}},"sender":{"login":"octocat"},"pull_request":{"number":42,"state":"open","head":{"ref":%q,"sha":%q},"base":{"ref":"main"}}}`,
		action, installationID, map[int64]int64{8101: 8811, 8102: 8821}[installationID], repositoryID, repositoryID, branch, sha))
}

func lifecycleWorkflowJobPayload(installationID, repositoryID int64, sha string) []byte {
	return []byte(fmt.Sprintf(`{"action":"queued","installation":{"id":%d,"app_id":8811},"repository":{"id":%d,"name":"repo-%d","owner":{"login":"acme"}},"sender":{"login":"octocat"},"workflow_job":{"id":91,"run_id":92,"name":"verify","head_branch":"main","head_sha":%q,"status":"queued"}}`, installationID, repositoryID, repositoryID, sha))
}

type webhookRetryBatchPool struct {
	inner           *pgxpool.Pool
	mu              sync.Mutex
	beginAttempts   int
	injectedRetries int
}

func (p *webhookRetryBatchPool) Begin(ctx context.Context) (pgx.Tx, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.beginAttempts++
	if p.injectedRetries == 0 {
		p.injectedRetries++
		return nil, &pgconn.PgError{Code: occretry.ErrorCodeMutation, Message: "synthetic transient serialization conflict"}
	}
	return p.inner.Begin(ctx)
}

func (p *webhookRetryBatchPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return p.inner.Query(ctx, sql, args...)
}
