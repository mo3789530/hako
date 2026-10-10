package githubapp

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/golang-jwt/jwt/v5"
	"github.com/mo3789530/hako/internal/domain"
)

// CredentialError deliberately excludes upstream errors, response bodies and
// request headers. Code, Status and Retryable are safe for operational logging.
type CredentialError struct {
	Code      string
	Status    int
	Retryable bool
	RetryAt   time.Time
}

func (e *CredentialError) Error() string {
	return fmt.Sprintf("GitHub credentials: %s (HTTP %d)", e.Code, e.Status)
}

func credentialError(code string) error { return &CredentialError{Code: code} }

// RepositoryAccess is supplied by trusted CP authorization, never a webhook's
// owner/name or a caller's claimed Installation binding.
type RepositoryAccess struct {
	Owner string
	Name  string
}

type RepositoryAuthorizer interface {
	AuthorizeRepository(context.Context, domain.TenantID, int64, int64) (RepositoryAccess, error)
}

type TenantAppIdentity struct {
	TenantID             domain.TenantID
	IntegrationID        string
	GitHubAppID          int64
	ConfigRevision       int64
	CredentialGeneration int64
}

// TenantAppAuthorizer rechecks the active integration revision/generation on
// each credential use, including before returning a cached installation token.
type TenantAppAuthorizer interface {
	AuthorizeTenantApp(context.Context, TenantAppIdentity) error
}

// ErrRepositoryAuthorizationUnavailable distinguishes a transient CP store
// failure from a denied binding without carrying database or secret details.
var ErrRepositoryAuthorizationUnavailable = errors.New("GitHub repository authorization unavailable")

type SecretValueAPI interface {
	GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

type TokenConfig struct {
	AppID                     int64
	PrivateKeySecretARN       string
	PrivateKeySecretVersionID string
	TenantID                  domain.TenantID
	IntegrationID             string
	ConfigRevision            int64
	CredentialGeneration      int64
}

// TokenConfigFromEnv is for the trusted credential service, not API startup or
// Job environments. The OAuth client secret is a different credential.
func TokenConfigFromEnv(getenv func(string) string) (TokenConfig, error) {
	if getenv == nil {
		return TokenConfig{}, credentialError("invalid_configuration")
	}
	id, err := strconv.ParseInt(strings.TrimSpace(getenv("HAKO_GITHUB_APP_ID")), 10, 64)
	config := TokenConfig{AppID: id, PrivateKeySecretARN: strings.TrimSpace(getenv("HAKO_GITHUB_APP_PRIVATE_KEY_SECRET_ARN"))}
	if err != nil || !config.valid() {
		return TokenConfig{}, credentialError("invalid_configuration")
	}
	return config, nil
}

func (c TokenConfig) valid() bool {
	a, err := arn.Parse(c.PrivateKeySecretARN)
	if err != nil || c.AppID <= 0 || a.Service != "secretsmanager" || a.Region == "" || len(a.AccountID) != 12 || !strings.HasPrefix(a.Resource, "secret:") || len(a.Resource) <= len("secret:") {
		return false
	}
	for _, r := range a.AccountID {
		if r < '0' || r > '9' {
			return false
		}
	}
	if strings.ContainsAny(c.PrivateKeySecretARN, "*?\r\n") || (c.PrivateKeySecretVersionID != "" && !validSecretVersionID(c.PrivateKeySecretVersionID)) {
		return false
	}
	scoped := c.TenantID != "" || c.IntegrationID != "" || c.ConfigRevision != 0 || c.CredentialGeneration != 0
	if !scoped {
		return true
	}
	return validInternalID(string(c.TenantID)) && validInternalID(c.IntegrationID) && c.ConfigRevision > 0 && c.CredentialGeneration > 0 && c.PrivateKeySecretVersionID != ""
}

// InstallationToken is intentionally opaque and redacts common formatting and
// JSON paths. Value must only be passed to the trusted checkout HTTP boundary.
type InstallationToken struct {
	value     string
	expiresAt time.Time
}

func (t InstallationToken) Value() string                  { return t.value }
func (t InstallationToken) ExpiresAt() time.Time           { return t.expiresAt }
func (InstallationToken) String() string                   { return "[REDACTED GitHub installation token]" }
func (t InstallationToken) GoString() string               { return t.String() }
func (t InstallationToken) Format(state fmt.State, _ rune) { _, _ = fmt.Fprint(state, t.String()) }
func (InstallationToken) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED GitHub installation token]"`), nil
}

