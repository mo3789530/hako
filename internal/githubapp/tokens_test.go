package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/golang-jwt/jwt/v5"
	"github.com/mo3789530/hako/internal/domain"
)

const testKeyARN = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:github-app-key-test"
const testTokenValue = "synthetic-token-never-log"

type secretAPIFunc func(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)

func (f secretAPIFunc) GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	return f(ctx, in, opts...)
}

type repositoryAuthorizerFunc func(context.Context, domain.TenantID, int64, int64) (RepositoryAccess, error)

func (f repositoryAuthorizerFunc) AuthorizeRepository(ctx context.Context, tenant domain.TenantID, installation, repository int64) (RepositoryAccess, error) {
	return f(ctx, tenant, installation, repository)
}

type scopedRepositoryAuthorizer struct {
	repositoryAuthorizerFunc
	check func(context.Context, TenantAppIdentity) error
}

func (a scopedRepositoryAuthorizer) AuthorizeTenantApp(ctx context.Context, identity TenantAppIdentity) error {
	return a.check(ctx, identity)
}

type actionsRunnerRepositoryAuthorizer struct {
	access  RepositoryAccess
	allowed bool
}

func (a actionsRunnerRepositoryAuthorizer) AuthorizeRepository(_ context.Context, tenant domain.TenantID, installation, repository int64) (RepositoryAccess, error) {
	if tenant != "tenant_one" || installation != 77 || repository != 88 {
		return RepositoryAccess{}, errors.New("denied")
	}
	return a.access, nil
}

func (a actionsRunnerRepositoryAuthorizer) AuthorizeTenantApp(_ context.Context, identity TenantAppIdentity) error {
	if identity.TenantID != "tenant_one" || identity.IntegrationID != "integration_one" || identity.ConfigRevision != 2 || identity.CredentialGeneration != 3 {
		return errors.New("stale App")
	}
	return nil
}

func (a actionsRunnerRepositoryAuthorizer) AuthorizeActionsRunnerRepository(_ context.Context, identity TenantAppIdentity, installation, repository int64) (RepositoryAccess, error) {
	if !a.allowed || identity.TenantID != "tenant_one" || identity.IntegrationID != "integration_one" || identity.GitHubAppID != 12 ||
		identity.ConfigRevision != 2 || identity.CredentialGeneration != 3 || installation != 77 || repository != 88 {
		return RepositoryAccess{}, errors.New("Runner policy denied")
	}
	return a.access, nil
}

type credentialTransportFunc func(*http.Request) (*http.Response, error)

func (f credentialTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newSigningKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal("generate test signing key failed")
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func newTestTokenClient(t *testing.T, handler http.Handler) (*TokenClient, *rsa.PrivateKey) {
	t.Helper()
	key, privatePEM := newSigningKey(t)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	secrets := secretAPIFunc(func(ctx context.Context, in *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
		if in.SecretId == nil || *in.SecretId != testKeyARN || in.VersionStage == nil || *in.VersionStage != "AWSCURRENT" {
			t.Error("incorrect secret ARN or version stage")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("secret request lacks deadline")
		}
		return &secretsmanager.GetSecretValueOutput{SecretString: &privatePEM}, nil
	})
	authorizer := repositoryAuthorizerFunc(func(_ context.Context, tenant domain.TenantID, installation, repository int64) (RepositoryAccess, error) {
		if tenant != "tenant_one" || installation != 77 || repository != 88 {
			return RepositoryAccess{}, errors.New("denied")
		}
		return RepositoryAccess{Owner: "acme", Name: "api"}, nil
	})
	client, err := NewTokenClient(TokenConfig{AppID: 12, PrivateKeySecretARN: testKeyARN}, secrets, authorizer, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.apiURL = server.URL
	client.now = func() time.Time { return time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC) }
	client.wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return client, key
}

func respondCredentials(w http.ResponseWriter, r *http.Request, now time.Time) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, `{"id":77,"app_id":12,"suspended_at":null}`)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"token": testTokenValue, "expires_at": now.Add(time.Hour), "permissions": map[string]string{"contents": "read", "metadata": "read"}, "repositories": []map[string]any{{"id": 88}}})
}

func requireCredentialCode(t *testing.T, err error, code string) {
	t.Helper()
	var safe *CredentialError
	if !errors.As(err, &safe) || safe.Code != code {
		t.Fatalf("unexpected credential error classification; want %s", code)
	}
	if strings.Contains(err.Error(), testTokenValue) || strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Fatal("credential error leaked secret")
	}
}

