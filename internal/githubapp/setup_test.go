package githubapp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSetupClientExchangesCodeAndVerifiesUserInstallation(t *testing.T) {
	var installationAuth string
	const codeVerifier = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/oauth/access_token":
			if r.Method != http.MethodPost || r.Header.Get("Accept") != "application/json" {
				t.Errorf("OAuth request method/header = %s %q", r.Method, r.Header.Get("Accept"))
			}
			body, _ := io.ReadAll(r.Body)
			values, _ := url.ParseQuery(string(body))
			if values.Get("client_id") != "client-id" || values.Get("client_secret") != "client-secret" || values.Get("code") != "one-time-code" || values.Get("redirect_uri") != "https://hako.example/api/v1/integrations/github/setup/callback" || values.Get("code_verifier") != codeVerifier {
				t.Errorf("OAuth form = %v", values)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "transient-user-token", "token_type": "bearer"})
		case "/user/installations":
			if r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("page") != "1" {
				t.Errorf("Installations pagination query = %v", r.URL.Query())
			}
			installationAuth = r.Header.Get("Authorization")
			if r.Header.Get("X-GitHub-Api-Version") == "" {
				t.Error("GitHub API version header was not set")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "installations": []any{map[string]any{"id": 987, "app_id": 123, "account": map[string]string{"login": "acme"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewSetupClient(SetupConfig{Slug: "hako-app", AppID: 123, ClientID: "client-id", ClientSecret: "client-secret", CallbackURL: "https://hako.example/api/v1/integrations/github/setup/callback", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.oauthURL = server.URL + "/login/oauth/access_token"
	client.apiURL = server.URL
	identity, err := client.VerifyInstallation(context.Background(), "one-time-code", 987, codeVerifier)
	if err != nil {
		t.Fatalf("verify Installation: %v", err)
	}
	if identity.ID != 987 || identity.AppID != 123 || identity.AccountLogin != "acme" {
		t.Fatalf("Installation identity = %+v", identity)
	}
	if installationAuth != "Bearer transient-user-token" {
		t.Fatalf("Installation authorization header = %q", installationAuth)
	}
	if strings.Contains(client.SetupURL("state + value"), " ") || !strings.Contains(client.SetupURL("state + value"), "state=state+%2B+value") {
		t.Fatalf("setup URL did not safely encode state: %s", client.SetupURL("state + value"))
	}
	parsed, err := url.Parse(client.AuthorizationURL("nonce", codeVerifier))
	if err != nil || parsed.Query().Get("state") != "nonce" || parsed.Query().Get("redirect_uri") != "https://hako.example/api/v1/integrations/github/setup/callback" || parsed.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("OAuth authorization URL = %q, err=%v", client.AuthorizationURL("nonce", codeVerifier), err)
	}
	challengeDigest := sha256.Sum256([]byte(codeVerifier))
	if parsed.Query().Get("code_challenge") != base64.RawURLEncoding.EncodeToString(challengeDigest[:]) {
		t.Fatalf("OAuth authorization URL has invalid PKCE challenge: %q", parsed.Query().Get("code_challenge"))
	}
}

func TestSetupClientRejectsDifferentAppOrInstallation(t *testing.T) {
	for _, body := range []string{
		`{"id":987,"app_id":999,"account":{"login":"acme"}}`,
		`{"id":988,"app_id":123,"account":{"login":"acme"}}`,
		`{"id":987,"app_id":123,"account":{"login":"-invalid"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/login/oauth/access_token" {
					_, _ = io.WriteString(w, `{"access_token":"token","token_type":"bearer"}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"total_count":1,"installations":[`+body+`]}`)
			}))
			defer server.Close()
			client, err := NewSetupClient(SetupConfig{Slug: "hako-app", AppID: 123, ClientID: "client", ClientSecret: "secret", CallbackURL: "https://hako.example/github/callback", HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			client.oauthURL, client.apiURL = server.URL+"/login/oauth/access_token", server.URL
			if _, err := client.VerifyInstallation(context.Background(), "code", 987, strings.Repeat("v", 43)); err == nil {
				t.Fatal("VerifyInstallation unexpectedly accepted mismatched identity")
			}
		})
	}
}