type tokenCacheKey struct {
	tenant                   domain.TenantID
	integration              string
	appID, configRevision    int64
	credentialGeneration     int64
	installation, repository int64
}

// TokenClient runs only in a trusted credential service. It does not expose a
// public token API. Cache and minting are serialized with a context-aware gate;
// queue waits and all retries share a bounded request deadline.
type TokenClient struct {
	config        TokenConfig
	secrets       SecretValueAPI
	authorizer    RepositoryAuthorizer
	tenantAppAuth TenantAppAuthorizer
	http          *http.Client
	apiURL        string
	now           func() time.Time
	wait          func(context.Context, time.Duration) error
	gate          chan struct{}
	cache         map[tokenCacheKey]InstallationToken
	backoffUntil  time.Time
	lastMutation  time.Time
}

// VerifyApp proves that the configured private key belongs to the configured
// App ID. Tenant-owned setup must call this outside its database transaction,
// then recheck the integration revision before persisting verification.
func (c *TokenClient) VerifyApp(ctx context.Context) error {
	if c == nil {
		return credentialError("invalid_configuration")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.gate }()
	appJWT, err := c.appJWT(ctx)
	if err != nil {
		return err
	}
	var app struct {
		ID int64 `json:"id"`
	}
	if err := c.request(ctx, http.MethodGet, "/app", appJWT, nil, &app); err != nil {
		return err
	}
	if app.ID != c.config.AppID {
		return credentialError("app_id_mismatch")
	}
	return nil
}

func NewTokenClient(config TokenConfig, secrets SecretValueAPI, authorizer RepositoryAuthorizer, httpClient *http.Client) (*TokenClient, error) {
	if authorizer == nil {
		return nil, credentialError("invalid_configuration")
	}
	tokenClient, err := newTokenClient(config, secrets, httpClient)
	if err != nil {
		return nil, err
	}
	tokenClient.authorizer = authorizer
	if config.IntegrationID != "" {
		tenantAppAuth, ok := authorizer.(TenantAppAuthorizer)
		if !ok {
			return nil, credentialError("invalid_configuration")
		}
		tokenClient.tenantAppAuth = tenantAppAuth
	}
	return tokenClient, nil
}

func newTokenClient(config TokenConfig, secrets SecretValueAPI, httpClient *http.Client) (*TokenClient, error) {
	if !config.valid() || secrets == nil {
		return nil, credentialError("invalid_configuration")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if httpClient != nil {
		*client = *httpClient
	}
	if client.Timeout <= 0 || client.Timeout > 10*time.Second {
		client.Timeout = 10 * time.Second
	}
	// Even same-host redirects may redirect token minting to an unintended path.
	// Never follow any redirect while sending credentials.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &TokenClient{config: config, secrets: secrets, http: client, apiURL: "https://api.github.com", now: time.Now, wait: waitTokenRetry, gate: make(chan struct{}, 1), cache: make(map[tokenCacheKey]InstallationToken)}, nil
}

// NewTenantAppTokenClient composes the database-resolved Tenant identity and
// the pinned Secret provider. access must come from the trusted active-App
// resolver; it is never accepted as a user/API supplied value.
func NewTenantAppTokenClient(access TenantPrivateKeyAccess, assumer STSAssumeRoleAPI, clients AssumedSecretClientFactory, authorizer RepositoryAuthorizer, httpClient *http.Client) (*TokenClient, error) {
	secretAPI, err := NewTenantPrivateKeySecretAPI(access, assumer, clients)
	if err != nil {
		return nil, err
	}
	return newTenantAppTokenClient(access, secretAPI, authorizer, httpClient)
}

func NewTenantAppTokenClientFromConfig(access TenantPrivateKeyAccess, config aws.Config, authorizer RepositoryAuthorizer, httpClient *http.Client) (*TokenClient, error) {
	secretAPI, err := NewTenantPrivateKeySecretAPIFromConfig(access, config)
	if err != nil {
		return nil, err
	}
	return newTenantAppTokenClient(access, secretAPI, authorizer, httpClient)
}

func newTenantAppTokenClient(access TenantPrivateKeyAccess, secrets SecretValueAPI, authorizer RepositoryAuthorizer, httpClient *http.Client) (*TokenClient, error) {
	return NewTokenClient(TokenConfig{
		AppID:                     access.GitHubAppID,
		PrivateKeySecretARN:       access.SecretARN,
		PrivateKeySecretVersionID: access.SecretVersionID,
		TenantID:                  domain.TenantID(access.TenantID),
		IntegrationID:             access.IntegrationID,
		ConfigRevision:            access.ConfigRevision,
		CredentialGeneration:      access.CredentialGeneration,
	}, secrets, authorizer, httpClient)
}

// TenantAppProofClient exposes only the draft App identity proof operation.
// It cannot mint Installation tokens and accepts only resolver-derived pins.
type TenantAppProofClient struct{ client *TokenClient }

func NewTenantAppProofClientFromConfig(access TenantPrivateKeyAccess, config aws.Config, httpClient *http.Client) (*TenantAppProofClient, error) {
	secretAPI, err := NewTenantPrivateKeySecretAPIFromConfig(access, config)
	if err != nil {
		return nil, err
	}
	client, err := newTokenClient(TokenConfig{AppID: access.GitHubAppID, PrivateKeySecretARN: access.SecretARN, PrivateKeySecretVersionID: access.SecretVersionID}, secretAPI, httpClient)
	if err != nil {
		return nil, err
	}
	return &TenantAppProofClient{client: client}, nil
}

func (c *TenantAppProofClient) VerifyApp(ctx context.Context) error {
	if c == nil || c.client == nil {
		return credentialError("invalid_configuration")
	}
	return c.client.VerifyApp(ctx)
}

func (c *TokenClient) CheckoutToken(ctx context.Context, tenant domain.TenantID, installationID, repositoryID int64) (InstallationToken, error) {
	if c == nil || c.authorizer == nil || tenant == "" || installationID <= 0 || repositoryID <= 0 {
		return InstallationToken{}, credentialError("invalid_request")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return InstallationToken{}, err
	}
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return InstallationToken{}, ctx.Err()
	}
	defer func() { <-c.gate }()
	key := tokenCacheKey{tenant: tenant, integration: c.config.IntegrationID, appID: c.config.AppID, configRevision: c.config.ConfigRevision, credentialGeneration: c.config.CredentialGeneration, installation: installationID, repository: repositoryID}
	result, err := c.checkoutToken(ctx, key)
	if err != nil {
		delete(c.cache, key)
	}
	return result, err
}

