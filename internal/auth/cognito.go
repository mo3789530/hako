// Package auth implements Hako CLI authentication.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	KeyringService       = "hako-cli"
	KeyringAccount       = "default"
	HakoAPIScope         = "hako/api"
	DefaultCallbackURL   = "http://127.0.0.1:53682/callback"
	DefaultLoginTimeout  = 5 * time.Minute
	maxTokenResponseSize = 1 << 20
)

type Config struct {
	Domain      string
	ClientID    string
	CallbackURL string
	Scopes      []string
	HTTPClient  *http.Client
	Timeout     time.Duration
}

type CredentialStore interface {
	Set(service, account, secret string) error
}

type CredentialGetter interface {
	Get(service, account string) (string, error)
}

type BrowserOpener func(string) error
type URLAnnouncer func(string)

type TokenSet struct {
	AccessToken  string    `json:"access_token"`
	IDToken      string    `json:"id_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type"`
	ExpiresIn    int       `json:"expires_in"`
	StoredAt     time.Time `json:"stored_at"`
}

// LoadAccessToken retrieves the stored access token if it is still within its
// advertised lifetime. Tokens are never included in returned errors.
func LoadAccessToken(store CredentialGetter, now time.Time) (string, error) {
	if store == nil {
		return "", errors.New("credential store is required")
	}
	encoded, err := store.Get(KeyringService, KeyringAccount)
	if err != nil {
		return "", errors.New("no saved Hako login; run hako login")
	}
	var tokens TokenSet
	if err := json.Unmarshal([]byte(encoded), &tokens); err != nil {
		return "", errors.New("saved Hako login is invalid; run hako login again")
	}
	if tokens.AccessToken == "" || tokens.ExpiresIn <= 0 || tokens.ExpiresIn > 86400 || tokens.StoredAt.IsZero() {
		return "", errors.New("saved Hako login is incomplete; run hako login again")
	}
	if now.IsZero() {
		now = time.Now()
	}
	if !now.Before(tokens.StoredAt.Add(time.Duration(tokens.ExpiresIn) * time.Second)) {
		return "", errors.New("saved Hako access token expired; run hako login again")
	}
	return tokens.AccessToken, nil
}

type callbackResult struct {
	code string
	err  error
}

// Login runs the OAuth 2.0 authorization-code flow with PKCE and stores the
// resulting tokens in the caller's credential store. It never uses a client
// secret; Hako's Cognito app client is a public native client.
func Login(ctx context.Context, config Config, store CredentialStore, openBrowser BrowserOpener, announceURL URLAnnouncer) error {
	config, err := normalizeConfig(config)
	if err != nil {
		return err
	}
	if store == nil {
		return errors.New("credential store is required")
	}
	if openBrowser == nil {
		return errors.New("browser opener is required")
	}

	listener, callbackURL, err := listenCallback(config.CallbackURL)
	if err != nil {
		return err
	}
	defer listener.Close()

	state, err := randomURLSafe(32)
	if err != nil {
		return fmt.Errorf("generate OAuth state: %w", err)
	}
	verifier, err := randomURLSafe(32)
	if err != nil {
		return fmt.Errorf("generate PKCE verifier: %w", err)
	}
	challenge := PKCEChallenge(verifier)
	callbackResults := make(chan callbackResult, 1)
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler:           callbackHandler(callbackURL.Path, state, callbackResults),
	}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	authorizeURL := buildAuthorizeURL(config, callbackURL, state, challenge)
	if announceURL != nil {
		announceURL(authorizeURL)
	}
	if err := openBrowser(authorizeURL); err != nil {
		// The URL has already been announced so the user can open it manually.
	}

	loginCtx := ctx
	if config.Timeout > 0 {
		var cancel context.CancelFunc
		loginCtx, cancel = context.WithTimeout(ctx, config.Timeout)
		defer cancel()
	} else {
		var cancel context.CancelFunc
		loginCtx, cancel = context.WithTimeout(ctx, DefaultLoginTimeout)
		defer cancel()
	}
	var result callbackResult
	select {
	case result = <-callbackResults:
	case err := <-serveErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve OAuth callback: %w", err)
		}
		return errors.New("OAuth callback server stopped before login completed")
	case <-loginCtx.Done():
		return fmt.Errorf("waiting for OAuth callback: %w", loginCtx.Err())
	}
	if result.err != nil {
		return result.err
	}

	tokens, err := exchangeCode(loginCtx, config, callbackURL.String(), result.code, verifier)
	if err != nil {
		return err
	}
	tokens.StoredAt = time.Now().UTC()
	encoded, err := json.Marshal(tokens)
	if err != nil {
		return fmt.Errorf("encode authentication session: %w", err)
	}
	if err := store.Set(KeyringService, KeyringAccount, string(encoded)); err != nil {
		return fmt.Errorf("save authentication session to the OS credential store: %w", err)
	}
	return nil
}

