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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/quotas"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/testutil"
	"github.com/mo3789530/hako/internal/workloadartifactaccess"
)

func TestWorkloadRunRoutesEnforceTenantAndRequesterVisibility(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	seeds := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants (id, name, created_at) VALUES ($1, $2, $3)`, []any{"tenant_runs_a", "Runs A", now}},
		{`INSERT INTO tenants (id, name, created_at) VALUES ($1, $2, $3)`, []any{"tenant_runs_b", "Runs B", now}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ($1, $2, '', $3)`, []any{"usr_runs_member", "runs-member", now}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ($1, $2, '', $3)`, []any{"usr_runs_other", "runs-other", now}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ($1, $2, '', $3)`, []any{"usr_runs_owner", "runs-owner", now}},
		{`INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ($1, $2, $3, $4)`, []any{"tenant_runs_a", "usr_runs_member", "member", now}},
		{`INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ($1, $2, $3, $4)`, []any{"tenant_runs_a", "usr_runs_owner", "owner", now}},
		{`INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ($1, $2, $3, $4)`, []any{"tenant_runs_b", "usr_runs_other", "owner", now}},
		{`INSERT INTO tenant_github_installations (tenant_id, installation_id, account_login, status, requested_by, requested_at, updated_at) VALUES ($1, $2, $3, 'active', $4, $5, $5)`, []any{"tenant_runs_a", 9301, "acme", "usr_runs_member", now}},
		{`INSERT INTO github_app_installation_bindings (installation_id, tenant_id, bound_at) VALUES ($1, $2, $3)`, []any{9301, "tenant_runs_a", now}},
		{`INSERT INTO tenant_github_repositories (tenant_id, installation_id, github_repository_id, owner_login, repository_name, default_branch, synchronized_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`, []any{"tenant_runs_a", 9301, 9302, "acme", "api", "main", now}},
	}
	for _, seed := range seeds {
		if _, err := pool.Exec(ctx, seed.query, seed.args...); err != nil {
			t.Fatalf("seed API data: %v", err)
		}
	}
	createRun := func(id string, requester domain.UserID) {
		t.Helper()
		run := domain.WorkloadRun{ID: domain.WorkloadRunID(id), TenantID: "tenant_runs_a", Kind: domain.WorkloadJob,
			RequestedBy: requester, IdempotencyKey: "idempotency-" + id, RuntimeClass: "standard",
			TrustLevel: domain.WorkloadUntrusted, TimeoutSeconds: 600, TimeoutAt: now.Add(10 * time.Minute), CreatedAt: now}
		if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
			stored, _, err := workloads.CreateOrGet(ctx, tx, run)
			return stored, err
		}); err != nil {
			t.Fatalf("create Workload Run: %v", err)
		}
	}
	createRun("run_owned_member", "usr_runs_member")
	createRun("run_owned_owner", "usr_runs_owner")
	for _, runID := range []domain.WorkloadRunID{"run_owned_member", "run_owned_owner"} {
		if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
			current, err := workloads.Get(ctx, tx, "tenant_runs_a", runID)
			if err != nil {
				return domain.WorkloadRun{}, err
			}
			return workloads.Transition(ctx, tx, workloads.TransitionInput{TenantID: current.TenantID, RunID: current.ID,
				ExpectedRevision: current.Revision, NextState: domain.WorkloadProvisioning, OccurredAt: now.Add(time.Second)})
		}); err != nil {
			t.Fatalf("move seeded Run out of queue: %v", err)
		}
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
		return quotas.SetWithQueueAndPreview(ctx, tx, "tenant_runs_a", "usr_runs_owner", quotas.DefaultWorkspaces,
			quotas.DefaultConcurrentWorkloadRuns, 3, quotas.DefaultPreviewRuns, quotas.DefaultPreviewLifetimeSeconds,
			quotas.DefaultArtifactBytes, quotas.DefaultWorkspaceServiceMemoryBytes, quotas.DefaultWorkspaceServiceMilliCPU, now.Add(2*time.Second))
	}); err != nil {
		t.Fatalf("configure Tenant queue backlog quota: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workload_artifacts
		(id, tenant_id, workload_run_id, kind, storage_ref, size_bytes, sha256, redacted, expires_at, created_at)
		VALUES ('artifact_run_detail', 'tenant_runs_a', 'run_owned_member', 'logs',
		's3://private-artifacts/workloads/secret-key-path', 12, $1, true, $2, $3)`,
		strings.Repeat("a", 64), now.Add(time.Hour), now); err != nil {
		t.Fatalf("seed Workload artifact: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES ('rp_runs_a','aws','ap-northeast-1','["microvm"]')`); err != nil {
		t.Fatalf("seed Workload Resource Plane: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workload_runs SET resource_plane_id='rp_runs_a' WHERE id='run_owned_member'`); err != nil {
		t.Fatalf("pin Workload Run Resource Plane: %v", err)
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "tenant-api-key",
			"n": base64.RawURLEncoding.EncodeToString(privateKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
		}}})
	}))
	defer keys.Close()
	issuer := keys.URL + "/pool"
	verifier, err := auth.NewCognitoVerifier(auth.CognitoVerifierConfig{Issuer: issuer, ClientID: "hako-cli"})
	if err != nil {
		t.Fatal(err)
	}
	artifactKey := []byte(strings.Repeat("k", 32))
	keyResolver := func(_ context.Context, planeID domain.ResourcePlaneID) ([]byte, error) {
		if planeID != "rp_runs_a" {
			return nil, fmt.Errorf("unknown plane")
		}
		return append([]byte(nil), artifactKey...), nil
	}
	handler := NewHandler(verifier, pool, WorkspaceCreateConfig{WorkloadRunsEnabled: true,
		ResourcePlaneWorkspaceSessionKeyResolver: keyResolver})
	request := func(subject, method, path string) *httptest.ResponseRecorder {
		t.Helper()
		token := signTenantAPIToken(t, privateKey, issuer, subject, "openid hako/api")
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder
	}
	createRequest := func(subject, key, body string) *httptest.ResponseRecorder {
		t.Helper()
		token := signTenantAPIToken(t, privateKey, issuer, subject, "openid hako/api")
		req := httptest.NewRequest(http.MethodPost, "/v1/tenants/tenant_runs_a/workload-runs", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		req.Header.Set(requestIDHeader, "req_11111111-1111-4111-8111-111111111111")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder
	}

	createBody := `{"github_installation_id":9301,"github_repository_id":9302,"commit_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","ref":"refs/heads/main","timeout_seconds":600}`
	created := createRequest("runs-member", "create-run-key", createBody)
	if created.Code != http.StatusAccepted || !strings.Contains(created.Body.String(), `"state":"pending"`) {
		t.Fatalf("create Workload Run = HTTP %d %s", created.Code, created.Body.String())
	}
	var createdRun domain.WorkloadRun
	if err := json.Unmarshal(created.Body.Bytes(), &createdRun); err != nil || createdRun.ID == "" || createdRun.TenantID != "tenant_runs_a" {
		t.Fatalf("decode created Workload Run: run=%+v error=%v", createdRun, err)
	}
	requestCorrelationID := created.Header().Get(requestIDHeader)
	if requestCorrelationID == "" || requestCorrelationID == "req_11111111-1111-4111-8111-111111111111" ||
		createdRun.CorrelationID != requestCorrelationID {
		t.Fatalf("Workload Run correlation ID=%q response header=%q", createdRun.CorrelationID, requestCorrelationID)
	}
	pendingRuns := []domain.WorkloadRun{createdRun}
	replayedCreate := createRequest("runs-member", "create-run-key", createBody)
	if replayedCreate.Code != http.StatusAccepted || !strings.Contains(replayedCreate.Body.String(), `"id":"`+string(createdRun.ID)+`"`) ||
		!strings.Contains(replayedCreate.Body.String(), `"correlation_id":"`+requestCorrelationID+`"`) {
		t.Fatalf("idempotent create did not return existing run: HTTP %d %s", replayedCreate.Code, replayedCreate.Body.String())
	}
	if conflict := createRequest("runs-member", "create-run-key", strings.Replace(createBody, "refs/heads/main", "refs/heads/other", 1)); conflict.Code != http.StatusConflict {
		t.Fatalf("create idempotency conflict returned HTTP %d %s", conflict.Code, conflict.Body.String())
	}
	if inactive := createRequest("runs-member", "inactive-run-key", strings.Replace(createBody, "9302", "9399", 1)); inactive.Code != http.StatusNotFound {
		t.Fatalf("unregistered repository returned HTTP %d %s", inactive.Code, inactive.Body.String())
	}
	var scheduledEvents, createAudits int
	var outboxPayload string
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'workload_run.schedule_requested'`, createdRun.ID).Scan(&scheduledEvents); err != nil || scheduledEvents != 1 {
		t.Fatalf("scheduled outbox events=%d error=%v", scheduledEvents, err)
	}
	if err := pool.QueryRow(ctx, `SELECT payload_json::text FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'workload_run.schedule_requested'`, createdRun.ID).Scan(&outboxPayload); err != nil {
		t.Fatalf("read Workload Run scheduling payload: %v", err)
	}
	var scheduled struct {
		CorrelationID string `json:"correlation_id"`
	}
	if err := json.Unmarshal([]byte(outboxPayload), &scheduled); err != nil || scheduled.CorrelationID != requestCorrelationID {
		t.Fatalf("scheduled correlation ID=%q, error=%v, payload=%s", scheduled.CorrelationID, err, outboxPayload)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE tenant_id = $1 AND actor_user_id = $2 AND action = 'workload_run.create' AND target_id = $3`, "tenant_runs_a", "usr_runs_member", createdRun.ID).Scan(&createAudits); err != nil || createAudits != 1 {
		t.Fatalf("create audit count=%d error=%v", createAudits, err)
	}
	for index := 1; index <= 2; index++ {
		response := createRequest("runs-member", fmt.Sprintf("additional-run-key-%d", index), createBody)
		if response.Code != http.StatusAccepted {
			t.Fatalf("Workload Run %d below quota returned HTTP %d %s", index, response.Code, response.Body.String())
		}
		var run domain.WorkloadRun
		if err := json.Unmarshal(response.Body.Bytes(), &run); err != nil {
			t.Fatalf("decode additional Workload Run: %v", err)
		}
		pendingRuns = append(pendingRuns, run)
	}
	queueQuotaExceeded := createRequest("runs-member", "over-queue-quota-run-key", createBody)
	var quotaError errorResponse
	if queueQuotaExceeded.Code != http.StatusConflict || json.Unmarshal(queueQuotaExceeded.Body.Bytes(), &quotaError) != nil || quotaError.Error.Code != "workload_queue_quota_exceeded" {
		t.Fatalf("queue backlog limit should return a stable quota conflict: HTTP %d %s", queueQuotaExceeded.Code, queueQuotaExceeded.Body.String())
	}
	for _, run := range pendingRuns {
		if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
			current, err := workloads.Get(ctx, tx, run.TenantID, run.ID)
			if err != nil {
				return domain.WorkloadRun{}, err
			}
			return workloads.Transition(ctx, tx, workloads.TransitionInput{TenantID: current.TenantID, RunID: current.ID,
				ExpectedRevision: current.Revision, NextState: domain.WorkloadProvisioning, OccurredAt: time.Now().UTC()})
		}); err != nil {
			t.Fatalf("dispatch pending Run from queue: %v", err)
		}
	}
	quotaExceeded := createRequest("runs-member", "over-concurrent-quota-run-key", createBody)
	if quotaExceeded.Code != http.StatusConflict || json.Unmarshal(quotaExceeded.Body.Bytes(), &quotaError) != nil || quotaError.Error.Code != "workload_quota_exceeded" {
		t.Fatalf("sixth concurrent Workload Run should return a stable quota conflict: HTTP %d %s", quotaExceeded.Code, quotaExceeded.Body.String())
	}
	enabledHandler := handler
	handler = NewHandler(verifier, pool)
	if disabled := createRequest("runs-member", "disabled-create-key", createBody); disabled.Code != http.StatusServiceUnavailable {
		t.Fatalf("Workload Run creation should be disabled by default: HTTP %d %s", disabled.Code, disabled.Body.String())
	}
	handler = enabledHandler

	list := request("runs-member", http.MethodGet, "/v1/tenants/tenant_runs_a/workload-runs")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "run_owned_member") || strings.Contains(list.Body.String(), "run_owned_owner") {
		t.Fatalf("Tenant member should see only own runs: HTTP %d %s", list.Code, list.Body.String())
	}
	ownerList := request("runs-owner", http.MethodGet, "/v1/tenants/tenant_runs_a/workload-runs?limit=1&offset=0")
	if ownerList.Code != http.StatusOK || !strings.Contains(ownerList.Body.String(), `"total":5`) {
		t.Fatalf("Tenant owner should see all tenant runs with pagination: HTTP %d %s", ownerList.Code, ownerList.Body.String())
	}
	usage := request("runs-member", http.MethodGet, "/v1/tenants/tenant_runs_a/usage")
	if usage.Code != http.StatusOK || !strings.Contains(usage.Body.String(), `"workload_runs_used":5`) || !strings.Contains(usage.Body.String(), `"workload_runs_limit":5`) ||
		!strings.Contains(usage.Body.String(), `"queued_workload_runs_used":0`) || !strings.Contains(usage.Body.String(), `"queued_workload_runs_limit":3`) {
		t.Fatalf("Tenant usage should include five active Workload reservations: HTTP %d %s", usage.Code, usage.Body.String())
	}
	if badPage := request("runs-owner", http.MethodGet, "/v1/tenants/tenant_runs_a/workload-runs?limit=101"); badPage.Code != http.StatusBadRequest {
		t.Fatalf("invalid pagination returned HTTP %d", badPage.Code)
	}
	if hidden := request("runs-member", http.MethodGet, "/v1/tenants/tenant_runs_a/workload-runs/run_owned_owner"); hidden.Code != http.StatusNotFound {
		t.Fatalf("member could read another requester's run: HTTP %d %s", hidden.Code, hidden.Body.String())
	}
	if crossTenant := request("runs-other", http.MethodGet, "/v1/tenants/tenant_runs_a/workload-runs/run_owned_member"); crossTenant.Code != http.StatusNotFound {
		t.Fatalf("cross-Tenant run lookup returned HTTP %d", crossTenant.Code)
	}
	own := request("runs-member", http.MethodGet, "/v1/tenants/tenant_runs_a/workload-runs/run_owned_member")
	if own.Code != http.StatusOK {
		t.Fatalf("requester could not read own Workload Run: HTTP %d %s", own.Code, own.Body.String())
	}
	var detail domain.WorkloadRun
	if err := json.Unmarshal(own.Body.Bytes(), &detail); err != nil || len(detail.Artifacts) != 1 ||
		detail.Artifacts[0].ID != "artifact_run_detail" || detail.Artifacts[0].Kind != "logs" ||
		detail.Artifacts[0].SHA256 != strings.Repeat("a", 64) || strings.Contains(own.Body.String(), "private-artifacts") {
		t.Fatalf("Run detail did not expose safe artifact metadata: detail=%+v body=%s err=%v", detail, own.Body.String(), err)
	}
	artifactTicketPath := "/v1/tenants/tenant_runs_a/workload-runs/run_owned_member/artifacts/artifact_run_detail/download-ticket"
	ticketRecorder := request("runs-member", http.MethodPost, artifactTicketPath)
	var artifactTicket workloadArtifactTicketResponse
	if ticketRecorder.Code != http.StatusCreated || json.Unmarshal(ticketRecorder.Body.Bytes(), &artifactTicket) != nil || artifactTicket.Ticket == "" {
		t.Fatalf("create artifact download ticket = HTTP %d %s", ticketRecorder.Code, ticketRecorder.Body.String())
	}
	ticketClaims, err := workloadartifactaccess.Verify(artifactTicket.Ticket, artifactKey, time.Now().UTC())
	if err != nil || ticketClaims.UserID != "usr_runs_member" || ticketClaims.TenantID != "tenant_runs_a" ||
		ticketClaims.WorkloadRunID != "run_owned_member" || ticketClaims.ArtifactID != "artifact_run_detail" || ticketClaims.ResourcePlane != "rp_runs_a" {
		t.Fatalf("artifact ticket claims = %+v error=%v", ticketClaims, err)
	}
	wrongRequesterTicket := request("runs-member", http.MethodPost, "/v1/tenants/tenant_runs_a/workload-runs/run_owned_owner/artifacts/artifact_run_detail/download-ticket")
	if wrongRequesterTicket.Code != http.StatusNotFound {
		t.Fatalf("member received another requester's artifact ticket: HTTP %d %s", wrongRequesterTicket.Code, wrongRequesterTicket.Body.String())
	}
	resolvePath := "/v1/internal/resource-planes/rp_runs_a/workload-artifacts/artifact_run_detail/access"
	claimsJSON, _ := json.Marshal(workloadArtifactAccessGrant{Claims: ticketClaims})
	resolveReq := httptest.NewRequest(http.MethodPost, resolvePath, strings.NewReader(string(claimsJSON)))
	resolveReq.Header.Set("Content-Type", "application/json")
	resolveReq.Header.Set("X-Hako-Timestamp", strconv.FormatInt(time.Now().UTC().Unix(), 10))
	resolveReq.Header.Set("X-Hako-Request-ID", "req_11111111111111111111111111111111")
	resolveReq.Header.Set("X-Hako-Signature", signArtifactAccessRequest(artifactKey, resolvePath,
		resolveReq.Header.Get("X-Hako-Timestamp"), resolveReq.Header.Get("X-Hako-Request-ID"), claimsJSON))
	resolveRecorder := httptest.NewRecorder()
	handler.ServeHTTP(resolveRecorder, resolveReq)
	var grant workloadArtifactAccessResult
	if resolveRecorder.Code != http.StatusOK || json.Unmarshal(resolveRecorder.Body.Bytes(), &grant) != nil ||
		grant.StorageRef != "s3://private-artifacts/workloads/secret-key-path" || grant.SHA256 != strings.Repeat("a", 64) || grant.SizeBytes != 12 {
		t.Fatalf("resolve artifact grant = HTTP %d grant=%+v body=%s", resolveRecorder.Code, grant, resolveRecorder.Body.String())
	}

	cancelled := request("runs-member", http.MethodPost, "/v1/tenants/tenant_runs_a/workload-runs/run_owned_member/cancel")
	if cancelled.Code != http.StatusAccepted || !strings.Contains(cancelled.Body.String(), `"desired_state":"cancelled"`) {
		t.Fatalf("cancel own Workload Run = HTTP %d %s", cancelled.Code, cancelled.Body.String())
	}
	replayed := request("runs-member", http.MethodPost, "/v1/tenants/tenant_runs_a/workload-runs/run_owned_member/cancel")
	if replayed.Code != http.StatusAccepted {
		t.Fatalf("idempotent cancellation returned HTTP %d %s", replayed.Code, replayed.Body.String())
	}
	if forbidden := request("runs-member", http.MethodPost, "/v1/tenants/tenant_runs_a/workload-runs/run_owned_owner/cancel"); forbidden.Code != http.StatusNotFound {
		t.Fatalf("member cancelled another requester's run: HTTP %d", forbidden.Code)
	}
	var cancelAudits int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE tenant_id = $1 AND actor_user_id = $2 AND action = 'workload_run.cancel'`, "tenant_runs_a", "usr_runs_member").Scan(&cancelAudits); err != nil || cancelAudits != 1 {
		t.Fatalf("cancellation audit count=%d error=%v", cancelAudits, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM tenant_members WHERE tenant_id='tenant_runs_a' AND user_id='usr_runs_member'`); err != nil {
		t.Fatalf("revoke artifact requester membership: %v", err)
	}
	revokedResolveReq := httptest.NewRequest(http.MethodPost, resolvePath, strings.NewReader(string(claimsJSON)))
	revokedResolveReq.Header = resolveReq.Header.Clone()
	revokedResolveRecorder := httptest.NewRecorder()
	handler.ServeHTTP(revokedResolveRecorder, revokedResolveReq)
	if revokedResolveRecorder.Code != http.StatusNotFound {
		t.Fatalf("revoked Tenant member retained artifact access: HTTP %d %s", revokedResolveRecorder.Code, revokedResolveRecorder.Body.String())
	}
}

func signArtifactAccessRequest(key []byte, path, timestamp, requestID string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	message := fmt.Sprintf("%s\n%s\n%s\n%s\n%s", http.MethodPost, path, timestamp, requestID, hex.EncodeToString(bodyHash[:]))
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}
