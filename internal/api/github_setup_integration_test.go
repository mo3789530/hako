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
	"github.com/mo3789530/hako/internal/testutil"
)

type integrationGitHubSetupVerifier struct {
	verifiedInstallationID int64
	verifiedCodeVerifier   string
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
	return githubapp.InstallationIdentity{ID: installationID, AppID: 123, AccountLogin: "acme"}, nil
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
	replay := httptest.NewRequest(http.MethodGet, "/v1/integrations/github/setup/callback?code=valid-code&state="+url.QueryEscape(state), nil)
	replayResponse := httptest.NewRecorder()
	handler.ServeHTTP(replayResponse, replay)
	if replayResponse.Code != http.StatusBadRequest {
		t.Fatalf("replayed setup callback = HTTP %d %s, want 400", replayResponse.Code, replayResponse.Body.String())
	}
}
