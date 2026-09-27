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
	handler := NewHandler(verifier, pool)
	request := func(subject, method, path string) *httptest.ResponseRecorder {
		t.Helper()
		token := signTenantAPIToken(t, privateKey, issuer, subject, "openid hako/api")
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder
	}

	list := request("runs-member", http.MethodGet, "/v1/tenants/tenant_runs_a/workload-runs")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "run_owned_member") || strings.Contains(list.Body.String(), "run_owned_owner") {
		t.Fatalf("Tenant member should see only own runs: HTTP %d %s", list.Code, list.Body.String())
	}
	ownerList := request("runs-owner", http.MethodGet, "/v1/tenants/tenant_runs_a/workload-runs?limit=1&offset=0")
	if ownerList.Code != http.StatusOK || !strings.Contains(ownerList.Body.String(), `"total":2`) {
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
