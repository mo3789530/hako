package api

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/auth"
)

func TestHealthRequiresValidAccessTokenAndHakoScope(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer := ""
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pool/.well-known/jwks.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "api-test-key",
			"n": base64.RawURLEncoding.EncodeToString(privateKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
		}}})
	}))
	defer keys.Close()
	issuer = keys.URL + "/pool"
	verifier, err := auth.NewCognitoVerifier(auth.CognitoVerifierConfig{Issuer: issuer, ClientID: "hako-cli"})
	if err != nil {
		t.Fatalf("create Cognito verifier: %v", err)
	}
	handler := NewHandler(verifier, nil)

	request := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("missing token returned HTTP %d, want 401", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	request.Header.Set("Authorization", "Bearer "+signHealthToken(t, privateKey, issuer, "openid email"))
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("token without hako/api scope returned HTTP %d, want 403", recorder.Code)
	}
	if recorder.Header().Get("WWW-Authenticate") != `Bearer error="insufficient_scope"` {
		t.Fatalf("missing scope response omitted insufficient_scope challenge: %q", recorder.Header().Get("WWW-Authenticate"))
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	request.Header.Set("Authorization", "Bearer "+signHealthToken(t, privateKey, issuer, "openid hako/api"))
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("scoped token returned HTTP %d, want 200: %s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/unknown", nil)
	request.Header.Set("Authorization", "Bearer "+signHealthToken(t, privateKey, issuer, "openid hako/api"))
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound || recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("unknown route returned HTTP %d, want JSON 404: %s", recorder.Code, recorder.Body.String())
	}
	var apiError errorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &apiError); err != nil || apiError.Error.Code != "not_found" {
		t.Fatalf("unexpected API error envelope: %+v, decode error=%v", apiError, err)
	}
}

func TestDecodeJSONRequestRejectsUnknownTrailingAndOversizedBodies(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantTooLarge bool
	}{
		{name: "valid", body: `{"name":"api-dev"}`},
		{name: "unknown field", body: `{"name":"api-dev","admin":true}`},
		{name: "trailing JSON", body: `{"name":"api-dev"} {}`},
		{name: "oversized", body: `{"name":"` + strings.Repeat("x", maxJSONRequestBytes) + `"}`, wantTooLarge: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.body))
			recorder := httptest.NewRecorder()
			ctx := echo.New().NewContext(request, recorder)
			var decoded workspaceCreateRequest
			err := decodeJSONRequest(ctx, &decoded)
			if test.name == "valid" {
				if err != nil || decoded.Name != "api-dev" {
					t.Fatalf("valid request decode = %+v, %v", decoded, err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected malformed request to be rejected")
			}
			if got := isRequestTooLarge(err); got != test.wantTooLarge {
				t.Fatalf("isRequestTooLarge = %t, want %t (err=%v)", got, test.wantTooLarge, err)
			}
		})
	}
}

func TestParseWorkspacePageUsesBoundedDefaultsAndRejectsAmbiguousQuery(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantLimit  int
		wantOffset int
		wantError  bool
	}{
		{name: "defaults", wantLimit: 20},
		{name: "valid", query: "?limit=100&offset=12", wantLimit: 100, wantOffset: 12},
		{name: "duplicate", query: "?limit=2&limit=3", wantError: true},
		{name: "unknown", query: "?page=2", wantError: true},
		{name: "empty", query: "?offset=", wantError: true},
		{name: "upper bound", query: "?offset=1000001", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/tenants/t/workspaces"+test.query, nil)
			ctx := echo.New().NewContext(request, httptest.NewRecorder())
			limit, offset, err := parseWorkspacePage(ctx)
			if (err != nil) != test.wantError || limit != test.wantLimit || offset != test.wantOffset {
				t.Fatalf("parseWorkspacePage = (%d, %d, %v), want (%d, %d, error=%t)", limit, offset, err, test.wantLimit, test.wantOffset, test.wantError)
			}
		})
	}
}

func signHealthToken(t *testing.T, privateKey *rsa.PrivateKey, issuer, scope string) string {
	t.Helper()
	claims := auth.CognitoAccessClaims{
		ClientID: "hako-cli", TokenUse: "access", Scope: scope,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: issuer, Subject: "test-subject", IssuedAt: jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "api-test-key"
	signed, err := token.SignedString(privateKey)
	if err != nil {
		t.Fatalf("sign test token: %v", err)
	}
	return signed
}
