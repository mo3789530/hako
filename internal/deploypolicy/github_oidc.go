package deploypolicy

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	GitHubIssuer     = "https://token.actions.githubusercontent.com"
	GitHubJWKSURL    = GitHubIssuer + "/.well-known/jwks"
	maxOIDCTokenSize = 64 << 10
	maxOIDCJWKSSize  = 1 << 20
)

var (
	ErrInvalidToken = errors.New("invalid GitHub Actions OIDC token")
	ErrUnavailable  = errors.New("GitHub Actions OIDC verifier unavailable")
)

// Claims contains only GitHub claims used by policy checks. The signed token
// itself is never retained.
type Claims struct {
	Repository        string `json:"repository"`
	RepositoryID      string `json:"repository_id"`
	RepositoryOwner   string `json:"repository_owner"`
	RepositoryOwnerID string `json:"repository_owner_id"`
	Ref               string `json:"ref"`
	SHA               string `json:"sha"`
	Environment       string `json:"environment"`
	WorkflowRef       string `json:"workflow_ref"`
	JobWorkflowRef    string `json:"job_workflow_ref,omitempty"`
	EventName         string `json:"event_name"`
	RunID             string `json:"run_id"`
	RunAttempt        string `json:"run_attempt"`
	jwt.RegisteredClaims
}

type GitHubVerifier struct {
	client   *http.Client
	clock    func() time.Time
	cacheTTL time.Duration

	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	refreshedAt time.Time
	expiresAt   time.Time
}

type oidcJWKSet struct {
	Keys []oidcJWK `json:"keys"`
}

type oidcJWK struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type GitHubVerifierOption func(*GitHubVerifier)

func WithOIDCClock(clock func() time.Time) GitHubVerifierOption {
	return func(v *GitHubVerifier) {
		if clock != nil {
			v.clock = clock
		}
	}
}

func WithOIDCCacheTTL(ttl time.Duration) GitHubVerifierOption {
	return func(v *GitHubVerifier) {
		if ttl > 0 {
			v.cacheTTL = ttl
		}
	}
}

func NewGitHubVerifier(client *http.Client, options ...GitHubVerifierOption) *GitHubVerifier {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	} else {
		copy := *client
		copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &copy
	}
	v := &GitHubVerifier{client: client, clock: time.Now, cacheTTL: 15 * time.Minute, keys: make(map[string]*rsa.PublicKey)}
	for _, option := range options {
		option(v)
	}
	return v
}

// Verify checks the fixed GitHub issuer, RS256 signature, exact audience,
// Tenant Catalog Repository, ref, environment and workflow allowlists. It
// does not exchange the token for AWS credentials or enable deployments.
func (v *GitHubVerifier) Verify(ctx context.Context, token string, policy Policy, repository string, repositoryID int64) (Claims, error) {
	if v == nil || len(token) == 0 || len(token) > maxOIDCTokenSize || policy.Validate() != nil || !policy.Enabled || !validRepository(repository) || repositoryID <= 0 {
		return Claims{}, ErrInvalidToken
	}
	claims := &Claims{}
	keyFetchFailed := false
	parsed, err := jwt.ParseWithClaims(token, claims, func(parsed *jwt.Token) (any, error) {
		if parsed.Method != jwt.SigningMethodRS256 {
			return nil, ErrInvalidToken
		}
		kid, ok := parsed.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, ErrInvalidToken
		}
		key, keyErr := v.publicKey(ctx, kid)
		if errors.Is(keyErr, ErrUnavailable) {
			keyFetchFailed = true
		}
		return key, keyErr
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(GitHubIssuer), jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(), jwt.WithLeeway(30*time.Second), jwt.WithTimeFunc(v.clock))
	if err != nil {
		if keyFetchFailed {
			return Claims{}, fmt.Errorf("%w: signing key fetch failed", ErrUnavailable)
		}
		return Claims{}, ErrInvalidToken
	}
	if parsed == nil || !parsed.Valid || claims.Subject == "" || claims.IssuedAt == nil || claims.ExpiresAt == nil ||
		claims.Repository != repository || claims.RepositoryID != fmt.Sprint(repositoryID) || claims.RepositoryOwner == "" || claims.RepositoryOwnerID == "" ||
		claims.Ref == "" || !validCommitSHA(claims.SHA) || claims.Environment == "" || claims.WorkflowRef == "" || claims.EventName == "" || claims.RunID == "" || claims.RunAttempt == "" ||
		!exactAudience(claims.Audience, policy.Audience) {
		return Claims{}, ErrInvalidToken
	}
	if !contains(policy.AllowedRefs, claims.Ref) || !contains(policy.Environments, claims.Environment) ||
		!contains(policy.AllowedWorkflowRefs, claims.WorkflowRef) ||
		(claims.JobWorkflowRef != "" && !contains(policy.AllowedWorkflowRefs, claims.JobWorkflowRef)) ||
		!validSubject(claims, repository) ||
		(claims.EventName != "push" && claims.EventName != "workflow_dispatch") {
		return Claims{}, ErrInvalidToken
	}
	return *claims, nil
}

func validCommitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func validSubject(claims *Claims, repository string) bool {
	parts := strings.SplitN(repository, "/", 2)
	if len(parts) != 2 || claims.RepositoryOwner != parts[0] {
		return false
	}
	legacy := "repo:" + repository + ":environment:" + claims.Environment
	immutable := "repo:" + parts[0] + "@" + claims.RepositoryOwnerID + "/" + parts[1] + "@" + claims.RepositoryID + ":environment:" + claims.Environment
	return claims.Subject == legacy || claims.Subject == immutable
}

func (v *GitHubVerifier) publicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	now := v.clock()
	v.mu.Lock()
	defer v.mu.Unlock()
	if key, ok := v.keys[kid]; ok && now.Before(v.expiresAt) {
		return key, nil
	}
	if _, cached := v.keys[kid]; !cached && !v.refreshedAt.IsZero() && now.Before(v.refreshedAt.Add(time.Minute)) {
		return nil, ErrInvalidToken
	}
	if !now.Before(v.expiresAt) || v.refreshedAt.IsZero() || now.Sub(v.refreshedAt) >= time.Minute {
		keys, err := v.fetchKeys(ctx)
		if err != nil {
			return nil, fmt.Errorf("%w: fetch signing keys", ErrUnavailable)
		}
		v.keys, v.refreshedAt, v.expiresAt = keys, now, now.Add(v.cacheTTL)
	}
	key, ok := v.keys[kid]
	if !ok {
		return nil, ErrInvalidToken
	}
	return key, nil
}

func (v *GitHubVerifier) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, GitHubJWKSURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOIDCJWKSSize+1))
	if err != nil || len(body) > maxOIDCJWKSSize {
		return nil, errors.New("invalid JWKS response size")
	}
	var set oidcJWKSet
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, err
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, jwk := range set.Keys {
		if jwk.Kty != "RSA" || (jwk.Use != "" && jwk.Use != "sig") || (jwk.Alg != "" && jwk.Alg != "RS256") {
			continue
		}
		if jwk.Kid == "" {
			return nil, errors.New("JWKS key has no kid")
		}
		n, errN := base64.RawURLEncoding.DecodeString(jwk.N)
		e, errE := base64.RawURLEncoding.DecodeString(jwk.E)
		if errN != nil || errE != nil {
			return nil, errors.New("invalid JWKS RSA key")
		}
		modulus := new(big.Int).SetBytes(n)
		exponent := new(big.Int).SetBytes(e)
		if modulus.BitLen() < 2048 || modulus.BitLen() > 8192 || !exponent.IsInt64() || exponent.Int64() < 3 || exponent.Int64() > 1<<31-1 || exponent.Int64()%2 == 0 {
			return nil, errors.New("JWKS RSA key outside supported bounds")
		}
		if _, exists := keys[jwk.Kid]; exists {
			return nil, errors.New("duplicate JWKS key id")
		}
		keys[jwk.Kid] = &rsa.PublicKey{N: modulus, E: int(exponent.Int64())}
	}
	if len(keys) == 0 {
		return nil, errors.New("JWKS has no supported RSA keys")
	}
	return keys, nil
}

func exactAudience(audiences jwt.ClaimStrings, expected string) bool {
	return len(audiences) == 1 && audiences[0] == expected
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func validRepository(repository string) bool {
	parts := strings.Split(repository, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != "" && !strings.ContainsAny(repository, "\\\r\n\x00")
}