// PKCEChallenge returns the S256 challenge for a code verifier.
func PKCEChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func normalizeConfig(config Config) (Config, error) {
	config.Domain = strings.TrimRight(strings.TrimSpace(config.Domain), "/")
	config.ClientID = strings.TrimSpace(config.ClientID)
	if config.Domain == "" || config.ClientID == "" {
		return Config{}, errors.New("Cognito domain and public app client id are required")
	}
	domainURL, err := url.Parse(config.Domain)
	if err != nil || domainURL.Host == "" || domainURL.User != nil || domainURL.RawQuery != "" || domainURL.Fragment != "" || (domainURL.Path != "" && domainURL.Path != "/") {
		return Config{}, errors.New("Cognito domain must be an origin URL without a path, query, or fragment")
	}
	if domainURL.Scheme != "https" && !(domainURL.Scheme == "http" && isLoopback(domainURL.Hostname())) {
		return Config{}, errors.New("Cognito domain must use HTTPS")
	}
	if config.CallbackURL == "" {
		config.CallbackURL = DefaultCallbackURL
	}
	if len(config.Scopes) == 0 {
		config.Scopes = []string{"openid", "email", "profile", HakoAPIScope}
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return config, nil
}

func listenCallback(callback string) (net.Listener, *url.URL, error) {
	parsed, err := url.Parse(callback)
	if err != nil || parsed.Scheme != "http" || !isLoopback(parsed.Hostname()) || parsed.Port() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "/callback" {
		return nil, nil, errors.New("OAuth callback must be a registered loopback URL such as http://127.0.0.1:53682/callback")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, nil, errors.New("OAuth callback URL must include a valid TCP port")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", parsed.Port()))
	if err != nil {
		return nil, nil, fmt.Errorf("listen on OAuth callback port %s: %w", parsed.Port(), err)
	}
	return listener, parsed, nil
}

func callbackHandler(path, expectedState string, results chan<- callbackResult) http.Handler {
	var accepted atomic.Bool
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(expectedState)) != 1 {
			http.Error(w, "OAuth state did not match; close this page and retry login", http.StatusBadRequest)
			return
		}
		if !accepted.CompareAndSwap(false, true) {
			http.Error(w, "OAuth callback was already received", http.StatusConflict)
			return
		}
		if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
			results <- callbackResult{err: fmt.Errorf("Cognito authorization failed: %s", safeOAuthError(oauthErr))}
			http.Error(w, "Login failed. Return to the Hako CLI for details.", http.StatusBadRequest)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			results <- callbackResult{err: errors.New("Cognito callback did not include an authorization code")}
			http.Error(w, "Login failed. Return to the Hako CLI for details.", http.StatusBadRequest)
			return
		}
		results <- callbackResult{code: code}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "Hako login complete. You can close this tab and return to the CLI.\n")
	})
}

func buildAuthorizeURL(config Config, callback *url.URL, state, challenge string) string {
	endpoint := strings.TrimRight(config.Domain, "/") + "/oauth2/authorize"
	u, _ := url.Parse(endpoint)
	query := u.Query()
	query.Set("response_type", "code")
	query.Set("client_id", config.ClientID)
	query.Set("redirect_uri", callback.String())
	query.Set("scope", strings.Join(config.Scopes, " "))
	query.Set("state", state)
	query.Set("code_challenge_method", "S256")
	query.Set("code_challenge", challenge)
	u.RawQuery = query.Encode()
	return u.String()
}

func exchangeCode(ctx context.Context, config Config, callbackURL, code, verifier string) (TokenSet, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {config.ClientID},
		"code":          {code},
		"redirect_uri":  {callbackURL},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(config.Domain, "/")+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return TokenSet{}, fmt.Errorf("build Cognito token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := config.HTTPClient.Do(req)
	if err != nil {
		return TokenSet{}, fmt.Errorf("exchange Cognito authorization code: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return TokenSet{}, fmt.Errorf("Cognito token endpoint returned HTTP %d", resp.StatusCode)
	}
	var tokens TokenSet
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenResponseSize)).Decode(&tokens); err != nil {
		return TokenSet{}, errors.New("Cognito returned an invalid token response")
	}
	if tokens.AccessToken == "" || !strings.EqualFold(tokens.TokenType, "Bearer") || tokens.ExpiresIn <= 0 {
		return TokenSet{}, errors.New("Cognito token response is missing required access token fields")
	}
	return tokens, nil
}

func randomURLSafe(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func safeOAuthError(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown error"
	}
	if len(value) > 128 {
		value = value[:128]
	}
	for _, r := range value {
		if r < 0x20 || r > 0x7e {
			return "invalid error response"
		}
	}
	return value
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
