package localidp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/auth"
)

func TestPKCEAuthorizationExchangeAndCognitoVerification(t *testing.T) {
	origin, _, client := startTestServer(t)
	verifier := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.~"
	callback := "http://127.0.0.1:53682/callback"
	state := "state-test-value"
	query := url.Values{
		"response_type": {"code"}, "client_id": {"hako-local-dev"}, "redirect_uri": {callback},
		"scope": {"openid hako/api"}, "state": {state}, "code_challenge_method": {"S256"},
		"code_challenge": {auth.PKCEChallenge(verifier)},
	}
	authorizeURL := origin + "/oauth2/authorize?" + query.Encode()
	response, err := client.Get(authorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("authorize status = %d", response.StatusCode)
	}
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil || location.String() == "" || location.Query().Get("state") != state || location.Query().Get("code") == "" {
		t.Fatalf("invalid authorization redirect: %v %q", err, response.Header.Get("Location"))
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {"hako-local-dev"},
		"redirect_uri": {callback}, "code": {location.Query().Get("code")}, "code_verifier": {verifier}}
	tokenResponse, err := client.Post(origin+"/oauth2/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer tokenResponse.Body.Close()
	if tokenResponse.StatusCode != http.StatusOK {
		t.Fatalf("token status = %d", tokenResponse.StatusCode)
	}
	var tokenSet auth.TokenSet
	if err := json.NewDecoder(tokenResponse.Body).Decode(&tokenSet); err != nil || tokenSet.AccessToken == "" || tokenSet.ExpiresIn != int(tokenTTL.Seconds()) {
		t.Fatalf("token response = %+v, error=%v", tokenSet, err)
	}
	jwtVerifier, err := auth.NewCognitoVerifier(auth.CognitoVerifierConfig{Issuer: origin + "/pool", ClientID: "hako-local-dev", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := jwtVerifier.Verify(context.Background(), tokenSet.AccessToken)
	if err != nil || claims.Subject != "hako-local-user" || claims.Scope != "openid hako/api" {
		t.Fatalf("local access token verification = %+v, %v", claims, err)
	}
	replay, err := client.Post(origin+"/oauth2/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	replay.Body.Close()
	if replay.StatusCode != http.StatusBadRequest {
		t.Fatalf("authorization code replay status = %d", replay.StatusCode)
	}
}

func TestAuthorizeRejectsUnsafeOrAmbiguousRequests(t *testing.T) {
	origin, _, client := startTestServer(t)
	base := url.Values{"response_type": {"code"}, "client_id": {"hako-local-dev"},
		"redirect_uri": {"http://127.0.0.1:53682/callback"}, "scope": {"hako/api"}, "state": {"state"},
		"code_challenge_method": {"S256"}, "code_challenge": {auth.PKCEChallenge(strings.Repeat("a", 43))}}
	for name, mutate := range map[string]func(url.Values){
		"remote callback":     func(values url.Values) { values.Set("redirect_uri", "https://example.com/callback") },
		"missing API scope":   func(values url.Values) { values.Set("scope", "openid profile") },
		"duplicate client id": func(values url.Values) { values.Add("client_id", "other") },
	} {
		t.Run(name, func(t *testing.T) {
			query := url.Values{}
			for key, items := range base {
				query[key] = append([]string(nil), items...)
			}
			mutate(query)
			response, err := client.Get(origin + "/oauth2/authorize?" + query.Encode())
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("unsafe authorize request status = %d", response.StatusCode)
			}
		})
	}
}

func TestNewRequiresLoopbackOrigin(t *testing.T) {
	if _, err := New(Config{Origin: "http://0.0.0.0:8787", ClientID: "client", Subject: "subject"}); err == nil {
		t.Fatal("wildcard bind must be rejected")
	}
	if _, err := New(Config{Origin: "https://127.0.0.1:8787/path", ClientID: "client", Subject: "subject"}); err == nil {
		t.Fatal("issuer origin with path must be rejected")
	}
}

func startTestServer(t *testing.T) (string, *http.Server, *http.Client) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://" + listener.Addr().String()
	provider, err := New(Config{Origin: origin, ClientID: "hako-local-dev", Subject: "hako-local-user"})
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	server := &http.Server{Handler: provider, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return origin, server, client
}