// CheckoutRunToken enforces the durable integration/revision/generation pin
// captured when the Workload Run was created. Shared-mode Runs continue to
// use CheckoutToken through the legacy provider contract.
func (c *TokenClient) CheckoutRunToken(ctx context.Context, run domain.WorkloadRun) (string, error) {
	if c == nil || run.GitHubIntegrationID == "" || c.config.IntegrationID != run.GitHubIntegrationID ||
		c.config.TenantID != run.TenantID || c.config.ConfigRevision != run.GitHubConfigRevision ||
		c.config.CredentialGeneration != run.GitHubCredentialGeneration {
		return "", credentialError("run_credential_identity_mismatch")
	}
	token, err := c.CheckoutToken(ctx, run.TenantID, run.GitHubInstallation, run.GitHubRepository)
	if err != nil {
		return "", err
	}
	return token.Value(), nil
}

func (c *TokenClient) checkoutToken(ctx context.Context, key tokenCacheKey) (InstallationToken, error) {
	access, err := c.authorize(ctx, key)
	if err != nil {
		return InstallationToken{}, err
	}
	appJWT, err := c.appJWT(ctx)
	if err != nil {
		return InstallationToken{}, err
	}
	var installation struct {
		ID          int64      `json:"id"`
		AppID       int64      `json:"app_id"`
		SuspendedAt *time.Time `json:"suspended_at"`
	}
	if err := c.request(ctx, http.MethodGet, fmt.Sprintf("/app/installations/%d", key.installation), appJWT, nil, &installation); err != nil {
		return InstallationToken{}, err
	}
	if installation.ID != key.installation || installation.AppID != c.config.AppID || installation.SuspendedAt != nil {
		return InstallationToken{}, credentialError("installation_unavailable")
	}
	var repositoryInstallation struct {
		ID          int64      `json:"id"`
		AppID       int64      `json:"app_id"`
		SuspendedAt *time.Time `json:"suspended_at"`
	}
	path := "/repos/" + url.PathEscape(access.Owner) + "/" + url.PathEscape(access.Name) + "/installation"
	if err := c.request(ctx, http.MethodGet, path, appJWT, nil, &repositoryInstallation); err != nil {
		return InstallationToken{}, err
	}
	if repositoryInstallation.ID != key.installation || repositoryInstallation.AppID != c.config.AppID || repositoryInstallation.SuspendedAt != nil {
		return InstallationToken{}, credentialError("repository_unavailable")
	}
	if token, ok := c.cache[key]; ok && token.expiresAt.After(c.now().Add(2*time.Minute)) {
		if _, err := c.authorize(ctx, key); err != nil {
			return InstallationToken{}, err
		}
		return token, nil
	}
	body, _ := json.Marshal(map[string]any{"repository_ids": []int64{key.repository}, "permissions": map[string]string{"contents": "read"}})
	var response struct {
		Token        string            `json:"token"`
		ExpiresAt    time.Time         `json:"expires_at"`
		Permissions  map[string]string `json:"permissions"`
		Repositories []struct {
			ID int64 `json:"id"`
		} `json:"repositories"`
	}
	if err := c.request(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", key.installation), appJWT, body, &response); err != nil {
		return InstallationToken{}, err
	}
	if response.Token == "" || len(response.Token) > 16384 || strings.ContainsAny(response.Token, "\r\n\x00 ") || !response.ExpiresAt.After(c.now().Add(2*time.Minute)) || response.ExpiresAt.After(c.now().Add(65*time.Minute)) || response.Permissions["contents"] != "read" || len(response.Repositories) != 1 || response.Repositories[0].ID != key.repository {
		return InstallationToken{}, credentialError("invalid_token_response")
	}
	for permission, level := range response.Permissions {
		if (permission != "contents" && permission != "metadata") || level != "read" {
			return InstallationToken{}, credentialError("invalid_token_scope")
		}
	}
	// Authorization can change while GitHub/Secrets Manager calls are in flight.
	if _, err := c.authorize(ctx, key); err != nil {
		return InstallationToken{}, err
	}
	token := InstallationToken{value: response.Token, expiresAt: response.ExpiresAt}
	if len(c.cache) >= 128 { // bounded memory; do not persist eviction data
		var oldest tokenCacheKey
		var expiry time.Time
		for k, v := range c.cache {
			if expiry.IsZero() || v.expiresAt.Before(expiry) {
				oldest, expiry = k, v.expiresAt
			}
		}
		delete(c.cache, oldest)
	}
	c.cache[key] = token
	return token, nil
}