func TestCheckoutTokenSignsScopesCachesAndRefreshes(t *testing.T) {
	var mintCount atomic.Int32
	var jwtValue string
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	client, key := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jwtValue = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") == "" {
			t.Error("missing GitHub headers")
		}
		switch r.URL.Path {
		case "/app/installations/77", "/repos/acme/api/installation":
			if r.Method != http.MethodGet {
				t.Error("unexpected validation method")
			}
		case "/app/installations/77/access_tokens":
			mintCount.Add(1)
			var body struct {
				RepositoryIDs []int64           `json:"repository_ids"`
				Permissions   map[string]string `json:"permissions"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != 88 || len(body.Permissions) != 1 || body.Permissions["contents"] != "read" {
				t.Error("token request was not scoped to one repository/read-only contents")
			}
		default:
			t.Error("unexpected GitHub endpoint")
		}
		respondCredentials(w, r, now)
	}))
	ctx := context.Background()
	token, err := client.CheckoutToken(ctx, "tenant_one", 77, 88)
	if err != nil {
		t.Fatal(err)
	}
	if token.Value() != testTokenValue || !token.ExpiresAt().Equal(now.Add(time.Hour)) {
		t.Fatal("unexpected token or expiry")
	}
	claims := &jwt.RegisteredClaims{}
	_, err = jwt.ParseWithClaims(jwtValue, claims, func(token *jwt.Token) (any, error) { return &key.PublicKey, nil }, jwt.WithValidMethods([]string{"RS256"}), jwt.WithTimeFunc(func() time.Time { return now }), jwt.WithIssuer("12"), jwt.WithExpirationRequired())
	if err != nil || !claims.IssuedAt.Time.Equal(now.Add(-time.Minute)) || !claims.ExpiresAt.Time.Equal(now.Add(9*time.Minute)) {
		t.Fatal("invalid App JWT claims or signature")
	}
	if _, err = client.CheckoutToken(ctx, "tenant_one", 77, 88); err != nil || mintCount.Load() != 1 {
		t.Fatal("token cache did not reuse token")
	}
	now = now.Add(59 * time.Minute)
	client.now = func() time.Time { return now }
	if _, err = client.CheckoutToken(ctx, "tenant_one", 77, 88); err != nil || mintCount.Load() != 2 {
		t.Fatal("token did not refresh before expiry")
	}
	if err = client.ClearCache(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = client.CheckoutToken(ctx, "tenant_one", 77, 88); err != nil || mintCount.Load() != 3 {
		t.Fatal("cache clearing did not force mint")
	}
}

func TestAgentContextTokenIsTenantPinnedAndUsesSeparateReadOnlyScopes(t *testing.T) {
	var mintCount atomic.Int32
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/installations/77", "/repos/acme/api/installation":
			_, _ = io.WriteString(w, `{"id":77,"app_id":12,"suspended_at":null}`)
		case "/app/installations/77/access_tokens":
			mintCount.Add(1)
			var body struct {
				RepositoryIDs []int64           `json:"repository_ids"`
				Permissions   map[string]string `json:"permissions"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != 88 || len(body.Permissions) != 2 ||
				body.Permissions["contents"] != "read" || body.Permissions["issues"] != "read" {
				t.Error("Agent context token was not restricted to one repository and the two read permissions")
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"token": testTokenValue, "expires_at": now.Add(time.Hour),
				"permissions": map[string]string{"contents": "read", "issues": "read", "metadata": "read"}, "repositories": []map[string]int{{"id": 88}}})
		default:
			t.Errorf("unexpected GitHub endpoint %s", r.URL.Path)
		}
	}))
	client.config.TenantID = "tenant_one"
	client.config.IntegrationID = "integration_one"
	client.config.ConfigRevision = 2
	client.config.CredentialGeneration = 3
	client.now = func() time.Time { return now }
	checkAuth := func(_ context.Context, identity TenantAppIdentity) error {
		if identity.TenantID != "tenant_one" || identity.IntegrationID != "integration_one" || identity.ConfigRevision != 2 || identity.CredentialGeneration != 3 {
			return errors.New("wrong pinned Tenant App")
		}
		return nil
	}
	authorizer := scopedRepositoryAuthorizer{repositoryAuthorizerFunc: repositoryAuthorizerFunc(func(_ context.Context, tenant domain.TenantID, installation, repository int64) (RepositoryAccess, error) {
		if tenant != "tenant_one" || installation != 77 || repository != 88 {
			return RepositoryAccess{}, errors.New("repository denied")
		}
		return RepositoryAccess{Owner: "acme", Name: "api"}, nil
	}), check: checkAuth}
	client.tenantAppAuth = authorizer
	client.authorizer = authorizer

	if _, _, err := client.AgentContextToken(context.Background(), "tenant_other", 77, 88); err == nil {
		t.Fatal("cross-Tenant Agent context token was issued")
	}
	token, access, err := client.AgentContextToken(context.Background(), "tenant_one", 77, 88)
	if err != nil || token.Value() != testTokenValue || access != (RepositoryAccess{Owner: "acme", Name: "api"}) {
		t.Fatalf("Agent context token = redacted token, %+v, error=%v", access, err)
	}
	if _, _, err := client.AgentContextToken(context.Background(), "tenant_one", 77, 88); err != nil || mintCount.Load() != 1 {
		t.Fatalf("Agent token cache failed or mixed scope: mint count=%d error=%v", mintCount.Load(), err)
	}
}

