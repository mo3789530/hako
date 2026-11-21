package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestAuthenticateVerifiesAccessTokenAndCachesJWKS(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var jwksRequests atomic.Int32
	issuer := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pool/.well-known/jwks.json" {
			http.NotFound(w, r)
			return
		}
		jwksRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "key-1",
			"n": base64.RawURLEncoding.EncodeToString(privateKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
		}}})
	}))
	defer server.Close()
	issuer = server.URL + "/pool"
	verifier, err := NewCognitoVerifier(CognitoVerifierConfig{Issuer: issuer, ClientID: "client-1"})
	if err != nil {
		t.Fatalf("create verifier: %v", err)
	}

	claims := CognitoAccessClaims{
		ClientID: "client-1", TokenUse: "access", Scope: "openid hako/api",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: issuer, Subject: "user-sub", IssuedAt: jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	validToken := signCognitoToken(t, privateKey, "key-1", claims)
	for i := 0; i < 2; i++ {
		principal, err := Authenticate(context.Background(), "Bearer "+validToken, verifier)
		if err != nil {
			t.Fatalf("authenticate valid access token: %v", err)
		}
		if principal.CognitoSubject != "user-sub" || len(principal.Scopes) != 2 || principal.Scopes[0] != "openid" || principal.Scopes[1] != "hako/api" {
			t.Fatalf("unexpected authenticated principal: %+v", principal)
		}
	}
	if jwksRequests.Load() != 1 {
		t.Fatalf("expected cached JWKS to be fetched once, got %d requests", jwksRequests.Load())
	}

	claims.TokenUse = "id"
	idToken := signCognitoToken(t, privateKey, "key-1", claims)
	if _, err := Authenticate(context.Background(), "Bearer "+idToken, verifier); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("ID token should be rejected, got %v", err)
	}
	if _, err := Authenticate(context.Background(), "Basic invalid", verifier); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("malformed Authorization header should be rejected, got %v", err)
	}
}

func signCognitoToken(t *testing.T, key *rsa.PrivateKey, keyID string, claims CognitoAccessClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = keyID
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign test Cognito token: %v", err)
	}
	return signed
}