func (c *TokenClient) authorize(ctx context.Context, key tokenCacheKey) (RepositoryAccess, error) {
	if c.config.IntegrationID != "" {
		if key.tenant != c.config.TenantID {
			return RepositoryAccess{}, credentialError("repository_not_authorized")
		}
		err := c.tenantAppAuth.AuthorizeTenantApp(ctx, TenantAppIdentity{TenantID: c.config.TenantID, IntegrationID: c.config.IntegrationID, GitHubAppID: c.config.AppID, ConfigRevision: c.config.ConfigRevision, CredentialGeneration: c.config.CredentialGeneration})
		if ctx.Err() != nil {
			return RepositoryAccess{}, ctx.Err()
		}
		if errors.Is(err, ErrRepositoryAuthorizationUnavailable) {
			return RepositoryAccess{}, &CredentialError{Code: "authorization_unavailable", Retryable: true}
		}
		if err != nil {
			return RepositoryAccess{}, credentialError("repository_not_authorized")
		}
	}
	access, err := c.authorizer.AuthorizeRepository(ctx, key.tenant, key.installation, key.repository)
	if ctx.Err() != nil {
		return RepositoryAccess{}, ctx.Err()
	}
	if errors.Is(err, ErrRepositoryAuthorizationUnavailable) {
		return RepositoryAccess{}, &CredentialError{Code: "authorization_unavailable", Retryable: true}
	}
	if err != nil || !validLogin(access.Owner) || !validCredentialRepositoryName(access.Name) {
		return RepositoryAccess{}, credentialError("repository_not_authorized")
	}
	return access, nil
}

func validCredentialRepositoryName(name string) bool {
	if name == "" || len(name) > 100 || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func (c *TokenClient) appJWT(ctx context.Context) (string, error) {
	secretCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	input := &secretsmanager.GetSecretValueInput{SecretId: &c.config.PrivateKeySecretARN}
	if c.config.PrivateKeySecretVersionID != "" {
		input.VersionId = &c.config.PrivateKeySecretVersionID
	} else {
		stage := "AWSCURRENT"
		input.VersionStage = &stage
	}
	response, err := c.secrets.GetSecretValue(secretCtx, input)
	if err != nil || response == nil || response.SecretString == nil || len(*response.SecretString) > 65536 || (c.config.PrivateKeySecretVersionID != "" && aws.ToString(response.VersionId) != c.config.PrivateKeySecretVersionID) {
		return "", &CredentialError{Code: "private_key_unavailable", Retryable: true}
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(*response.SecretString))
	if err != nil || key.N.BitLen() < 2048 {
		return "", credentialError("invalid_private_key")
	}
	return signAppJWT(key, c.config.AppID, c.now())
}

func signAppJWT(key *rsa.PrivateKey, appID int64, now time.Time) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{Issuer: strconv.FormatInt(appID, 10), IssuedAt: jwt.NewNumericDate(now.Add(-60 * time.Second)), ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute))})
	value, err := token.SignedString(key)
	if err != nil {
		return "", credentialError("jwt_signing_failed")
	}
	return value, nil
}