func TestAgentProposalPublishTokenIsTenantPinnedAndExactWriteScope(t *testing.T) {
	var mintCount atomic.Int32
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/installations/77", "/repos/acme/api/installation":
			_, _ = io.WriteString(w, `{"id":77,"app_id":12,"suspended_at":null}`)
		case "/app/installations/77/access_tokens":
			mintCount.Add(1)
			var body struct {
				RepositoryIDs []int64           `json:"repository_ids"`
				Permissions   map[string]string `json:"permissions"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != 88 ||
				len(body.Permissions) != 2 || body.Permissions["contents"] != "write" || body.Permissions["pull_requests"] != "write" {
				t.Error("Agent proposal token did not request exactly repository-scoped Contents/write and Pull Requests/write")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": testTokenValue, "expires_at": now.Add(time.Hour),
				"permissions":  map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read"},
				"repositories": []map[string]int{{"id": 88}}})
		default:
			t.Errorf("unexpected GitHub endpoint %s", r.URL.Path)
		}
	}))
	client.config.TenantID = "tenant_one"
	client.config.IntegrationID = "integration_one"
	client.config.ConfigRevision = 2
	client.config.CredentialGeneration = 3
	client.now = func() time.Time { return now }
	checkAuth := func(_ context.Context, identity TenantAppIdentity) error {
		if identity.TenantID != "tenant_one" || identity.IntegrationID != "integration_one" || identity.ConfigRevision != 2 || identity.CredentialGeneration != 3 {
			return errors.New("wrong pinned Tenant App")
		}
		return nil
	}
	authorizer := scopedRepositoryAuthorizer{repositoryAuthorizerFunc: repositoryAuthorizerFunc(func(_ context.Context, tenant domain.TenantID, installation, repository int64) (RepositoryAccess, error) {
		if tenant != "tenant_one" || installation != 77 || repository != 88 {
			return RepositoryAccess{}, errors.New("repository denied")
		}
		return RepositoryAccess{Owner: "acme", Name: "api"}, nil
	}), check: checkAuth}
	client.tenantAppAuth = authorizer
	client.authorizer = authorizer
	run := domain.WorkloadRun{ID: "run_agent_publish", TenantID: "tenant_one", Kind: domain.WorkloadAgent,
		GitHubInstallation: 77, GitHubRepository: 88, GitHubIntegrationID: "integration_one",
		GitHubConfigRevision: 2, GitHubCredentialGeneration: 3}
	if _, _, err := client.AgentProposalPublishToken(context.Background(), domain.WorkloadRun{TenantID: "tenant_other", Kind: domain.WorkloadAgent,
		GitHubInstallation: 77, GitHubRepository: 88, GitHubIntegrationID: "integration_one", GitHubConfigRevision: 2, GitHubCredentialGeneration: 3}); err == nil {
		t.Fatal("cross-Tenant Agent publication token was issued")
	}
	if _, _, err := client.AgentProposalPublishToken(context.Background(), domain.WorkloadRun{TenantID: "tenant_one", Kind: domain.WorkloadJob,
		GitHubInstallation: 77, GitHubRepository: 88, GitHubIntegrationID: "integration_one", GitHubConfigRevision: 2, GitHubCredentialGeneration: 3}); err == nil {
		t.Fatal("non-Agent Run received a publication token")
	}
	token, access, err := client.AgentProposalPublishToken(context.Background(), run)
	if err != nil || token.Value() != testTokenValue || access != (RepositoryAccess{Owner: "acme", Name: "api"}) {
		t.Fatalf("Agent proposal publish token = redacted token, %+v, error=%v", access, err)
	}
	if _, _, err := client.AgentProposalPublishToken(context.Background(), run); err != nil || mintCount.Load() != 1 {
		t.Fatalf("scoped token cache failed or mixed permissions: mint count=%d error=%v", mintCount.Load(), err)
	}
}

func TestResolveAgentIssueContextReturnsImmutableDefaultBranchCommit(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	commit := strings.Repeat("a", 40)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+testTokenValue {
			t.Errorf("unexpected Agent context request method/auth: %s %q", r.Method, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch r.URL.Path {
		case "/repos/acme/api/issues/17":
			_, _ = io.WriteString(w, `{"number":17,"title":"Fix parser","body":"Please fix safely","state":"open"}`)
		case "/repos/acme/api":
			_, _ = io.WriteString(w, `{"default_branch":"main"}`)
		case "/repos/acme/api/commits":
			if r.URL.Query().Get("sha") != "main" || r.URL.Query().Get("per_page") != "1" {
				t.Errorf("commit lookup is not pinned to the default branch: %s", r.URL.RawQuery)
			}
			_, _ = fmt.Fprintf(w, `[{"sha":%q}]`, commit)
		default:
			t.Errorf("unexpected Agent context path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &TokenClient{http: server.Client(), apiURL: server.URL, now: func() time.Time { return now }}
	token := InstallationToken{value: testTokenValue, expiresAt: now.Add(time.Hour)}
	got, err := client.ResolveAgentIssueContext(context.Background(), token, RepositoryAccess{Owner: "acme", Name: "api"}, 17)
	if err != nil || got.IssueNumber != 17 || got.Title != "Fix parser" || got.Body != "Please fix safely" || got.DefaultRef != "main" || got.CommitSHA != commit {
		t.Fatalf("resolved Agent issue context = %+v error=%v", got, err)
	}
}

func TestResolveAgentIssueContextRejectsPullRequestsAndInvalidSHAs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		issue      string
		commitJSON string
	}{
		{name: "pull request", issue: `{"number":17,"title":"PR","body":"","state":"open","pull_request":{"url":"https://github.test/pulls/17"}}`, commitJSON: `[{"sha":"` + strings.Repeat("b", 40) + `"}]`},
		{name: "malformed sha", issue: `{"number":17,"title":"Issue","body":"","state":"open"}`, commitJSON: `[{"sha":"not-a-commit"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/repos/acme/api/issues/17":
					_, _ = io.WriteString(w, tc.issue)
				case "/repos/acme/api":
					_, _ = io.WriteString(w, `{"default_branch":"main"}`)
				case "/repos/acme/api/commits":
					_, _ = io.WriteString(w, tc.commitJSON)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := &TokenClient{http: server.Client(), apiURL: server.URL, now: func() time.Time { return now }}
			token := InstallationToken{value: testTokenValue, expiresAt: now.Add(time.Hour)}
			if _, err := client.ResolveAgentIssueContext(context.Background(), token, RepositoryAccess{Owner: "acme", Name: "api"}, 17); err == nil {
				t.Fatal("untrusted Issue context accepted")
			}
		})
	}
}

func TestChecksTokenIsRepositoryScopedAndNeverReusesCheckoutToken(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	var minted []string
	client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/installations/77", "/repos/acme/api/installation":
			_, _ = io.WriteString(w, `{"id":77,"app_id":12,"suspended_at":null}`)
		case "/app/installations/77/access_tokens":
			var body struct {
				RepositoryIDs []int64           `json:"repository_ids"`
				Permissions   map[string]string `json:"permissions"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != 88 {
				t.Error("Installation token was not limited to the authorized Repository")
			}
			if body.Permissions["checks"] == "write" && len(body.Permissions) == 1 {
				minted = append(minted, "checks")
				_ = json.NewEncoder(w).Encode(map[string]any{"token": "synthetic-checks-write-token", "expires_at": now.Add(time.Hour), "permissions": map[string]string{"checks": "write", "metadata": "read"}, "repositories": []map[string]int{{"id": 88}}})
				return
			}
			if body.Permissions["contents"] == "read" && len(body.Permissions) == 1 {
				minted = append(minted, "contents")
				_ = json.NewEncoder(w).Encode(map[string]any{"token": testTokenValue, "expires_at": now.Add(time.Hour), "permissions": map[string]string{"contents": "read", "metadata": "read"}, "repositories": []map[string]int{{"id": 88}}})
				return
			}
			t.Errorf("unexpected installation token permission scope: %+v", body.Permissions)
		default:
			t.Errorf("unexpected GitHub endpoint %s", r.URL.Path)
		}
	}))
	client.now = func() time.Time { return now }

	checkout, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88)
	if err != nil || checkout.Value() != testTokenValue {
		t.Fatalf("read-only checkout token failed: %v", err)
	}
	checks, err := client.ChecksToken(context.Background(), "tenant_one", 77, 88)
	if err != nil || checks.Value() != "synthetic-checks-write-token" {
		t.Fatalf("checks token failed: %v", err)
	}
	if _, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ChecksToken(context.Background(), "tenant_one", 77, 88); err != nil {
		t.Fatal(err)
	}
	if len(minted) != 2 || minted[0] != "contents" || minted[1] != "checks" {
		t.Fatalf("permission scopes shared a token cache or minted unexpectedly: %v", minted)
	}
	if strings.Contains(fmt.Sprintf("%+v", checks), "synthetic-checks-write-token") {
		t.Fatal("checks Installation token leaked via formatting")
	}
}

func TestActionsRunnerRegistrationTokenRequiresOptInAndUsesRepositoryScopedPermissions(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	registrationToken := "synthetic-runner-registration-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/installations/77", "/repos/acme/api/installation":
			_, _ = io.WriteString(w, `{"id":77,"app_id":12,"suspended_at":null}`)
		case "/app/installations/77/access_tokens":
			var body struct {
				RepositoryIDs []int64           `json:"repository_ids"`
				Permissions   map[string]string `json:"permissions"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != 88 ||
				len(body.Permissions) != 1 || body.Permissions["administration"] != "write" {
				t.Errorf("runner Installation token was broader than its exact repository scope: %+v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": testTokenValue, "expires_at": now.Add(time.Hour),
				"permissions": map[string]string{"administration": "write", "metadata": "read"}, "repositories": []map[string]int{{"id": 88}}})
		case "/repos/acme/api/actions/runners/registration-token":
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+testTokenValue {
				t.Errorf("registration token request method/credential = %s / %q", r.Method, r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": registrationToken, "expires_at": now.Add(time.Hour)})
		default:
			t.Errorf("unexpected GitHub endpoint %s", r.URL.Path)
		}
	}))
	defer server.Close()
	_, privatePEM := newSigningKey(t)
	secrets := secretAPIFunc(func(_ context.Context, _ *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
		versionID := strings.Repeat("a", 32)
		return &secretsmanager.GetSecretValueOutput{SecretString: &privatePEM, VersionId: &versionID}, nil
	})
	authorizer := actionsRunnerRepositoryAuthorizer{access: RepositoryAccess{Owner: "acme", Name: "api"}, allowed: true}
	client, err := NewTokenClient(TokenConfig{AppID: 12, PrivateKeySecretARN: testKeyARN, PrivateKeySecretVersionID: strings.Repeat("a", 32),
		TenantID: "tenant_one", IntegrationID: "integration_one", ConfigRevision: 2, CredentialGeneration: 3}, secrets, authorizer, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.apiURL = server.URL
	client.now = func() time.Time { return now }
	client.wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	token, repository, err := client.ActionsRunnerRegistrationToken(context.Background(), "tenant_one", 77, 88)
	if err != nil || token.Value() != registrationToken || !token.ExpiresAt().Equal(now.Add(time.Hour)) || repository.Owner != "acme" || repository.Name != "api" {
		t.Fatalf("ActionsRunnerRegistrationToken() = %v, %+v, error=%v", token, repository, err)
	}
	encoded, err := json.Marshal(token)
	if err != nil || strings.Contains(string(encoded), registrationToken) || strings.Contains(fmt.Sprintf("%+v", token), registrationToken) {
		t.Fatalf("runner registration token leaked in formatting/JSON: %s (%v)", encoded, err)
	}
	if _, cached := client.cache[tokenCacheKey{tenant: "tenant_one", integration: "integration_one", appID: 12, configRevision: 2,
		credentialGeneration: 3, installation: 77, repository: 88, permissionScope: "administration:write"}]; cached {
		t.Fatal("broad Runner Installation token remained cached")
	}
}

