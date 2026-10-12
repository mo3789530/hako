//go:build integration

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/testutil"
	"github.com/mo3789530/hako/internal/workspacesession"
)

func TestWorkspaceSessionAPIEnforcesRBACPersistsAndRevokesTickets(t *testing.T) {
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
		{`INSERT INTO tenants (id, name, created_at) VALUES ($1, $2, $3)`, []any{"tenant_sessions_a", "Session A", now}},
		{`INSERT INTO tenants (id, name, created_at) VALUES ($1, $2, $3)`, []any{"tenant_sessions_b", "Session B", now}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ($1, $2, '', $3)`, []any{"usr_session_owner", "session-owner", now}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ($1, $2, '', $3)`, []any{"usr_session_other", "session-other", now}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ($1, $2, '', $3)`, []any{"usr_session_foreign", "session-foreign", now}},
		{`INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ($1, $2, 'member', $3)`, []any{"tenant_sessions_a", "usr_session_owner", now}},
		{`INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ($1, $2, 'member', $3)`, []any{"tenant_sessions_a", "usr_session_other", now}},
		{`INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ($1, $2, 'owner', $3)`, []any{"tenant_sessions_b", "usr_session_foreign", now}},
		{`INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES ($1, 'aws', 'ap-northeast-1', '["microvm"]')`, []any{"rp_sessions_a"}},
		{`INSERT INTO workspaces (id, tenant_id, owner_id, name, runtime_class, image, created_at, updated_at) VALUES ($1, $2, $3, $4, 'standard', 'hako/test', $5, $5)`, []any{"ws_session_owner", "tenant_sessions_a", "usr_session_owner", "owner-ws", now}},
		{`INSERT INTO workspaces (id, tenant_id, owner_id, name, runtime_class, image, created_at, updated_at) VALUES ($1, $2, $3, $4, 'standard', 'hako/test', $5, $5)`, []any{"ws_session_other", "tenant_sessions_a", "usr_session_other", "other-ws", now}},
		{`INSERT INTO workspaces (id, tenant_id, owner_id, name, runtime_class, image, created_at, updated_at) VALUES ($1, $2, $3, $4, 'standard', 'hako/test', $5, $5)`, []any{"ws_session_stopped", "tenant_sessions_a", "usr_session_owner", "stopped-ws", now}},
		{`INSERT INTO workspace_status (workspace_id, desired_state, observed_state, updated_at) VALUES ($1, 'running', 'running', $2)`, []any{"ws_session_owner", now}},
		{`INSERT INTO workspace_status (workspace_id, desired_state, observed_state, updated_at) VALUES ($1, 'running', 'running', $2)`, []any{"ws_session_other", now}},
		{`INSERT INTO workspace_status (workspace_id, desired_state, observed_state, updated_at) VALUES ($1, 'running', 'suspended', $2)`, []any{"ws_session_stopped", now}},
		{`INSERT INTO placements (workspace_id, resource_plane_id, placed_at) VALUES ($1, $2, $3)`, []any{"ws_session_owner", "rp_sessions_a", now}},
		{`INSERT INTO placements (workspace_id, resource_plane_id, placed_at) VALUES ($1, $2, $3)`, []any{"ws_session_other", "rp_sessions_a", now}},
		{`INSERT INTO placements (workspace_id, resource_plane_id, placed_at) VALUES ($1, $2, $3)`, []any{"ws_session_stopped", "rp_sessions_a", now}},
		{`INSERT INTO sessions (id, user_id, tenant_id, workspace_id, resource_plane_id, permissions_json, expires_at, created_at) VALUES ($1, $2, $3, $4, $5, '["shell"]', $6, $7)`, []any{"000000000000000000000000000000000000000000000000", "usr_session_owner", "tenant_sessions_a", "ws_session_owner", "rp_sessions_a", now.Add(-time.Minute), now.Add(-2 * time.Minute)}},
	}
	for _, seed := range seeds {
		if _, err := pool.Exec(ctx, seed.query, seed.args...); err != nil {
			t.Fatalf("seed API data: %v", err)
		}
	}

	apiPrivateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "tenant-api-key",
			"n": base64.RawURLEncoding.EncodeToString(apiPrivateKey.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1})}}})
	}))
	defer keys.Close()
	issuerURL := keys.URL + "/pool"
	verifier, err := auth.NewCognitoVerifier(auth.CognitoVerifierConfig{Issuer: issuerURL, ClientID: "hako-cli"})
	if err != nil {
		t.Fatal(err)
	}
	ticketPublic, ticketPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ticketIssuer := &workspacesession.Ed25519Issuer{KeyID: "session-key-v1", PrivateKey: ticketPrivate, TTL: 2 * time.Minute}
	handler := NewHandler(verifier, pool, WorkspaceCreateConfig{WorkspaceSessionTicketIssuer: ticketIssuer})
	request := func(subject, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+signTenantAPIToken(t, apiPrivateKey, issuerURL, subject, "openid hako/api"))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder
	}
	basePath := "/v1/tenants/tenant_sessions_a/workspaces/ws_session_owner/sessions"
	created := request("session-owner", http.MethodPost, basePath, `{"permissions":["shell","port-forward"]}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create Workspace session = HTTP %d %s", created.Code, created.Body.String())
	}
	var response createWorkspaceSessionResponse
	if err := json.Unmarshal(created.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.SessionID == "" || response.Ticket == "" || !response.ExpiresAt.After(now) || len(response.Ticket) > 8192 {
		t.Fatalf("invalid session response (ticket omitted): %+v", response)
	}
	claims, err := workspacesession.Verify(response.Ticket, map[string]ed25519.PublicKey{"session-key-v1": ticketPublic}, workspacesession.AudienceGateway, time.Now())
	if err != nil {
		t.Fatalf("verify issued session ticket: %v", err)
	}
	if claims.UserID != "usr_session_owner" || claims.TenantID != "tenant_sessions_a" || claims.WorkspaceID != "ws_session_owner" || claims.ResourcePlane != "rp_sessions_a" {
		t.Fatalf("ticket did not bind authorized Workspace route: %+v", claims)
	}
	lookupKey := []byte(strings.Repeat("s", 32))
	lookupHandler := NewHandler(nil, pool, WorkspaceCreateConfig{ResourcePlaneWorkspaceSessionKeyResolver: func(_ context.Context, planeID domain.ResourcePlaneID) ([]byte, error) {
		if planeID != "rp_sessions_a" {
			return nil, errors.New("unknown Resource Plane")
		}
		return lookupKey, nil
	}})
	lookupServer := httptest.NewTLSServer(lookupHandler)
	defer lookupServer.Close()
	activeLookup, err := workspacesession.NewHTTPActiveSessionLookup(workspacesession.HTTPActiveSessionLookupConfig{
		Endpoint: lookupServer.URL, ResourcePlaneID: "rp_sessions_a", HMACKey: lookupKey,
		Transport: lookupServer.Client().Transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	if active, err := activeLookup.IsActive(ctx, claims); err != nil || !active {
		t.Fatalf("active session lookup = %v, %v; want true, nil", active, err)
	}
	wrongIdentity := claims
	wrongIdentity.UserID = "usr_session_other"
	if active, err := activeLookup.IsActive(ctx, wrongIdentity); err != nil || active {
		t.Fatalf("session lookup for mismatched user = %v, %v; want false, nil", active, err)
	}
	expiredClaims := claims
	expiredClaims.TicketID = "000000000000000000000000000000000000000000000000"
	expiredClaims.Permissions = []workspacesession.Permission{workspacesession.PermissionShell}
	if active, err := activeLookup.IsActive(ctx, expiredClaims); err != nil || active {
		t.Fatalf("expired session lookup = %v, %v; want false, nil", active, err)
	}
	wrongKeyLookup, err := workspacesession.NewHTTPActiveSessionLookup(workspacesession.HTTPActiveSessionLookupConfig{
		Endpoint: lookupServer.URL, ResourcePlaneID: "rp_sessions_a", HMACKey: []byte(strings.Repeat("x", 32)),
		Transport: lookupServer.Client().Transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	if active, err := wrongKeyLookup.IsActive(ctx, claims); err == nil || active {
		t.Fatalf("invalid Resource Plane HMAC returned active=%v error=%v", active, err)
	}
	var storedUser string
	if err := pool.QueryRow(ctx, `SELECT user_id FROM sessions WHERE id = $1 AND permissions_json = '["port-forward","shell"]'`, response.SessionID).Scan(&storedUser); err != nil || storedUser != "usr_session_owner" {
		t.Fatalf("session was not persisted with canonical permissions: user=%q error=%v", storedUser, err)
	}
	var expiredCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM sessions WHERE id = $1`, "000000000000000000000000000000000000000000000000").Scan(&expiredCount); err != nil || expiredCount != 0 {
		t.Fatalf("expired sessions were not pruned: count=%d error=%v", expiredCount, err)
	}
	if invalid := request("session-owner", http.MethodPost, basePath, `{"permissions":["admin"]}`); invalid.Code != http.StatusBadRequest {
		t.Fatalf("unknown permission status = %d %s", invalid.Code, invalid.Body.String())
	}
	if hidden := request("session-other", http.MethodPost, basePath, `{}`); hidden.Code != http.StatusNotFound {
		t.Fatalf("same-Tenant non-owner session request = HTTP %d %s", hidden.Code, hidden.Body.String())
	}
	if foreign := request("session-foreign", http.MethodPost, basePath, `{}`); foreign.Code != http.StatusNotFound {
		t.Fatalf("cross-Tenant session request = HTTP %d %s", foreign.Code, foreign.Body.String())
	}
	if stopped := request("session-owner", http.MethodPost, "/v1/tenants/tenant_sessions_a/workspaces/ws_session_stopped/sessions", `{}`); stopped.Code != http.StatusConflict {
		t.Fatalf("non-running Workspace session request = HTTP %d %s", stopped.Code, stopped.Body.String())
	}
	revokePath := basePath + "/" + string(response.SessionID)
	if denied := request("session-other", http.MethodDelete, revokePath, ""); denied.Code != http.StatusNotFound {
		t.Fatalf("non-owner revoke status = %d %s", denied.Code, denied.Body.String())
	}
	if revoked := request("session-owner", http.MethodDelete, revokePath, ""); revoked.Code != http.StatusNoContent {
		t.Fatalf("revoke session = HTTP %d %s", revoked.Code, revoked.Body.String())
	}
	if active, err := activeLookup.IsActive(ctx, claims); err != nil || active {
		t.Fatalf("revoked session lookup = %v, %v; want false, nil", active, err)
	}
	var remaining, auditCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM sessions WHERE id = $1`, response.SessionID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("revoked session remains active: count=%d error=%v", remaining, err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE tenant_id = $1 AND actor_user_id = $2 AND action IN ('workspace.session.create','workspace.session.revoke') AND target_id = $3`, "tenant_sessions_a", "usr_session_owner", response.SessionID).Scan(&auditCount); err != nil || auditCount != 2 {
		t.Fatalf("session audit events=%d error=%v", auditCount, err)
	}
}
