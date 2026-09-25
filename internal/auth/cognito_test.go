package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

type memoryCredentialStore struct {
	service string
	account string
	secret  string
}

func (s *memoryCredentialStore) Set(service, account, secret string) error {
	s.service, s.account, s.secret = service, account, secret
	return nil
}

func (s *memoryCredentialStore) Get(service, account string) (string, error) {
	if s.service != service || s.account != account || s.secret == "" {
		return "", errors.New("credential not found")
	}
	return s.secret, nil
}

func TestLoginUsesAuthorizationCodePKCEAndStoresTokens(t *testing.T) {
	store := &memoryCredentialStore{}
	var callbackURL string
	expectedChallenge := make(chan string, 1)
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/token" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token exchange request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("client_id") != "public-client" || r.Form.Get("code") != "auth-code" || r.Form.Get("redirect_uri") != callbackURL {
			t.Errorf("unexpected token exchange form: %+v", r.Form)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		verifier := r.Form.Get("code_verifier")
		if len(verifier) < 43 {
			t.Errorf("missing PKCE verifier: %q", verifier)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if challenge := <-expectedChallenge; PKCEChallenge(verifier) != challenge {
			t.Errorf("PKCE challenge and verifier did not match: challenge=%q", challenge)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access","id_token":"identity","refresh_token":"refresh","token_type":"Bearer","expires_in":3600}`))
	}))
	defer issuer.Close()

	callbackURL = reserveCallbackURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var authorizationURL string
	err := Login(ctx, Config{
		Domain: issuer.URL, ClientID: "public-client", CallbackURL: callbackURL,
		Timeout: 4 * time.Second,
	}, store, func(raw string) error {
		authorizationURL = raw
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		query := u.Query()
		if query.Get("response_type") != "code" || query.Get("code_challenge_method") != "S256" || query.Get("client_id") != "public-client" {
			t.Errorf("unexpected authorize request: %s", raw)
		}
		if got, want := strings.Fields(query.Get("scope")), []string{"openid", "email", "profile", HakoAPIScope}; !reflect.DeepEqual(got, want) {
			t.Errorf("authorization scopes = %v, want %v", got, want)
		}
		state := query.Get("state")
		challenge := query.Get("code_challenge")
		if state == "" || challenge == "" {
			t.Errorf("authorize request omitted state or PKCE challenge: %s", raw)
		}
		expectedChallenge <- challenge
		callback, err := url.Parse(callbackURL)
		if err != nil {
			return err
		}
		callback.RawQuery = url.Values{"code": {"auth-code"}, "state": {state}}.Encode()
		response, err := http.Get(callback.String())
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("OAuth callback returned HTTP %d", response.StatusCode)
		}
		return nil
	}, func(string) {})
	if err != nil {
		t.Fatalf("Login returned error: %v", err)
	}
	if !strings.Contains(authorizationURL, "/oauth2/authorize?") {
		t.Fatalf("unexpected authorization URL: %q", authorizationURL)
	}
	if store.service != KeyringService || store.account != KeyringAccount || store.secret == "" {
		t.Fatalf("tokens were not stored in the expected credential store entry: %+v", store)
	}
	var tokens TokenSet
	if err := json.Unmarshal([]byte(store.secret), &tokens); err != nil {
		t.Fatalf("decode stored tokens: %v", err)
	}
	if tokens.AccessToken != "access" || tokens.IDToken != "identity" || tokens.RefreshToken != "refresh" || tokens.ExpiresIn != 3600 || tokens.StoredAt.IsZero() {
		t.Fatalf("unexpected stored tokens: %+v", tokens)
	}
	accessToken, err := LoadAccessToken(store, tokens.StoredAt.Add(time.Minute))
	if err != nil || accessToken != tokens.AccessToken {
		t.Fatalf("load stored access token = %q, %v", accessToken, err)
	}
	if _, err := LoadAccessToken(store, tokens.StoredAt.Add(2*time.Hour)); err == nil {
		t.Fatal("expired access token should require a new login")
	}
}

func TestPKCEChallengeIsS256(t *testing.T) {
	verifier := strings.Repeat("a", 43)
	challenge := PKCEChallenge(verifier)
	if len(challenge) != 43 || strings.ContainsAny(challenge, "+/=") {
		t.Fatalf("challenge is not base64url encoded: %q", challenge)
	}
}

func TestCallbackRejectsIncorrectState(t *testing.T) {
	results := make(chan callbackResult, 1)
	handler := callbackHandler("/callback", "expected-state", results)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/callback?code=secret&state=wrong", nil)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for invalid state, got %d", recorder.Code)
	}
	select {
	case <-results:
		t.Fatal("invalid OAuth state must not be accepted")
	default:
	}
}

func reserveCallbackURL(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("http://127.0.0.1:%d/callback", port)
}