func TestActionsRunnerRegistrationTokenDeniesRepositoryWithoutRunnerOptInBeforeGitHubCall(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	_, privatePEM := newSigningKey(t)
	secrets := secretAPIFunc(func(_ context.Context, _ *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
		versionID := strings.Repeat("a", 32)
		return &secretsmanager.GetSecretValueOutput{SecretString: &privatePEM, VersionId: &versionID}, nil
	})
	authorizer := actionsRunnerRepositoryAuthorizer{access: RepositoryAccess{Owner: "acme", Name: "api"}, allowed: false}
	client, err := NewTokenClient(TokenConfig{AppID: 12, PrivateKeySecretARN: testKeyARN, PrivateKeySecretVersionID: strings.Repeat("a", 32),
		TenantID: "tenant_one", IntegrationID: "integration_one", ConfigRevision: 2, CredentialGeneration: 3}, secrets, authorizer, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.apiURL = server.URL
	_, _, err = client.ActionsRunnerRegistrationToken(context.Background(), "tenant_one", 77, 88)
	requireCredentialCode(t, err, "repository_not_authorized")
	if called {
		t.Fatal("GitHub API called for repository without Runner opt-in")
	}
}

func TestActionsRunnerProvenanceTokenUsesReadOnlyRepositoryScope(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/installations/77", "/repos/acme/api/installation":
			_, _ = io.WriteString(w, `{"id":77,"app_id":12,"suspended_at":null}`)
		case "/app/installations/77/access_tokens":
			var body struct {
				RepositoryIDs []int64           `json:"repository_ids"`
				Permissions   map[string]string `json:"permissions"`
			}
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil || len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != 88 ||
				len(body.Permissions) != 1 || body.Permissions["actions"] != "read" {
				t.Errorf("provenance token was not scoped to Actions read: %+v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": testTokenValue, "expires_at": now.Add(time.Hour),
				"permissions": map[string]string{"actions": "read", "metadata": "read"}, "repositories": []map[string]int{{"id": 88}}})
		default:
			t.Errorf("unexpected GitHub endpoint %s", r.URL.Path)
		}
	}))
	defer server.Close()
	_, privatePEM := newSigningKey(t)
	secrets := secretAPIFunc(func(_ context.Context, _ *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
		versionID := strings.Repeat("a", 32)
		return &secretsmanager.GetSecretValueOutput{SecretString: &privatePEM, VersionId: &versionID}, nil
	})
	authorizer := actionsRunnerRepositoryAuthorizer{access: RepositoryAccess{Owner: "acme", Name: "api"}, allowed: true}
	client, err := NewTokenClient(TokenConfig{AppID: 12, PrivateKeySecretARN: testKeyARN, PrivateKeySecretVersionID: strings.Repeat("a", 32),
		TenantID: "tenant_one", IntegrationID: "integration_one", ConfigRevision: 2, CredentialGeneration: 3}, secrets, authorizer, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.apiURL = server.URL
	client.now = func() time.Time { return now }
	token, repository, err := client.ActionsRunnerProvenanceToken(context.Background(), "tenant_one", 77, 88)
	if err != nil || token.Value() != testTokenValue || repository != (RepositoryAccess{Owner: "acme", Name: "api"}) {
		t.Fatalf("ActionsRunnerProvenanceToken() = %v, %+v, error = %v", token, repository, err)
	}
	if _, cached := client.cache[tokenCacheKey{tenant: "tenant_one", integration: "integration_one", appID: 12, configRevision: 2,
		credentialGeneration: 3, installation: 77, repository: 88, permissionScope: "actions:read"}]; !cached {
		t.Fatal("read-only provenance token was not cached under its distinct scope")
	}
	approvalToken, approvalRepository, err := client.EnvironmentApprovalToken(context.Background(), "tenant_one", 77, 88)
	if err != nil || approvalToken.Value() != testTokenValue || approvalRepository != (RepositoryAccess{Owner: "acme", Name: "api"}) {
		t.Fatalf("EnvironmentApprovalToken() = %v, %+v, error = %v", approvalToken, approvalRepository, err)
	}
}

