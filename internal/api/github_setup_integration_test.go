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
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/testutil"
)

type integrationGitHubSetupVerifier struct {
	verifiedInstallationID int64
	verifiedCodeVerifier   string
	appID                  int64
	beforeVerify           func()
}

func (v *integrationGitHubSetupVerifier) SetupURL(state string) string {
	return "https://github.com/apps/hako/installations/new?state=" + url.QueryEscape(state)
}

func (v *integrationGitHubSetupVerifier) AuthorizationURL(state, codeVerifier string) string {
	if len(codeVerifier) < 43 {
		return "invalid"
	}
	return "https://github.com/login/oauth/authorize?state=" + url.QueryEscape(state) + "&code_challenge=challenge&code_challenge_method=S256"
}

func (v *integrationGitHubSetupVerifier) VerifyInstallation(_ context.Context, code string, installationID int64, codeVerifier string) (githubapp.InstallationIdentity, error) {
	if code != "valid-code" {
		return githubapp.InstallationIdentity{}, context.Canceled
	}
	v.verifiedInstallationID = installationID
	v.verifiedCodeVerifier = codeVerifier
	if v.beforeVerify != nil {
		v.beforeVerify()
	}
	appID := v.appID
	if appID == 0 {
		appID = 123
	}
	return githubapp.InstallationIdentity{ID: installationID, AppID: appID, AccountLogin: "acme", GitHubUserID: 5678, GitHubUserLogin: "hako-user"}, nil
}

type integrationSetupResolver func(context.Context, githubregistry.SetupAppPin) (githubapp.SetupVerifier, error)

func (f integrationSetupResolver) ResolveSetupVerifier(ctx context.Context, pin githubregistry.SetupAppPin) (githubapp.SetupVerifier, error) {
	return f(ctx, pin)
}

