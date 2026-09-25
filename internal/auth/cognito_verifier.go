package auth

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
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	maxJWTSize        = 64 << 10
	maxJWKSSize       = 1 << 20
	defaultJWKSCache  = 15 * time.Minute
	unknownKIDRefresh = time.Minute
	clockSkew         = 30 * time.Second
)

var (
	ErrInvalidToken        = errors.New("invalid Cognito access token")
	ErrVerifierUnavailable = errors.New("Cognito token verifier unavailable")
)

type CognitoVerifierConfig struct {
	Issuer     string
	ClientID   string
	Audience   string
	HTTPClient *http.Client
	CacheTTL   time.Duration
	Clock      func() time.Time
}

type CognitoAccessClaims struct {
	ClientID string   `json:"client_id"`
	TokenUse string   `json:"token_use"`
	Scope    string   `json:"scope"`
	Groups   []string `json:"cognito:groups,omitempty"`
	jwt.RegisteredClaims
}

type Principal struct {
	CognitoSubject string
	Scopes         []string
	Groups         []string
}

type cognitoJWKSet struct {
	Keys []cognitoJWK `json:"keys"`
}

type cognitoJWK struct {
	KeyType  string `json:"kty"`
	Use      string `json:"use"`
	Alg      string `json:"alg"`
	KeyID    string `json:"kid"`
	Modulus  string `json:"n"`
	Exponent string `json:"e"`
}

type CognitoVerifier struct {
	issuer     string
	clientID   string
	audience   string
	jwksURL    string
	httpClient *http.Client
	cacheTTL   time.Duration
	clock      func() time.Time

	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	refreshedAt time.Time
	expiresAt   time.Time
}