func TestDeregisterActionsRunnerUsesExactRepositoryScopeAfterRunnerOptOut(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	deleteCalled := false
	deleteStatus := http.StatusNoContent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/installations/77", "/repos/acme/api/installation":
			_, _ = io.WriteString(w, `{"id":77,"app_id":12,"suspended_at":null}`)
		case "/app/installations/77/access_tokens":
			var body struct {
				RepositoryIDs []int64           `json:"repository_ids"`
				Permissions   map[string]string `json:"permissions"`
			}
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil || len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != 88 ||
				len(body.Permissions) != 1 || body.Permissions["administration"] != "write" {
				t.Errorf("deregister Installation token was not exact/repository-scoped: %+v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": testTokenValue, "expires_at": now.Add(time.Hour),
				"permissions": map[string]string{"administration": "write", "metadata": "read"}, "repositories": []map[string]int{{"id": 88}}})
		case "/repos/acme/api/actions/runners/9810":
			deleteCalled = true
			if r.Method != http.MethodDelete || r.Header.Get("Authorization") != "Bearer "+testTokenValue {
				t.Errorf("deregister request method/credential = %s / %q", r.Method, r.Header.Get("Authorization"))
			}
			w.WriteHeader(deleteStatus)
		default:
			t.Errorf("unexpected GitHub endpoint %s", r.URL.Path)
		}
	}))
	defer server.Close()
	_, privatePEM := newSigningKey(t)
	secrets := secretAPIFunc(func(_ context.Context, _ *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
		versionID := strings.Repeat("a", 32)
		return &secretsmanager.GetSecretValueOutput{SecretString: &privatePEM, VersionId: &versionID}, nil
	})
	// Runner opt-in is deliberately false: cleanup must still be allowed after
	// admission was disabled, while the active Tenant App/repository remain bound.
	authorizer := actionsRunnerRepositoryAuthorizer{access: RepositoryAccess{Owner: "acme", Name: "api"}, allowed: false}
	client, err := NewTokenClient(TokenConfig{AppID: 12, PrivateKeySecretARN: testKeyARN, PrivateKeySecretVersionID: strings.Repeat("a", 32),
		TenantID: "tenant_one", IntegrationID: "integration_one", ConfigRevision: 2, CredentialGeneration: 3}, secrets, authorizer, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.apiURL = server.URL
	client.now = func() time.Time { return now }
	client.wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if err := client.DeregisterActionsRunner(context.Background(), "tenant_one", 77, 88, 9810); err != nil {
		t.Fatalf("DeregisterActionsRunner() error=%v", err)
	}
	deleteStatus = http.StatusNotFound
	if err := client.DeregisterActionsRunner(context.Background(), "tenant_one", 77, 88, 9810); err != nil {
		t.Fatalf("idempotent DeregisterActionsRunner() after runner removal error=%v", err)
	}
	if !deleteCalled {
		t.Fatal("GitHub runner deregistration endpoint was not called")
	}
}

func TestVerifyAppProvesConfiguredIDWithSafeErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "matching app", body: `{"id":12}`},
		{name: "different app", body: `{"id":99}`, want: "app_id_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.body = strings.ReplaceAll(tc.body, `\`, "")
			client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/app" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
					t.Errorf("unexpected App proof request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			err := client.VerifyApp(context.Background())
			if tc.want == "" && err != nil {
				t.Fatalf("matching App proof failed: %v", err)
			}
			if tc.want != "" {
				requireCredentialCode(t, err, tc.want)
			}
		})
	}
}