func TestGitHubInstallationSetupUsesOneTimeStateAndVerifiedCallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, created_at) VALUES ('tenant_github_setup_api', 'GitHub setup', $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('usr_github_setup_api', 'github-setup-api-sub', '', $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ('tenant_github_setup_api', 'usr_github_setup_api', 'owner', $1)`, now); err != nil {
		t.Fatal(err)
	}
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	githubVerifier := &integrationGitHubSetupVerifier{}
	handler := NewHandlerWithGitHubApp(verifier, pool, nil, githubVerifier)
	appToken := signTenantAPIToken(t, privateKey, issuer, "github-setup-api-sub", "openid hako/api")
	start := httptest.NewRequest(http.MethodPost, "/v1/tenants/tenant_github_setup_api/github/installations/setup", strings.NewReader(`{}`))
	start.Header.Set("Authorization", "Bearer "+appToken)
	start.Header.Set("Content-Type", "application/json")
	startResponse := httptest.NewRecorder()
	handler.ServeHTTP(startResponse, start)
	if startResponse.Code != http.StatusCreated {
		t.Fatalf("start setup = HTTP %d %s", startResponse.Code, startResponse.Body.String())
	}
	var setup struct {
		SetupURL string `json:"setup_url"`
	}
	if err := json.Unmarshal(startResponse.Body.Bytes(), &setup); err != nil {
		t.Fatal(err)
	}
	setupURL, err := url.Parse(setup.SetupURL)
	if err != nil || setupURL.Query().Get("state") == "" {
		t.Fatalf("setup URL = %q err=%v", setup.SetupURL, err)
	}
	state := setupURL.Query().Get("state")

	installationCallback := httptest.NewRequest(http.MethodGet, "/v1/integrations/github/setup/callback?installation_id=7743&setup_action=install&state="+url.QueryEscape(state), nil)
	installationResponse := httptest.NewRecorder()
	handler.ServeHTTP(installationResponse, installationCallback)
	if installationResponse.Code != http.StatusFound || !strings.Contains(installationResponse.Header().Get("Location"), "github.com/login/oauth/authorize") || !strings.Contains(installationResponse.Header().Get("Location"), "code_challenge_method=S256") || strings.Contains(installationResponse.Header().Get("Location"), "code_verifier=") {
		t.Fatalf("installation callback = HTTP %d location=%q body=%s", installationResponse.Code, installationResponse.Header().Get("Location"), installationResponse.Body.String())
	}

	oauthCallback := httptest.NewRequest(http.MethodGet, "/v1/integrations/github/setup/callback?code=valid-code&state="+url.QueryEscape(state), nil)
	oauthResponse := httptest.NewRecorder()
	handler.ServeHTTP(oauthResponse, oauthCallback)
	if oauthResponse.Code != http.StatusOK || !strings.Contains(oauthResponse.Body.String(), `"verified":true`) || githubVerifier.verifiedInstallationID != 7743 || len(githubVerifier.verifiedCodeVerifier) < 43 {
		t.Fatalf("OAuth callback = HTTP %d verified ID=%d body=%s", oauthResponse.Code, githubVerifier.verifiedInstallationID, oauthResponse.Body.String())
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM tenant_github_installations WHERE tenant_id = 'tenant_github_setup_api' AND installation_id = 7743`).Scan(&status); err != nil || status != "active" {
		t.Fatalf("verified Tenant Installation status = %q err=%v", status, err)
	}
	var bindings int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM github_app_installation_bindings WHERE installation_id = 7743 AND tenant_id = 'tenant_github_setup_api'`).Scan(&bindings); err != nil || bindings != 1 {
		t.Fatalf("Tenant binding count = %d err=%v", bindings, err)
	}
	var githubUserID int64
	var githubLogin string
	if err := pool.QueryRow(ctx, `SELECT github_user_id,github_login FROM users WHERE id='usr_github_setup_api'`).Scan(&githubUserID, &githubLogin); err != nil || githubUserID != 5678 || githubLogin != "hako-user" {
		t.Fatalf("verified GitHub identity link = %d/%q err=%v", githubUserID, githubLogin, err)
	}
	replay := httptest.NewRequest(http.MethodGet, "/v1/integrations/github/setup/callback?code=valid-code&state="+url.QueryEscape(state), nil)
	replayResponse := httptest.NewRecorder()
	handler.ServeHTTP(replayResponse, replay)
	if replayResponse.Code != http.StatusBadRequest {
		t.Fatalf("replayed setup callback = HTTP %d %s, want 400", replayResponse.Code, replayResponse.Body.String())
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id,name,created_at) VALUES ('tenant_github_setup_other','Other GitHub setup',$1)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_github_setup_other','github-setup-other-sub','',$1)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_members (tenant_id,user_id,role,joined_at) VALUES ('tenant_github_setup_other','usr_github_setup_other','owner',$1)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	otherToken := signTenantAPIToken(t, privateKey, issuer, "github-setup-other-sub", "openid hako/api")
	otherStart := httptest.NewRequest(http.MethodPost, "/v1/tenants/tenant_github_setup_other/github/installations/setup", strings.NewReader(`{}`))
	otherStart.Header.Set("Authorization", "Bearer "+otherToken)
	otherStart.Header.Set("Content-Type", "application/json")
	otherStartResponse := httptest.NewRecorder()
	handler.ServeHTTP(otherStartResponse, otherStart)
	if otherStartResponse.Code != http.StatusCreated {
		t.Fatalf("other user setup start = HTTP %d %s", otherStartResponse.Code, otherStartResponse.Body.String())
	}
	if err := json.Unmarshal(otherStartResponse.Body.Bytes(), &setup); err != nil {
		t.Fatal(err)
	}
	otherURL, err := url.Parse(setup.SetupURL)
	if err != nil || otherURL.Query().Get("state") == "" {
		t.Fatalf("other setup URL = %q err=%v", setup.SetupURL, err)
	}
	otherState := otherURL.Query().Get("state")
	otherInstall := httptest.NewRequest(http.MethodGet, "/v1/integrations/github/setup/callback?installation_id=7744&setup_action=install&state="+url.QueryEscape(otherState), nil)
	otherInstallResponse := httptest.NewRecorder()
	handler.ServeHTTP(otherInstallResponse, otherInstall)
	if otherInstallResponse.Code != http.StatusFound {
		t.Fatalf("other Installation callback = HTTP %d %s", otherInstallResponse.Code, otherInstallResponse.Body.String())
	}
	otherOAuth := httptest.NewRequest(http.MethodGet, "/v1/integrations/github/setup/callback?code=valid-code&state="+url.QueryEscape(otherState), nil)
	otherOAuthResponse := httptest.NewRecorder()
	handler.ServeHTTP(otherOAuthResponse, otherOAuth)
	if otherOAuthResponse.Code != http.StatusConflict || !strings.Contains(otherOAuthResponse.Body.String(), "github_identity_conflict") {
		t.Fatalf("cross-user GitHub identity link = HTTP %d %s", otherOAuthResponse.Code, otherOAuthResponse.Body.String())
	}
	var otherBindingCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM github_app_installation_bindings WHERE installation_id=7744`).Scan(&otherBindingCount); err != nil || otherBindingCount != 0 {
		t.Fatalf("conflicting identity activated an Installation binding: count=%d err=%v", otherBindingCount, err)
	}
	for _, statement := range []string{
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at) VALUES ('api_setup_binding','tenant_github_setup_api',911,'approved','private','oauth','webhook',now())`,
		`INSERT INTO github_app_registrations (id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('api_setup_registration',911,'tenant_owned','tenant_github_setup_api','owned-app','client',now())`,
		`INSERT INTO tenant_github_integrations (id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('api_setup_integration','tenant_github_setup_api','api_setup_registration',911,'api_setup_binding','active',3,6,'api-setup-key','hash','usr_github_setup_api',now(),now())`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	ownedVerifier := &integrationGitHubSetupVerifier{appID: 911}
	resolverCalls := 0
	ownedHandler := NewHandlerWithGitHubApp(verifier, pool, nil, githubVerifier, WorkspaceCreateConfig{
		GitHubAppManagementEnabled: true,
		TenantGitHubSetupResolver: integrationSetupResolver(func(_ context.Context, pin githubregistry.SetupAppPin) (githubapp.SetupVerifier, error) {
			resolverCalls++
			if pin.TenantID != "tenant_github_setup_api" || pin.IntegrationID != "api_setup_integration" || pin.GitHubAppID != 911 || pin.ConfigRevision != 3 {
				t.Fatalf("untrusted setup identity: %+v", pin)
			}
			return ownedVerifier, nil
		}),
	})
	startOwned := func(tenant string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/tenants/"+tenant+"/github/apps/api_setup_integration/setup", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+appToken)
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		ownedHandler.ServeHTTP(res, req)
		return res
	}
	if response := startOwned("tenant_other"); response.Code != http.StatusNotFound || resolverCalls != 0 {
		t.Fatalf("cross-Tenant setup accessed broker: HTTP %d calls=%d", response.Code, resolverCalls)
	}
	startOwnedState := func() string {
		response := startOwned("tenant_github_setup_api")
		if response.Code != http.StatusCreated {
			t.Fatalf("owned setup start: HTTP %d %s", response.Code, response.Body.String())
		}
		if err := json.Unmarshal(response.Body.Bytes(), &setup); err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(setup.SetupURL)
		if err != nil {
			t.Fatal(err)
		}
		return u.Query().Get("state")
	}
	ownedState := startOwnedState()
	callbackURL := "/v1/integrations/github/setup/callback?installation_id=7744&setup_action=install&state=" + url.QueryEscape(ownedState)
	noBroker := httptest.NewRecorder()
	handler.ServeHTTP(noBroker, httptest.NewRequest(http.MethodGet, callbackURL, nil))
	if noBroker.Code != http.StatusBadGateway {
		t.Fatalf("owned setup fell back to shared verifier: HTTP %d", noBroker.Code)
	}
	redirect := httptest.NewRecorder()
	ownedHandler.ServeHTTP(redirect, httptest.NewRequest(http.MethodGet, callbackURL+"&tenant_id=attacker&integration_id=attacker", nil))
	if redirect.Code != http.StatusFound {
		t.Fatalf("owned setup redirect: HTTP %d %s", redirect.Code, redirect.Body.String())
	}
	oauthOwned := "/v1/integrations/github/setup/callback?code=valid-code&state=" + url.QueryEscape(ownedState)
	ownedVerifier.appID = 912
	wrongApp := httptest.NewRecorder()
	ownedHandler.ServeHTTP(wrongApp, httptest.NewRequest(http.MethodGet, oauthOwned, nil))
	if wrongApp.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-App callback accepted: HTTP %d", wrongApp.Code)
	}
	ownedVerifier.appID = 911
	ownedVerifier.beforeVerify = func() {
		if _, err := pool.Exec(ctx, `UPDATE tenant_github_integrations SET credential_generation=7 WHERE id='api_setup_integration'`); err != nil {
			t.Fatal(err)
		}
	}
	rotated := httptest.NewRecorder()
	ownedHandler.ServeHTTP(rotated, httptest.NewRequest(http.MethodGet, oauthOwned, nil))
	if rotated.Code != http.StatusBadRequest {
		t.Fatalf("changed generation accepted after OAuth verification: HTTP %d %s", rotated.Code, rotated.Body.String())
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tenant_github_app_installation_bindings WHERE installation_id=7744`).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("failed setup created binding: %d %v", bindings, err)
	}
	ownedVerifier.beforeVerify = nil
	ownedState = startOwnedState()
	redirect = httptest.NewRecorder()
	ownedHandler.ServeHTTP(redirect, httptest.NewRequest(http.MethodGet, "/v1/integrations/github/setup/callback?installation_id=7744&setup_action=install&state="+url.QueryEscape(ownedState), nil))
	if redirect.Code != http.StatusFound {
		t.Fatalf("new owned setup redirect: HTTP %d", redirect.Code)
	}
	completed := httptest.NewRecorder()
	ownedHandler.ServeHTTP(completed, httptest.NewRequest(http.MethodGet, "/v1/integrations/github/setup/callback?code=valid-code&state="+url.QueryEscape(ownedState), nil))
	if completed.Code != http.StatusOK {
		t.Fatalf("complete owned setup: HTTP %d %s", completed.Code, completed.Body.String())
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tenant_github_app_installation_bindings WHERE installation_id=7744 AND integration_id='api_setup_integration'`).Scan(&bindings); err != nil || bindings != 1 {
		t.Fatalf("verified owned binding absent: %d %v", bindings, err)
	}
}
