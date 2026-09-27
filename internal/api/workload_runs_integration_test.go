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

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/testutil"
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
	handler := NewHandler(verifier, pool, WorkspaceCreateConfig{WorkloadRunsEnabled: true})
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
	replayedCreate := createRequest("runs-member", "create-run-key", createBody)
	if replayedCreate.Code != http.StatusAccepted || !strings.Contains(replayedCreate.Body.String(), `"id":"`+string(createdRun.ID)+`"`) {
		t.Fatalf("idempotent create did not return existing run: HTTP %d %s", replayedCreate.Code, replayedCreate.Body.String())
	}
	if conflict := createRequest("runs-member", "create-run-key", strings.Replace(createBody, "refs/heads/main", "refs/heads/other", 1)); conflict.Code != http.StatusConflict {
		t.Fatalf("create idempotency conflict returned HTTP %d %s", conflict.Code, conflict.Body.String())
	}
	if inactive := createRequest("runs-member", "inactive-run-key", strings.Replace(createBody, "9302", "9399", 1)); inactive.Code != http.StatusNotFound {
		t.Fatalf("unregistered repository returned HTTP %d %s", inactive.Code, inactive.Body.String())
	}
	var scheduledEvents, createAudits int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'workload_run.schedule_requested'`, createdRun.ID).Scan(&scheduledEvents); err != nil || scheduledEvents != 1 {
		t.Fatalf("scheduled outbox events=%d error=%v", scheduledEvents, err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE tenant_id = $1 AND actor_user_id = $2 AND action = 'workload_run.create' AND target_id = $3`, "tenant_runs_a", "usr_runs_member", createdRun.ID).Scan(&createAudits); err != nil || createAudits != 1 {
		t.Fatalf("create audit count=%d error=%v", createAudits, err)
	}
	handler = NewHandler(verifier, pool)
	if disabled := createRequest("runs-member", "disabled-create-key", createBody); disabled.Code != http.StatusServiceUnavailable {
		t.Fatalf("Workload Run creation should be disabled by default: HTTP %d %s", disabled.Code, disabled.Body.String())
	}

	list := request("runs-member", http.MethodGet, "/v1/tenants/tenant_runs_a/workload-runs")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "run_owned_member") || strings.Contains(list.Body.String(), "run_owned_owner") {
		t.Fatalf("Tenant member should see only own runs: HTTP %d %s", list.Code, list.Body.String())
	}
	ownerList := request("runs-owner", http.MethodGet, "/v1/tenants/tenant_runs_a/workload-runs?limit=1&offset=0")
	if ownerList.Code != http.StatusOK || !strings.Contains(ownerList.Body.String(), `"total":3`) {
		t.Fatalf("Tenant owner should see all tenant runs with pagination: HTTP %d %s", ownerList.Code, ownerList.Body.String())
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
	if own := request("runs-member", http.MethodGet, "/v1/tenants/tenant_runs_a/workload-runs/run_owned_member"); own.Code != http.StatusOK {
		t.Fatalf("requester could not read own Workload Run: HTTP %d %s", own.Code, own.Body.String())
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
}