func TestTenantTokenClientRequestsPinnedSecretVersion(t *testing.T) {
	client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/app" {
			t.Errorf("unexpected pinned App proof request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":12}`)
	}))
	_, privatePEM := newSigningKey(t)
	client.config.PrivateKeySecretVersionID = "0123456789abcdef0123456789abcdef"
	client.secrets = secretAPIFunc(func(_ context.Context, input *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
		if aws.ToString(input.SecretId) != testKeyARN || aws.ToString(input.VersionId) != client.config.PrivateKeySecretVersionID || input.VersionStage != nil {
			t.Fatal("Tenant App private key was not fetched by immutable VersionId")
		}
		return &secretsmanager.GetSecretValueOutput{SecretString: &privatePEM, VersionId: aws.String(client.config.PrivateKeySecretVersionID)}, nil
	})
	if err := client.VerifyApp(context.Background()); err != nil {
		t.Fatalf("pinned Tenant App proof failed: %v", err)
	}
	client.secrets = secretAPIFunc(func(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
		return &secretsmanager.GetSecretValueOutput{SecretString: &privatePEM, VersionId: aws.String("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}, nil
	})
	requireCredentialCode(t, client.VerifyApp(context.Background()), "private_key_unavailable")
}

func TestTenantTokenCacheIsScopedAndRevalidatesGeneration(t *testing.T) {
	var tokenMints, githubRequests, identityChecks atomic.Int32
	allowed := atomic.Bool{}
	allowed.Store(true)
	key, privatePEM := newSigningKey(t)
	_ = key
	secrets := secretAPIFunc(func(_ context.Context, input *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
		if aws.ToString(input.VersionId) != "0123456789abcdef0123456789abcdef" || input.VersionStage != nil {
			t.Fatal("Tenant cache client did not use its immutable key version")
		}
		return &secretsmanager.GetSecretValueOutput{SecretString: &privatePEM, VersionId: input.VersionId}, nil
	})
	authorizer := scopedRepositoryAuthorizer{
		repositoryAuthorizerFunc: func(_ context.Context, tenant domain.TenantID, installation, repository int64) (RepositoryAccess, error) {
			if tenant != "tenant_one" || installation != 77 || repository != 88 {
				return RepositoryAccess{}, errors.New("denied")
			}
			return RepositoryAccess{Owner: "acme", Name: "api"}, nil
		},
		check: func(_ context.Context, identity TenantAppIdentity) error {
			identityChecks.Add(1)
			if identity.TenantID != "tenant_one" || identity.IntegrationID != "integration_one" || identity.GitHubAppID != 12 || identity.ConfigRevision != 4 || identity.CredentialGeneration != 7 || !allowed.Load() {
				return errors.New("stale or unauthorized Tenant App identity")
			}
			return nil
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		githubRequests.Add(1)
		if r.Method == http.MethodPost {
			tokenMints.Add(1)
		}
		respondCredentials(w, r, time.Now())
	}))
	defer server.Close()
	client, err := NewTokenClient(TokenConfig{
		AppID:                     12,
		PrivateKeySecretARN:       testKeyARN,
		PrivateKeySecretVersionID: "0123456789abcdef0123456789abcdef",
		TenantID:                  "tenant_one",
		IntegrationID:             "integration_one",
		ConfigRevision:            4,
		CredentialGeneration:      7,
	}, secrets, authorizer, server.Client())
	if err != nil {
		t.Fatal("scoped Tenant App client rejected its live authorizer")
	}
	client.apiURL = server.URL
	client.now = time.Now
	client.wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	for i := 0; i < 2; i++ {
		if _, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88); err != nil {
			t.Fatalf("scoped token request failed: %v", err)
		}
	}
	if tokenMints.Load() != 1 || identityChecks.Load() < 4 {
		t.Fatal("cache either reminted unnecessarily or did not revalidate active generation on reuse")
	}
	githubRequestsBeforeDenial := githubRequests.Load()
	allowed.Store(false)
	requireCredentialCode(t, func() error {
		_, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88)
		return err
	}(), "repository_not_authorized")
	if githubRequests.Load() != githubRequestsBeforeDenial {
		t.Fatal("stale generation reached GitHub before authorization was rechecked")
	}
	_, err = client.CheckoutToken(context.Background(), "tenant_other", 77, 88)
	requireCredentialCode(t, err, "repository_not_authorized")
	if githubRequests.Load() != githubRequestsBeforeDenial {
		t.Fatal("another Tenant used a scoped App client")
	}
}

func TestTenantScopedTokenConfigRequiresIntegrationIdentityAuthorizer(t *testing.T) {
	secrets := secretAPIFunc(func(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
		return nil, nil
	})
	repositoryAuthorizer := repositoryAuthorizerFunc(func(context.Context, domain.TenantID, int64, int64) (RepositoryAccess, error) {
		return RepositoryAccess{}, nil
	})
	config := TokenConfig{AppID: 12, PrivateKeySecretARN: testKeyARN, TenantID: "tenant_one", IntegrationID: "integration_one", ConfigRevision: 1, CredentialGeneration: 1, PrivateKeySecretVersionID: "0123456789abcdef0123456789abcdef"}
	if _, err := NewTokenClient(config, secrets, repositoryAuthorizer, nil); err == nil {
		t.Fatal("Tenant-scoped TokenClient accepted an authorizer that cannot revalidate App generation")
	}
	if _, err := NewTokenClient(TokenConfig{AppID: 12, PrivateKeySecretARN: testKeyARN, TenantID: "tenant_one"}, secrets, repositoryAuthorizer, nil); err == nil {
		t.Fatal("partially scoped Tenant App identity was accepted")
	}
}

func TestCheckoutTokenConcurrentRequestsMintOnce(t *testing.T) {
	var mints atomic.Int32
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mints.Add(1)
		}
		respondCredentials(w, r, now)
	}))
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			if _, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88); err != nil {
				t.Error("concurrent checkout token failed")
			}
		})
	}
	wg.Wait()
	if mints.Load() != 1 {
		t.Fatal("concurrent requests minted duplicate tokens")
	}
}

func TestCheckoutTokenRejectsUnauthorizedTenantBeforeSecretOrGitHub(t *testing.T) {
	var calls atomic.Int32
	client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	client.secrets = secretAPIFunc(func(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
		t.Error("unauthorized request read key")
		return nil, nil
	})
	for _, tenant := range []domain.TenantID{"tenant_other", ""} {
		_, err := client.CheckoutToken(context.Background(), tenant, 77, 88)
		code := "repository_not_authorized"
		if tenant == "" {
			code = "invalid_request"
		}
		requireCredentialCode(t, err, code)
	}
	if calls.Load() != 0 {
		t.Fatal("unauthorized request reached GitHub")
	}
}

func TestCheckoutTokenClassifiesTemporaryAuthorizationFailure(t *testing.T) {
	client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("failed authorization reached GitHub") }))
	client.authorizer = repositoryAuthorizerFunc(func(context.Context, domain.TenantID, int64, int64) (RepositoryAccess, error) {
		return RepositoryAccess{}, ErrRepositoryAuthorizationUnavailable
	})
	_, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88)
	requireCredentialCode(t, err, "authorization_unavailable")
	var safe *CredentialError
	_ = errors.As(err, &safe)
	if !safe.Retryable {
		t.Fatal("temporary authorization failure was not retryable")
	}
}

func TestCheckoutTokenRechecksAuthorizationAndInvalidatesCachedToken(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { respondCredentials(w, r, now) }))
	if _, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88); err != nil {
		t.Fatal(err)
	}
	var authorizations int
	client.authorizer = repositoryAuthorizerFunc(func(context.Context, domain.TenantID, int64, int64) (RepositoryAccess, error) {
		authorizations++
		if authorizations == 2 {
			return RepositoryAccess{}, errors.New(testTokenValue)
		}
		return RepositoryAccess{Owner: "acme", Name: "api"}, nil
	})
	_, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88)
	requireCredentialCode(t, err, "repository_not_authorized")
	if len(client.cache) != 0 {
		t.Fatal("denied token remained cached")
	}
}

func TestCheckoutTokenRejectsInstallationStatesIncludingCache(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		body, code string
	}{
		{"revoked", 404, `{"message":"` + testTokenValue + `"}`, "github_rejected"},
		{"suspended", 200, `{"id":77,"app_id":12,"suspended_at":"2026-10-04T00:00:00Z"}`, "installation_unavailable"},
		{"wrong-app", 200, `{"id":77,"app_id":99}`, "installation_unavailable"},
		{"wrong-installation", 200, `{"id":99,"app_id":12}`, "installation_unavailable"},
		{"invalid-json", 200, `{"id":"` + testTokenValue + `"}`, "invalid_github_response"},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := false
			now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
			client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if bad {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(test.status)
					_, _ = io.WriteString(w, test.body)
					return
				}
				respondCredentials(w, r, now)
			}))
			if _, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88); err != nil {
				t.Fatal(err)
			}
			bad = true
			_, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88)
			requireCredentialCode(t, err, test.code)
			if len(client.cache) != 0 {
				t.Fatal("unavailable Installation token remained cached")
			}
		})
	}
}

func TestCheckoutTokenRejectsRepositoryInstallationMismatch(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":99,"app_id":12}`)
			return
		}
		if r.Method == http.MethodPost {
			t.Error("minted token for mismatched repository")
		}
		respondCredentials(w, r, now)
	}))
	_, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88)
	requireCredentialCode(t, err, "repository_unavailable")
}