// ClearCache is a local emergency/rotation action. It does NOT revoke tokens
// at GitHub or invalidate caches in other replicas; see the operational docs.
func (c *TokenClient) ClearCache(ctx context.Context) error {
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.gate }()
	clear(c.cache)
	return nil
}

func (c *TokenClient) request(ctx context.Context, method, path, appJWT string, body []byte, dst any) error {
	if c.backoffUntil.After(c.now()) {
		return &CredentialError{Code: "github_backoff", Retryable: true, RetryAt: c.backoffUntil}
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, c.apiURL+path, bytes.NewReader(body))
		if err != nil {
			return credentialError("request_failed")
		}
		req.Header.Set("Authorization", "Bearer "+appJWT)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if method == http.MethodPost {
			if delay := c.lastMutation.Add(time.Second).Sub(c.now()); delay > 0 {
				if err := c.wait(ctx, delay); err != nil {
					return err
				}
			}
			c.lastMutation = c.now()
		}
		response, err := c.http.Do(req)
		var safeErr *CredentialError
		if err != nil {
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			safeErr = &CredentialError{Code: "transport_failed", Retryable: true}
		} else {
			retryable := response.StatusCode == 429 || response.StatusCode == 500 || response.StatusCode == 502 || response.StatusCode == 503 || response.StatusCode == 504 || (response.StatusCode == 403 && (response.Header.Get("X-RateLimit-Remaining") == "0" || response.Header.Get("Retry-After") != ""))
			safeErr = &CredentialError{Code: "github_rejected", Status: response.StatusCode, Retryable: retryable}
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				if err := decodeGitHubJSON(response, dst); err != nil {
					return credentialError("invalid_github_response")
				}
				return nil
			}
			// Discard without reading arbitrary error bodies (including tokens).
			_ = response.Body.Close()
			if response.StatusCode == 429 || (response.StatusCode == 403 && retryable) {
				until := c.now().Add(time.Minute)
				if reset, e := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64); e == nil && response.Header.Get("X-RateLimit-Remaining") == "0" && time.Unix(reset, 0).After(until) {
					until = time.Unix(reset, 0)
				}
				if seconds, e := strconv.ParseInt(response.Header.Get("Retry-After"), 10, 32); e == nil && seconds > 0 && c.now().Add(time.Duration(seconds)*time.Second).After(until) {
					until = c.now().Add(time.Duration(seconds) * time.Second)
				}
				c.backoffUntil, safeErr.RetryAt = until, until
				return safeErr
			}
			// Respect longer server backoff by returning to the orchestrator,
			// rather than immediately retrying a throttled endpoint.
			if delay := response.Header.Get("Retry-After"); delay != "" {
				seconds, e := strconv.ParseInt(delay, 10, 32)
				if e != nil || seconds > 0 {
					until := c.now().Add(time.Minute)
					if e == nil && seconds > 0 {
						until = c.now().Add(time.Duration(seconds) * time.Second)
					}
					if parsed, e := http.ParseTime(delay); e == nil && parsed.After(until) {
						until = parsed
					}
					c.backoffUntil, safeErr.RetryAt = until, until
					return safeErr
				}
			}
		}
		if !safeErr.Retryable || attempt == 2 {
			return safeErr
		}
		ceiling := time.Duration(200*(1<<attempt)) * time.Millisecond
		if err := c.wait(ctx, time.Duration(rand.Int63n(int64(ceiling)))); err != nil {
			return err
		}
	}
	return credentialError("request_failed")
}

func waitTokenRetry(ctx context.Context, ceiling time.Duration) error {
	timer := time.NewTimer(ceiling)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Compile-time checks prevent accidental use of an API incompatible loader.
var _ SecretValueAPI = (*secretsmanager.Client)(nil)
var _ fmt.Stringer = InstallationToken{}
var _ json.Marshaler = InstallationToken{}
