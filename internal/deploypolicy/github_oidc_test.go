package deploypolicy

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type oidcRoundTripper func(*http.Request) (*http.Response, error)

func (f oidcRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubVerifierChecksSignatureIssuerAudienceRepositoryRefEnvironmentAndWorkflow(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	var jwksRequests int
	client := &http.Client{Transport: oidcRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != GitHubJWKSURL {
			t.Fatalf("unexpected JWKS URL: %s", r.URL)
		}
		jwksRequests++
		body, _ := json.Marshal(oidcJWKSet{Keys: []oidcJWK{{Kty: "RSA", Use: "sig", Alg: "RS256", Kid: "key-1",
			N: base64.RawURLEncoding.EncodeToString(key.N.Bytes()), E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: ioNopCloser{Reader: strings.NewReader(string(body))}}, nil
	})}
	verifier := NewGitHubVerifier(client, WithOIDCClock(func() time.Time { return now }))
	policy := Policy{Enabled: true, Audience: AWSSTSAudience, AllowedRefs: []string{"refs/heads/main"}, Environments: []string{"production"},
		AllowedWorkflowRefs: []string{"acme/api/.github/workflows/deploy.yml@refs/heads/main"}, RequireHumanApproval: true}
	baseClaims := Claims{Repository: "acme/api", RepositoryID: "1234", RepositoryOwner: "acme", RepositoryOwnerID: "5678", Ref: "refs/heads/main", SHA: strings.Repeat("a", 40), Environment: "production",
		WorkflowRef: "acme/api/.github/workflows/deploy.yml@refs/heads/main", EventName: "workflow_dispatch", RunID: "987", RunAttempt: "1",
		RegisteredClaims: jwt.RegisteredClaims{Issuer: GitHubIssuer, Subject: "repo:acme/api:environment:production", Audience: jwt.ClaimStrings{AWSSTSAudience},
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute))}}
	makeToken := func(claims Claims) string {
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "key-1"
		value, err := token.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	verified, err := verifier.Verify(context.Background(), makeToken(baseClaims), policy, "acme/api", 1234)
	if err != nil || verified.RunID != "987" {
		t.Fatalf("Verify() = %+v, %v", verified, err)
	}
	immutableClaims := baseClaims
	immutableClaims.Subject = "repo:acme@5678/api@1234:environment:production"
	if _, err := verifier.Verify(context.Background(), makeToken(immutableClaims), policy, "acme/api", 1234); err != nil {
		t.Fatalf("Verify() rejected immutable repository-ID subject: %v", err)
	}

	for name, mutate := range map[string]func(*Claims){
		"wrong issuer":                     func(c *Claims) { c.Issuer = "https://evil.example" },
		"wrong audience":                   func(c *Claims) { c.Audience = jwt.ClaimStrings{"other"} },
		"extra audience":                   func(c *Claims) { c.Audience = jwt.ClaimStrings{AWSSTSAudience, "other"} },
		"foreign repository":               func(c *Claims) { c.Repository = "other/api" },
		"foreign repository id":            func(c *Claims) { c.RepositoryID = "1235" },
		"foreign ref":                      func(c *Claims) { c.Ref = "refs/heads/attacker" },
		"foreign environment":              func(c *Claims) { c.Environment = "staging" },
		"foreign workflow":                 func(c *Claims) { c.WorkflowRef = "acme/api/.github/workflows/test.yml@refs/heads/main" },
		"foreign reusable workflow":        func(c *Claims) { c.JobWorkflowRef = "evil/repo/.github/workflows/reusable.yml@refs/heads/main" },
		"pull request event":               func(c *Claims) { c.EventName = "pull_request" },
		"subject mismatch":                 func(c *Claims) { c.Subject = "repo:acme/api:ref:refs/heads/main" },
		"immutable subject owner mismatch": func(c *Claims) { c.Subject = "repo:acme@9999/api@1234:environment:production" },
	} {
		t.Run(name, func(t *testing.T) {
			claims := baseClaims
			mutate(&claims)
			if _, err := verifier.Verify(context.Background(), makeToken(claims), policy, "acme/api", 1234); err == nil {
				t.Fatal("Verify accepted mismatched claims")
			}
		})
	}
	if jwksRequests != 1 {
		t.Fatalf("JWKS requests = %d, want cached single fetch", jwksRequests)
	}
	if _, err := verifier.Verify(context.Background(), makeToken(baseClaims), policy, "acme/api", 9999); err == nil {
		t.Fatal("Verify accepted a different catalog repository id")
	}
	if _, err := verifier.Verify(context.Background(), "not-a-jwt", policy, "acme/api", 1234); err == nil {
		t.Fatal("Verify accepted malformed token")
	}
}

type ioNopCloser struct{ *strings.Reader }

func (r ioNopCloser) Close() error { return nil }

func TestGitHubVerifierDoesNotFollowJWKSRedirects(t *testing.T) {
	var requests int
	client := &http.Client{Transport: oidcRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.String() != GitHubJWKSURL {
			t.Fatalf("verifier followed redirect to %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://attacker.invalid/keys"}}, Body: ioNopCloser{Reader: strings.NewReader("")}}, nil
	})}
	verifier := NewGitHubVerifier(client)
	_, err := verifier.fetchKeys(context.Background())
	if err == nil || requests != 1 {
		t.Fatalf("fetchKeys error = %v, requests=%d; want one non-followed redirect", err, requests)
	}
}