func TestCheckoutTokenRejectsInvalidTokenResponses(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
		code   string
	}{
		{"empty", func(b map[string]any) { b["token"] = "" }, "invalid_token_response"},
		{"header-injection", func(b map[string]any) { b["token"] = "synthetic\r\nvalue" }, "invalid_token_response"},
		{"expired", func(b map[string]any) { b["expires_at"] = "2026-10-03T00:00:00Z" }, "invalid_token_response"},
		{"too-long", func(b map[string]any) { b["expires_at"] = "2026-10-05T00:00:00Z" }, "invalid_token_response"},
		{"wrong-repo", func(b map[string]any) { b["repositories"] = []any{map[string]any{"id": 99}} }, "invalid_token_response"},
		{"wide-repo-scope", func(b map[string]any) { b["repositories"] = []any{map[string]any{"id": 88}, map[string]any{"id": 99}} }, "invalid_token_response"},
		{"contents-write", func(b map[string]any) { b["permissions"] = map[string]string{"contents": "write"} }, "invalid_token_response"},
		{"extra-permission", func(b map[string]any) { b["permissions"] = map[string]string{"contents": "read", "issues": "write"} }, "invalid_token_scope"},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
			client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					respondCredentials(w, r, now)
					return
				}
				body := map[string]any{"token": testTokenValue, "expires_at": now.Add(time.Hour), "permissions": map[string]string{"contents": "read"}, "repositories": []map[string]any{{"id": 88}}}
				test.mutate(body)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(body)
			}))
			_, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88)
			requireCredentialCode(t, err, test.code)
		})
	}
}

func TestCheckoutTokenRetryClassificationAndBounds(t *testing.T) {
	for _, test := range []struct {
		status                int
		retryAfter, remaining string
		attempts              int
		retryable             bool
	}{
		{401, "", "", 1, false}, {403, "", "", 1, false}, {404, "", "", 1, false}, {422, "", "", 1, false},
		{500, "", "", 3, true}, {502, "", "", 3, true}, {503, "", "", 3, true}, {504, "", "", 3, true}, {429, "", "", 1, true},
		{403, "", "0", 1, true}, {429, "30", "", 1, true}, {503, "Wed, 21 Oct 2026 07:28:00 GMT", "", 1, true},
	} {
		t.Run(fmt.Sprintf("%d-%s-%s", test.status, test.retryAfter, test.remaining), func(t *testing.T) {
			var calls atomic.Int32
			client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", test.retryAfter)
				w.Header().Set("X-RateLimit-Remaining", test.remaining)
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, testTokenValue)
			}))
			_, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88)
			requireCredentialCode(t, err, "github_rejected")
			var safe *CredentialError
			_ = errors.As(err, &safe)
			if calls.Load() != int32(test.attempts) || safe.Retryable != test.retryable || safe.Status != test.status {
				t.Fatal("incorrect retry classification or attempt bound")
			}
		})
	}
}

func TestCheckoutTokenRetriesTransientFailureThenSucceeds(t *testing.T) {
	var failures atomic.Int32
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/77" && failures.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		respondCredentials(w, r, now)
	}))
	if _, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88); err != nil || failures.Load() != 2 {
		t.Fatal("transient failure was not retried successfully")
	}
}

func TestCheckoutTokenDoesNotFollowRedirectOrLeakTransportError(t *testing.T) {
	var reached atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer destination.Close()
	client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	_, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88)
	requireCredentialCode(t, err, "github_rejected")
	if reached.Load() != 0 {
		t.Fatal("followed credential redirect")
	}
	var attempts int
	client.http.Transport = credentialTransportFunc(func(r *http.Request) (*http.Response, error) { attempts++; return nil, errors.New(testTokenValue) })
	_, err = client.CheckoutToken(context.Background(), "tenant_one", 77, 88)
	requireCredentialCode(t, err, "transport_failed")
	if attempts != 3 {
		t.Fatal("transport retry exceeded bounds")
	}
}

func TestTokenClientCancellationAndTimeoutConfiguration(t *testing.T) {
	client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("cancelled request reached GitHub") }))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.CheckoutToken(ctx, "tenant_one", 77, 88)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled context not respected")
	}
	client.gate <- struct{}{}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err = client.CheckoutToken(ctx, "tenant_one", 77, 88)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("cache gate wait was not bounded")
	}
	if err = client.ClearCache(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("cache clearing did not respect cancellation")
	}
	<-client.gate
	if client.http.Timeout != 10*time.Second {
		t.Fatal("HTTP timeout not bounded")
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if !errors.Is(waitTokenRetry(ctx, time.Second), context.Canceled) {
		t.Fatal("retry sleep ignored cancellation")
	}
}

func TestPrivateKeyRotationAndSafeKeyFailures(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	var seenJWT string
	client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenJWT = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		respondCredentials(w, r, now)
	}))
	if _, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88); err != nil {
		t.Fatal(err)
	}
	key, pkcs1 := newSigningKey(t)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal("encode PKCS8 failed")
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))
	client.secrets = secretAPIFunc(func(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
		return &secretsmanager.GetSecretValueOutput{SecretString: &privatePEM}, nil
	})
	if _, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88); err != nil {
		t.Fatal(err)
	}
	if _, err := jwt.Parse(seenJWT, func(*jwt.Token) (any, error) { return &key.PublicKey, nil }, jwt.WithTimeFunc(func() time.Time { return now }), jwt.WithValidMethods([]string{"RS256"})); err != nil {
		t.Fatal("rotated PKCS8 key not used")
	}
	privatePEM = pkcs1
	if _, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88); err != nil {
		t.Fatal(err)
	}
	privatePEM = "invalid-private-key-" + testTokenValue
	_, err = client.CheckoutToken(context.Background(), "tenant_one", 77, 88)
	requireCredentialCode(t, err, "invalid_private_key")
	client.secrets = secretAPIFunc(func(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
		return nil, errors.New(testTokenValue)
	})
	_, err = client.CheckoutToken(context.Background(), "tenant_one", 77, 88)
	requireCredentialCode(t, err, "private_key_unavailable")
}

func TestInstallationTokenFormattingNeverExposesValue(t *testing.T) {
	token := InstallationToken{value: testTokenValue, expiresAt: time.Now()}
	for _, format := range []string{"%s", "%v", "%+v", "%#v", "%q", "%x", "%d"} {
		if strings.Contains(fmt.Sprintf(format, token), testTokenValue) {
			t.Fatal("token formatting leaked value")
		}
	}
	encoded, err := json.Marshal(struct{ Token InstallationToken }{token})
	if err != nil || strings.Contains(string(encoded), testTokenValue) {
		t.Fatal("token JSON leaked value")
	}
}

