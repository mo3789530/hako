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
	previewstore "github.com/mo3789530/hako/internal/store/previews"
	"github.com/mo3789530/hako/internal/testutil"
	"github.com/mo3789530/hako/internal/workspacesession"
)

func TestRepositoryPreviewSessionIsTenantScopedAndFencedByPreviewLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_preview_session','Preview Session',now()),('tenant_preview_session_foreign','Foreign',now())`,
		`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_preview_session_owner','preview-session-owner','',now()),('usr_preview_session_foreign','preview-session-foreign','',now())`,
		`INSERT INTO tenant_members(tenant_id,user_id,role,joined_at) VALUES ('tenant_preview_session','usr_preview_session_owner','member',now()),('tenant_preview_session_foreign','usr_preview_session_foreign','owner',now())`,
		`INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES ('rp_preview_session','aws','ap-northeast-1','["microvm"]')`,
		`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_preview_session',8801,'acme','active','usr_preview_session_owner',now(),now())`,
		`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (8801,'tenant_preview_session',now())`,
		`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at) VALUES ('tenant_preview_session',8801,8802,'acme','preview-session','main',now())`,
		`INSERT INTO repository_previews(tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,desired_state,state,last_delivery_id,last_event_at,generation,expires_at,created_at,updated_at)
		 VALUES ('tenant_preview_session',8801,8802,19,$1,'feature/session','present','awaiting_runtime','40000000-0000-4000-8000-000000000001',now(),2,now()+interval '1 minute',now(),now())`,
		`INSERT INTO repository_preview_runtime_allocations(tenant_id,installation_id,github_repository_id,pull_request_number,generation,head_sha,head_ref,desired_state,state,expires_at,resource_plane_id,workspace_id,microvm_id,gateway_route_id,created_at,updated_at)
		 VALUES ('tenant_preview_session',8801,8802,19,2,$1,'feature/session','present','running',now()+interval '1 minute','rp_preview_session','prvws_preview_session','lambda-preview-session','route-preview-session',now(),now())`,
	} {
		var err error
		if strings.Contains(query, "$1") {
			_, err = pool.Exec(ctx, query, strings.Repeat("a", 40))
		} else {
			_, err = pool.Exec(ctx, query)
		}
		if err != nil {
			t.Fatalf("seed Preview session data: %v", err)
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
	key := []byte(strings.Repeat("p", 32))
	ticketIssuer := &workspacesession.Ed25519Issuer{KeyID: "preview-session-key", PrivateKey: ticketPrivate, TTL: 2 * time.Minute}
	handler := NewHandler(verifier, pool, WorkspaceCreateConfig{
		GitHubAppManagementEnabled:   true,
		WorkspaceSessionTicketIssuer: ticketIssuer,
		ResourcePlaneWorkspaceSessionKeyResolver: func(_ context.Context, planeID domain.ResourcePlaneID) ([]byte, error) {
			if planeID != "rp_preview_session" {
				return nil, errors.New("unknown Resource Plane")
			}
			return key, nil
		},
	})
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
	path := "/v1/tenants/tenant_preview_session/github/installations/8801/repositories/8802/previews/19/sessions"
	created := request("preview-session-owner", http.MethodPost, path, `{"permissions":["vscode","port-forward"]}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create Preview session = HTTP %d %s", created.Code, created.Body.String())
	}
	var response repositoryPreviewSessionResponse
	if err := json.Unmarshal(created.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.SessionID == "" || response.WorkspaceID != "prvws_preview_session" || response.Ticket == "" || response.ExpiresAt.After(time.Now().Add(2*time.Minute)) ||
		strings.Contains(created.Body.String(), "lambda-preview-session") || strings.Contains(created.Body.String(), "route-preview-session") {
		t.Fatalf("Preview session response has missing fields or provider data: %s", created.Body.String())
	}
	claims, err := workspacesession.Verify(response.Ticket, map[string]ed25519.PublicKey{"preview-session-key": ticketPublic}, workspacesession.AudienceGateway, time.Now())
	if err != nil || claims.UserID != "usr_preview_session_owner" || claims.TenantID != "tenant_preview_session" ||
		claims.WorkspaceID != "prvws_preview_session" || claims.ResourcePlane != "rp_preview_session" || time.Unix(claims.ExpiresAt, 0).After(response.ExpiresAt) {
		t.Fatalf("Preview ticket claims = %+v, error=%v", claims, err)
	}
	lookupHandler := NewHandler(nil, pool, WorkspaceCreateConfig{ResourcePlaneWorkspaceSessionKeyResolver: func(context.Context, domain.ResourcePlaneID) ([]byte, error) { return key, nil }})
	lookupServer := httptest.NewTLSServer(lookupHandler)
	defer lookupServer.Close()
	activeLookup, err := workspacesession.NewHTTPActiveSessionLookup(workspacesession.HTTPActiveSessionLookupConfig{
		Endpoint: lookupServer.URL, ResourcePlaneID: "rp_preview_session", HMACKey: key, Transport: lookupServer.Client().Transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	if active, err := activeLookup.IsActive(ctx, claims); err != nil || !active {
		t.Fatalf("active Preview session lookup = %v, %v; want true", active, err)
	}
	if foreign := request("preview-session-foreign", http.MethodPost, path, `{}`); foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign Tenant session request = HTTP %d %s", foreign.Code, foreign.Body.String())
	}
	if invalid := request("preview-session-owner", http.MethodPost, path, `{"unexpected":true}`); invalid.Code != http.StatusBadRequest {
		t.Fatalf("unknown session field status = HTTP %d %s", invalid.Code, invalid.Body.String())
	}
	for _, permissions := range []string{`["shell"]`, `["exec"]`, `["file-transfer"]`, `["ssh-proxy"]`} {
		invalid := request("preview-session-owner", http.MethodPost, path, `{"permissions":`+permissions+`}`)
		if invalid.Code != http.StatusBadRequest {
			t.Errorf("unsafe Preview Session permissions %s returned HTTP %d %s", permissions, invalid.Code, invalid.Body.String())
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE repository_previews SET desired_state='absent',state='cleanup_pending',generation=generation+1 WHERE tenant_id='tenant_preview_session' AND installation_id=8801 AND github_repository_id=8802 AND pull_request_number=19`); err != nil {
		t.Fatal(err)
	}
	if active, err := activeLookup.IsActive(ctx, claims); err != nil || active {
		t.Fatalf("closed/stale Preview ticket remained active = %v, %v", active, err)
	}
	if stale := request("preview-session-owner", http.MethodPost, path, `{}`); stale.Code != http.StatusConflict {
		t.Fatalf("closed Preview issued a new session: HTTP %d %s", stale.Code, stale.Body.String())
	}
	wrongScope := request("preview-session-owner", http.MethodDelete, strings.Replace(path, "/previews/19/", "/previews/20/", 1)+"/"+string(response.SessionID), "")
	if wrongScope.Code != http.StatusNotFound {
		t.Fatalf("wrong-PR revocation returned HTTP %d %s", wrongScope.Code, wrongScope.Body.String())
	}
	var stillStored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM repository_preview_sessions WHERE id=$1`, response.SessionID).Scan(&stillStored); err != nil || stillStored != 1 {
		t.Fatalf("wrong-scope revocation changed session rows: count=%d error=%v", stillStored, err)
	}
	revoked := request("preview-session-owner", http.MethodDelete, path+"/"+string(response.SessionID), "")
	if revoked.Code != http.StatusNoContent {
		t.Fatalf("revoke Preview session = HTTP %d %s", revoked.Code, revoked.Body.String())
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id='tenant_preview_session' AND target_id=$1 AND action IN ('github.preview.session.create','github.preview.session.revoke')`, response.SessionID).Scan(&auditCount); err != nil || auditCount != 2 {
		t.Fatalf("Preview session audit count = %d, error=%v", auditCount, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repository_preview_sessions
		(id,user_id,tenant_id,installation_id,github_repository_id,pull_request_number,generation,workspace_id,resource_plane_id,permissions_json,expires_at,created_at)
		VALUES ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','usr_preview_session_owner','tenant_preview_session',8801,8802,19,2,'prvws_preview_session','rp_preview_session','["vscode"]',now()-interval '1 second',now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := previewstore.ExpireBatch(ctx, pool, time.Now().UTC(), 10); err != nil {
		t.Fatalf("Preview expiry sweep did not remove expired Session rows: %v", err)
	}
	var expiredRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM repository_preview_sessions WHERE id='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'`).Scan(&expiredRows); err != nil || expiredRows != 0 {
		t.Fatalf("expired Preview Session rows remaining=%d error=%v", expiredRows, err)
	}
}