func NewCognitoVerifier(config CognitoVerifierConfig) (*CognitoVerifier, error) {
	config.Issuer = strings.TrimRight(strings.TrimSpace(config.Issuer), "/")
	config.ClientID = strings.TrimSpace(config.ClientID)
	config.Audience = strings.TrimSpace(config.Audience)
	issuer, err := url.Parse(config.Issuer)
	if err != nil || issuer.Host == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" || issuer.Path == "" {
		return nil, errors.New("Cognito issuer must be an absolute URL with a user pool path")
	}
	if issuer.Scheme != "https" && !(issuer.Scheme == "http" && isLoopback(issuer.Hostname())) {
		return nil, errors.New("Cognito issuer must use HTTPS")
	}
	if config.ClientID == "" {
		return nil, errors.New("Cognito app client id is required")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	if config.CacheTTL <= 0 {
		config.CacheTTL = defaultJWKSCache
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	return &CognitoVerifier{
		issuer: config.Issuer, clientID: config.ClientID, audience: config.Audience,
		jwksURL: config.Issuer + "/.well-known/jwks.json", httpClient: config.HTTPClient,
		cacheTTL: config.CacheTTL, clock: config.Clock, keys: make(map[string]*rsa.PublicKey),
	}, nil
}

func (v *CognitoVerifier) Verify(ctx context.Context, token string) (CognitoAccessClaims, error) {
	if v == nil || len(token) == 0 || len(token) > maxJWTSize {
		return CognitoAccessClaims{}, ErrInvalidToken
	}
	claims := &CognitoAccessClaims{}
	keyFetchFailed := false
	parsed, err := jwt.ParseWithClaims(token, claims, func(parsed *jwt.Token) (any, error) {
		if parsed.Method != jwt.SigningMethodRS256 {
			return nil, ErrInvalidToken
		}
		keyID, ok := parsed.Header["kid"].(string)
		if !ok || keyID == "" {
			return nil, ErrInvalidToken
		}
		key, keyErr := v.publicKey(ctx, keyID)
		if errors.Is(keyErr, ErrVerifierUnavailable) {
			keyFetchFailed = true
		}
		return key, keyErr
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(v.issuer), jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(), jwt.WithLeeway(clockSkew), jwt.WithTimeFunc(v.clock))
	if err != nil {
		if keyFetchFailed {
			return CognitoAccessClaims{}, fmt.Errorf("%w: signing key fetch failed", ErrVerifierUnavailable)
		}
		return CognitoAccessClaims{}, ErrInvalidToken
	}
	if parsed == nil || !parsed.Valid {
		return CognitoAccessClaims{}, ErrInvalidToken
	}
	if claims.Subject == "" || claims.ClientID != v.clientID || claims.TokenUse != "access" || claims.IssuedAt == nil || claims.ExpiresAt == nil {
		return CognitoAccessClaims{}, ErrInvalidToken
	}
	if v.audience != "" {
		matched := false
		for _, audience := range claims.Audience {
			if audience == v.audience {
				matched = true
				break
			}
		}
		if !matched {
			return CognitoAccessClaims{}, ErrInvalidToken
		}
	}
	if v.audience == "" && len(claims.Audience) != 0 {
		return CognitoAccessClaims{}, ErrInvalidToken
	}
	return *claims, nil
}

func (v *CognitoVerifier) publicKey(ctx context.Context, keyID string) (*rsa.PublicKey, error) {
	now := v.clock()
	v.mu.Lock()
	defer v.mu.Unlock()
	if key, ok := v.keys[keyID]; ok && now.Before(v.expiresAt) {
		return key, nil
	}
	shouldRefresh := !now.Before(v.expiresAt) || v.refreshedAt.IsZero() || now.Sub(v.refreshedAt) >= unknownKIDRefresh
	if shouldRefresh {
		keys, err := v.fetchKeys(ctx)
		if err != nil {
			return nil, fmt.Errorf("%w: fetch Cognito signing keys", ErrVerifierUnavailable)
		}
		v.keys = keys
		v.refreshedAt = now
		v.expiresAt = now.Add(v.cacheTTL)
	}
	key, ok := v.keys[keyID]
	if !ok {
		return nil, ErrInvalidToken
	}
	return key, nil
}

func (v *CognitoVerifier) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS endpoint returned HTTP %d", resp.StatusCode)
	}
	var set cognitoJWKSet
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSSize)).Decode(&set); err != nil {
		return nil, err
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, jwk := range set.Keys {
		if jwk.KeyType != "RSA" || (jwk.Use != "" && jwk.Use != "sig") || (jwk.Alg != "" && jwk.Alg != "RS256") {
			continue
		}
		if jwk.KeyID == "" {
			return nil, errors.New("JWKS contains a signing key without kid")
		}
		modulusBytes, err := base64.RawURLEncoding.DecodeString(jwk.Modulus)
		if err != nil {
			return nil, err
		}
		exponentBytes, err := base64.RawURLEncoding.DecodeString(jwk.Exponent)
		if err != nil {
			return nil, err
		}
		exponent := new(big.Int).SetBytes(exponentBytes)
		if !exponent.IsInt64() || exponent.Int64() < 3 || exponent.Int64() > 1<<31-1 || exponent.Int64()%2 == 0 {
			return nil, errors.New("JWKS contains an invalid RSA exponent")
		}
		modulus := new(big.Int).SetBytes(modulusBytes)
		if modulus.BitLen() < 2048 || modulus.BitLen() > 8192 {
			return nil, errors.New("JWKS contains an RSA modulus outside supported bounds")
		}
		if _, exists := keys[jwk.KeyID]; exists {
			return nil, errors.New("JWKS contains duplicate key ids")
		}
		keys[jwk.KeyID] = &rsa.PublicKey{N: modulus, E: int(exponent.Int64())}
	}
	if len(keys) == 0 {
		return nil, errors.New("JWKS contains no supported RSA signing keys")
	}
	return keys, nil
}

// Authenticate verifies a Bearer Authorization header and returns its trusted
// principal. Missing or malformed credentials return ErrInvalidToken.
func Authenticate(ctx context.Context, authorizationHeader string, verifier *CognitoVerifier) (Principal, error) {
	parts := strings.Fields(authorizationHeader)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return Principal{}, ErrInvalidToken
	}
	claims, err := verifier.Verify(ctx, parts[1])
	if err != nil {
		return Principal{}, err
	}
	principal := Principal{CognitoSubject: claims.Subject, Groups: append([]string(nil), claims.Groups...)}
	if claims.Scope != "" {
		principal.Scopes = strings.Fields(claims.Scope)
	}
	return principal, nil
}