func TestTokenConfigRejectsMissingOrBroadSecrets(t *testing.T) {
	for _, config := range []TokenConfig{{}, {AppID: 12}, {AppID: -1, PrivateKeySecretARN: testKeyARN}, {AppID: 12, PrivateKeySecretARN: "github-key"}, {AppID: 12, PrivateKeySecretARN: testKeyARN + "*"}, {AppID: 12, PrivateKeySecretARN: "arn:aws:s3:::bucket"}, {AppID: 12, PrivateKeySecretARN: "arn:aws:secretsmanager:ap-northeast-1:abcdefgh1234:secret:key"}} {
		if config.valid() {
			t.Fatal("accepted invalid or broad App configuration")
		}
	}
	config, err := TokenConfigFromEnv(func(key string) string {
		if key == "HAKO_GITHUB_APP_ID" {
			return "12"
		}
		return testKeyARN
	})
	if err != nil || config.AppID != 12 || config.PrivateKeySecretARN != testKeyARN {
		t.Fatal("valid env configuration rejected")
	}
	if _, err := TokenConfigFromEnv(nil); err == nil {
		t.Fatal("nil environment reader accepted")
	}
	if _, err := TokenConfigFromEnv(func(string) string { return "" }); err == nil {
		t.Fatal("missing env configuration accepted")
	}
}

func TestAppJWTHeaderUsesRS256(t *testing.T) {
	key, _ := newSigningKey(t)
	value, err := signAppJWT(key, 12, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(value, ".")
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || !strings.Contains(string(header), `"alg":"RS256"`) {
		t.Fatal("incorrect JWT signing algorithm")
	}
}

func TestTokenClientConstructorBoundsTimeoutAndRequiresDependencies(t *testing.T) {
	secrets := secretAPIFunc(func(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
		return nil, nil
	})
	authorizer := repositoryAuthorizerFunc(func(context.Context, domain.TenantID, int64, int64) (RepositoryAccess, error) {
		return RepositoryAccess{}, nil
	})
	config := TokenConfig{AppID: 12, PrivateKeySecretARN: testKeyARN}
	for _, client := range []*http.Client{nil, {Timeout: time.Minute}, {Timeout: time.Second}} {
		result, err := NewTokenClient(config, secrets, authorizer, client)
		if err != nil || result.http.Timeout <= 0 || result.http.Timeout > 10*time.Second {
			t.Fatal("constructor did not bound HTTP timeout")
		}
	}
	if _, err := NewTokenClient(config, nil, authorizer, nil); err == nil {
		t.Fatal("nil secret API accepted")
	}
	if _, err := NewTokenClient(config, secrets, nil, nil); err == nil {
		t.Fatal("nil authorizer accepted")
	}
	if _, err := NewTokenClient(TokenConfig{}, secrets, authorizer, nil); err == nil {
		t.Fatal("invalid config accepted")
	}
	for _, name := range []string{"", ".", "..", "a/b", "a?b", strings.Repeat("a", 101)} {
		if validCredentialRepositoryName(name) {
			t.Fatal("invalid repository name accepted")
		}
	}
}

func TestTokenClientRateLimitCooldownAndRecovery(t *testing.T) {
	var calls atomic.Int32
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprint(now.Add(2*time.Minute).Unix()))
			w.Header().Set("Retry-After", "90")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		respondCredentials(w, r, now)
	}))
	_, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88)
	requireCredentialCode(t, err, "github_rejected")
	var safe *CredentialError
	_ = errors.As(err, &safe)
	if !safe.RetryAt.Equal(now.Add(2 * time.Minute)) {
		t.Fatal("primary rate limit reset not respected")
	}
	_, err = client.CheckoutToken(context.Background(), "tenant_one", 77, 88)
	requireCredentialCode(t, err, "github_backoff")
	if calls.Load() != 1 {
		t.Fatal("cooldown request reached GitHub")
	}
	client.now = func() time.Time { return now.Add(3 * time.Minute) }
	if _, err = client.CheckoutToken(context.Background(), "tenant_one", 77, 88); err != nil {
		t.Fatal("client did not recover after rate limit reset")
	}
}

func TestCheckoutTokenBoundsCacheAndRechecksMintAuthorization(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { respondCredentials(w, r, now) }))
	for i := 0; i < 128; i++ {
		client.cache[tokenCacheKey{tenant: "tenant_one", appID: 12, installation: 77, repository: int64(1000 + i)}] = InstallationToken{value: testTokenValue, expiresAt: now.Add(time.Duration(i) * time.Second)}
	}
	if _, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88); err != nil {
		t.Fatal(err)
	}
	if len(client.cache) != 128 {
		t.Fatal("cache size is not bounded")
	}
	if _, exists := client.cache[tokenCacheKey{tenant: "tenant_one", appID: 12, installation: 77, repository: 1000}]; exists {
		t.Fatal("oldest cache entry not evicted")
	}
	if err := client.ClearCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	var checks int
	client.authorizer = repositoryAuthorizerFunc(func(context.Context, domain.TenantID, int64, int64) (RepositoryAccess, error) {
		checks++
		if checks == 2 {
			return RepositoryAccess{}, errors.New(testTokenValue)
		}
		return RepositoryAccess{Owner: "acme", Name: "api"}, nil
	})
	_, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88)
	requireCredentialCode(t, err, "repository_not_authorized")
	if len(client.cache) != 0 {
		t.Fatal("token minted during revocation remained cached")
	}
}

func TestCheckoutTokenRejectsMalformedBoundedJSON(t *testing.T) {
	for _, test := range []struct{ name, contentType, body string }{
		{"wrong-content-type", "text/html", testTokenValue},
		{"trailing-json", "application/json", `{"id":77,"app_id":12} {"secret":"` + testTokenValue + `"}`},
		{"oversized", "application/json", strings.Repeat(" ", maxGitHubResponseBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				_, _ = io.WriteString(w, test.body)
			}))
			_, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88)
			requireCredentialCode(t, err, "invalid_github_response")
		})
	}
}

func TestCheckoutTokenRejectsNilBinaryOversizedAndWeakKeys(t *testing.T) {
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal("weak test key generation failed")
	}
	weakPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(weak)}))
	large := strings.Repeat(" ", 65537)
	for _, test := range []struct {
		name     string
		response *secretsmanager.GetSecretValueOutput
		code     string
	}{
		{"nil", nil, "private_key_unavailable"},
		{"binary", &secretsmanager.GetSecretValueOutput{SecretBinary: []byte(testTokenValue)}, "private_key_unavailable"},
		{"oversized", &secretsmanager.GetSecretValueOutput{SecretString: &large}, "private_key_unavailable"},
		{"weak", &secretsmanager.GetSecretValueOutput{SecretString: &weakPEM}, "invalid_private_key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, _ := newTestTokenClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("invalid key request reached GitHub") }))
			client.secrets = secretAPIFunc(func(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
				return test.response, nil
			})
			_, err := client.CheckoutToken(context.Background(), "tenant_one", 77, 88)
			requireCredentialCode(t, err, test.code)
		})
	}
}
