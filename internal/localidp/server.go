// Package localidp implements a loopback-only, single-subject OAuth provider
// for local Hako development. It is not an authentication service for shared
// or deployed environments.
package localidp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	maxFormBytes = 16 << 10
	grantTTL     = time.Minute
	tokenTTL     = time.Hour
)

type Config struct {
	Origin   string
	ClientID string
	Subject  string
	Now      func() time.Time
	Key      *rsa.PrivateKey
}

type Server struct {
	origin   string
	issuer   string
	clientID string
	subject  string
	now      func() time.Time
	key      *rsa.PrivateKey
	mu       sync.Mutex
	grants   map[string]grant
}

type grant struct {
	redirectURI string
	challenge   string
	scope       string
	expiresAt   time.Time
}

type tokenClaims struct {
	ClientID string   `json:"client_id"`
	TokenUse string   `json:"token_use"`
	Scope    string   `json:"scope"`
	Groups   []string `json:"cognito:groups,omitempty"`
	jwt.RegisteredClaims
}

func New(config Config) (*Server, error) {
	config.Origin = strings.TrimRight(strings.TrimSpace(config.Origin), "/")
	config.ClientID = strings.TrimSpace(config.ClientID)
	config.Subject = strings.TrimSpace(config.Subject)
	origin, err := url.Parse(config.Origin)
	if err != nil || origin.Scheme != "http" || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || !isLoopback(origin.Hostname()) {
		return nil, errors.New("local identity provider origin must be an HTTP loopback origin")
	}
	if config.ClientID == "" || len(config.ClientID) > 128 || config.Subject == "" || len(config.Subject) > 128 || strings.ContainsAny(config.Subject, "\r\n\x00") {
		return nil, errors.New("local identity provider client and subject are required and bounded")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Key == nil {
		config.Key, err = rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, errors.New("generate local identity signing key")
		}
	}
	if config.Key.N == nil || config.Key.N.BitLen() < 2048 || config.Key.Validate() != nil {
		return nil, errors.New("local identity signing key is invalid")
	}
	return &Server{origin: config.Origin, issuer: config.Origin + "/pool", clientID: config.ClientID,
		subject: config.Subject, now: config.Now, key: config.Key, grants: make(map[string]grant)}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s == nil || r == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if !isLoopback(remoteHost(r.RemoteAddr)) {
		writeError(w, http.StatusForbidden, "loopback_only")
		return
	}
	switch {
	case r.URL.Path == "/pool/.well-known/jwks.json" && r.Method == http.MethodGet:
		s.serveJWKS(w)
	case r.URL.Path == "/oauth2/authorize" && r.Method == http.MethodGet:
		s.authorize(w, r)
	case r.URL.Path == "/oauth2/token" && r.Method == http.MethodPost:
		s.token(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found")
	}
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	redirectURI := query.Get("redirect_uri")
	if !singleValues(query, "response_type", "client_id", "redirect_uri", "code_challenge_method", "code_challenge", "state", "scope") || query.Get("response_type") != "code" || query.Get("client_id") != s.clientID || query.Get("code_challenge_method") != "S256" || query.Get("state") == "" || len(query.Get("state")) > 256 || !validChallenge(query.Get("code_challenge")) || !validCallback(redirectURI) {
		writeError(w, http.StatusBadRequest, "invalid_authorization_request")
		return
	}
	scope := strings.Join(strings.Fields(query.Get("scope")), " ")
	if !hasScope(scope, "hako/api") {
		writeError(w, http.StatusBadRequest, "required_scope_missing")
		return
	}
	code, err := randomValue(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "authorization_failed")
		return
	}
	now := s.now().UTC()
	s.mu.Lock()
	for existing, value := range s.grants {
		if !now.Before(value.expiresAt) {
			delete(s.grants, existing)
		}
	}
	if len(s.grants) >= 4096 {
		s.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "authorization_capacity")
		return
	}
	s.grants[code] = grant{redirectURI: redirectURI, challenge: query.Get("code_challenge"), scope: scope, expiresAt: now.Add(grantTTL)}
	s.mu.Unlock()
	callback, _ := url.Parse(redirectURI)
	callbackQuery := callback.Query()
	callbackQuery.Set("code", code)
	callbackQuery.Set("state", query.Get("state"))
	callback.RawQuery = callbackQuery.Encode()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, callback.String(), http.StatusFound)
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.URL.RawQuery != "" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil || !singleValues(r.PostForm, "grant_type", "client_id", "code", "redirect_uri", "code_verifier") || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("client_id") != s.clientID || len(r.PostForm.Get("code_verifier")) < 43 || len(r.PostForm.Get("code_verifier")) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_token_request")
		return
	}
	code := r.PostForm.Get("code")
	now := s.now().UTC()
	s.mu.Lock()
	grantValue, exists := s.grants[code]
	if exists && now.Before(grantValue.expiresAt) && grantValue.redirectURI == r.PostForm.Get("redirect_uri") && constantEqual(grantValue.challenge, challenge(r.PostForm.Get("code_verifier"))) {
		delete(s.grants, code)
	} else {
		exists = false
		if grantValue.expiresAt.IsZero() || !now.Before(grantValue.expiresAt) {
			delete(s.grants, code)
		}
	}
	s.mu.Unlock()
	if !exists {
		writeError(w, http.StatusBadRequest, "invalid_or_expired_code")
		return
	}
	issuedAt := now
	claims := tokenClaims{ClientID: s.clientID, TokenUse: "access", Scope: grantValue.scope,
		RegisteredClaims: jwt.RegisteredClaims{Issuer: s.issuer, Subject: s.subject, IssuedAt: jwt.NewNumericDate(issuedAt), ExpiresAt: jwt.NewNumericDate(issuedAt.Add(tokenTTL))}}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "local-development"
	signed, err := token.SignedString(s.key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "token_signing_failed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": signed, "token_type": "Bearer", "expires_in": int(tokenTTL.Seconds())})
}

func (s *Server) serveJWKS(w http.ResponseWriter) {
	modulus := base64.RawURLEncoding.EncodeToString(s.key.PublicKey.N.Bytes())
	exponent := big.NewInt(int64(s.key.PublicKey.E)).Bytes()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "local-development", "n": modulus, "e": base64.RawURLEncoding.EncodeToString(exponent)}}})
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}

func validCallback(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "http" && parsed.Host != "" && parsed.Port() != "" && isLoopback(parsed.Hostname()) && parsed.User == nil && parsed.Path == "/callback" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func singleValues(values url.Values, names ...string) bool {
	for _, name := range names {
		if len(values[name]) != 1 {
			return false
		}
	}
	return true
}

func validChallenge(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && len(value) == 43
}

func challenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func hasScope(scopes, required string) bool {
	for _, scope := range strings.Fields(scopes) {
		if scope == required {
			return true
		}
	}
	return false
}

func randomValue(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func constantEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	var diff byte
	for i := range left {
		diff |= left[i] ^ right[i]
	}
	return diff == 0
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":"`+code+`"}`)
}
