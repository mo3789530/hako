package githubbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var previewClaimIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

const previewCheckoutRoute = "/v1/preview-checkout/token"

type PreviewCheckoutIdentity struct {
	TenantID        domain.TenantID
	InstallationID  int64
	RepositoryID    int64
	PullRequest     int64
	Generation      int64
	ClaimID         string
	ClaimAttempt    int
	ResourcePlaneID domain.ResourcePlaneID
	HeadSHA         string
}

type PreviewCheckoutResolution struct {
	Identity   PreviewCheckoutIdentity
	Access     githubapp.TenantPrivateKeyAccess
	Repository githubapp.RepositoryAccess
}

type PreviewCheckoutResolver func(context.Context, string, PreviewCheckoutIdentity) (PreviewCheckoutResolution, error)
type PreviewCheckoutTokenMinter func(context.Context, PreviewCheckoutIdentity, githubapp.TenantPrivateKeyAccess) (string, time.Time, error)

type PreviewCheckoutService struct {
	resolve PreviewCheckoutResolver
	mint    PreviewCheckoutTokenMinter
	allowed map[string]struct{}
	now     func() time.Time
}

type PreviewCheckoutGrant struct {
	token      string
	expiresAt  time.Time
	repository githubapp.RepositoryAccess
}

func (PreviewCheckoutGrant) String() string     { return "Preview GitHub checkout grant [REDACTED]" }
func (g PreviewCheckoutGrant) GoString() string { return g.String() }
func (PreviewCheckoutGrant) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED Preview GitHub checkout grant]"`), nil
}
func (g PreviewCheckoutGrant) Token() string                          { return g.token }
func (g PreviewCheckoutGrant) ExpiresAt() time.Time                   { return g.expiresAt }
func (g PreviewCheckoutGrant) Repository() githubapp.RepositoryAccess { return g.repository }

func NewPreviewCheckoutService(resourcePlaneIDs []string, resolve PreviewCheckoutResolver, mint PreviewCheckoutTokenMinter) (*PreviewCheckoutService, error) {
	if len(resourcePlaneIDs) == 0 || resolve == nil || mint == nil {
		return nil, ErrUnavailable
	}
	allowed := make(map[string]struct{}, len(resourcePlaneIDs))
	for _, id := range resourcePlaneIDs {
		caller, err := ResourcePlaneCallerID(id)
		if err != nil {
			return nil, ErrUnavailable
		}
		if _, exists := allowed[caller]; exists {
			return nil, ErrUnavailable
		}
		allowed[caller] = struct{}{}
	}
	return &PreviewCheckoutService{resolve: resolve, mint: mint, allowed: allowed, now: time.Now}, nil
}

// NewPostgresPreviewCheckoutResolver permits checkout only while the exact
// current Preview generation has an active provisioning claim on the calling
// Resource Plane. The App identity is read from the immutable generation pin.
func NewPostgresPreviewCheckoutResolver(pool transaction.Beginner, policy transaction.Policy, now func() time.Time) PreviewCheckoutResolver {
	if now == nil {
		now = time.Now
	}
	return func(ctx context.Context, caller string, identity PreviewCheckoutIdentity) (PreviewCheckoutResolution, error) {
		planeID, ok := previewCheckoutCallerPlane(caller)
		if pool == nil || !ok || planeID != identity.ResourcePlaneID || !validPreviewCheckoutIdentity(identity) {
			return PreviewCheckoutResolution{}, ErrCheckoutDenied
		}
		value, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (PreviewCheckoutResolution, error) {
			var resolved PreviewCheckoutResolution
			err := tx.QueryRow(ctx, `SELECT a.tenant_id,a.installation_id,a.github_repository_id,a.pull_request_number,
				a.generation,a.provision_claim_id,a.provision_attempts,a.target_resource_plane_id,a.head_sha,
				a.github_integration_id,a.github_config_revision,a.github_credential_generation,repository.owner_login,repository.repository_name
				FROM repository_preview_runtime_allocations a
				JOIN repository_previews p ON p.tenant_id=a.tenant_id AND p.installation_id=a.installation_id
				  AND p.github_repository_id=a.github_repository_id AND p.pull_request_number=a.pull_request_number
				JOIN tenant_github_app_installation_bindings binding ON binding.tenant_id=a.tenant_id
				  AND binding.installation_id=a.installation_id AND binding.integration_id=a.github_integration_id
				JOIN tenant_github_installations installation ON installation.tenant_id=a.tenant_id AND installation.installation_id=a.installation_id
				JOIN tenant_github_repositories repository ON repository.tenant_id=a.tenant_id AND repository.installation_id=a.installation_id
				  AND repository.github_repository_id=a.github_repository_id
				JOIN tenant_github_integrations integration ON integration.tenant_id=a.tenant_id AND integration.id=a.github_integration_id
				JOIN github_app_registrations registration ON registration.id=integration.app_registration_id
				  AND registration.github_app_id=integration.github_app_id AND registration.mode='tenant_owned'
				  AND registration.owner_tenant_id=a.tenant_id
				JOIN github_credential_bindings credential ON credential.tenant_id=integration.tenant_id
				  AND credential.id=integration.credential_binding_id AND credential.github_app_id=integration.github_app_id
				WHERE a.tenant_id=$1 AND a.installation_id=$2 AND a.github_repository_id=$3 AND a.pull_request_number=$4
				  AND a.generation=$5 AND a.provision_claim_id=$6 AND a.provision_attempts=$7
				  AND a.target_resource_plane_id=$8 AND a.state='provisioning' AND a.desired_state='present'
				  AND a.provision_lease_expires_at>$9 AND a.expires_at>$9 AND a.head_sha=$10
				  AND p.generation=a.generation AND p.desired_state='present' AND p.expires_at>$9 AND p.head_sha=a.head_sha
				  AND a.github_integration_id IS NOT NULL AND a.github_config_revision>0 AND a.github_credential_generation>0
				  AND integration.status='active' AND integration.config_revision=a.github_config_revision
				  AND integration.credential_generation=a.github_credential_generation
				  AND credential.status='approved' AND installation.status='active' AND installation.suspended_at IS NULL`,
				identity.TenantID, identity.InstallationID, identity.RepositoryID, identity.PullRequest, identity.Generation,
				identity.ClaimID, identity.ClaimAttempt, identity.ResourcePlaneID, now().UTC(), identity.HeadSHA).
				Scan(&resolved.Identity.TenantID, &resolved.Identity.InstallationID, &resolved.Identity.RepositoryID,
					&resolved.Identity.PullRequest, &resolved.Identity.Generation, &resolved.Identity.ClaimID,
					&resolved.Identity.ClaimAttempt, &resolved.Identity.ResourcePlaneID, &resolved.Identity.HeadSHA,
					&resolved.Access.IntegrationID, &resolved.Access.ConfigRevision, &resolved.Access.CredentialGeneration,
					&resolved.Repository.Owner, &resolved.Repository.Name)
			if err != nil {
				return PreviewCheckoutResolution{}, ErrCheckoutDenied
			}
			credential, err := githubregistry.ResolveActiveAppCredential(ctx, tx, identity.TenantID,
				resolved.Access.IntegrationID, resolved.Access.ConfigRevision, resolved.Access.CredentialGeneration)
			if err != nil {
				return PreviewCheckoutResolution{}, ErrCheckoutDenied
			}
			resolved.Access.TenantID = string(identity.TenantID)
			resolved.Access.GitHubAppID = credential.GitHubAppID
			resolved.Access.ReaderRoleARN = credential.PrivateKeyReaderRoleARN
			resolved.Access.SecretARN = credential.PrivateKeySecretARN
			resolved.Access.SecretVersionID = credential.PrivateKeyVersionID
			return resolved, nil
		})
		if err != nil {
			return PreviewCheckoutResolution{}, ErrCheckoutDenied
		}
		return value, nil
	}
}

func NewAWSPreviewCheckoutTokenMinter(config aws.Config, pool transaction.Beginner, policy transaction.Policy, httpClient *http.Client) (PreviewCheckoutTokenMinter, error) {
	if strings.TrimSpace(config.Region) == "" || pool == nil {
		return nil, ErrUnavailable
	}
	authorizer := githubregistry.CredentialAuthorizer{Pool: pool, Policy: policy}
	return func(ctx context.Context, identity PreviewCheckoutIdentity, access githubapp.TenantPrivateKeyAccess) (string, time.Time, error) {
		client, err := githubapp.NewTenantAppTokenClientFromConfig(access, config, authorizer, httpClient)
		if err != nil {
			return "", time.Time{}, ErrUnavailable
		}
		token, err := client.CheckoutToken(ctx, identity.TenantID, identity.InstallationID, identity.RepositoryID)
		if err != nil {
			return "", time.Time{}, ErrUnavailable
		}
		return token.Value(), token.ExpiresAt(), nil
	}, nil
}

func (s *PreviewCheckoutService) Issue(ctx context.Context, caller string, identity PreviewCheckoutIdentity) (PreviewCheckoutGrant, error) {
	if s == nil || ctx == nil || !validPreviewCheckoutIdentity(identity) {
		return PreviewCheckoutGrant{}, ErrCheckoutDenied
	}
	callerPlane, validCaller := previewCheckoutCallerPlane(caller)
	if !validCaller || callerPlane != identity.ResourcePlaneID {
		return PreviewCheckoutGrant{}, ErrCheckoutDenied
	}
	if _, ok := s.allowed[caller]; !ok {
		return PreviewCheckoutGrant{}, ErrCheckoutDenied
	}
	first, err := s.resolve(ctx, caller, identity)
	if err != nil || !samePreviewCheckoutIdentity(first.Identity, identity) || !accessMatchesPreviewIdentity(first.Access, first.Identity) ||
		!validBrokerLogin(first.Repository.Owner) || !validCredentialRepository(first.Repository.Name) {
		return PreviewCheckoutGrant{}, ErrCheckoutDenied
	}
	token, expiresAt, err := s.mint(ctx, first.Identity, first.Access)
	if err != nil || !validBrokerToken(token, expiresAt, s.now()) {
		return PreviewCheckoutGrant{}, ErrCheckoutDenied
	}
	current, err := s.resolve(ctx, caller, identity)
	if err != nil || !samePreviewCheckoutIdentity(first.Identity, current.Identity) || first.Access != current.Access || first.Repository != current.Repository {
		return PreviewCheckoutGrant{}, ErrCheckoutDenied
	}
	return PreviewCheckoutGrant{token: token, expiresAt: expiresAt, repository: first.Repository}, nil
}

type previewCheckoutRequest struct {
	TenantID       domain.TenantID `json:"tenant_id"`
	InstallationID int64           `json:"installation_id"`
	RepositoryID   int64           `json:"repository_id"`
	PullRequest    int64           `json:"pull_request_number"`
	Generation     int64           `json:"generation"`
	ClaimID        string          `json:"claim_id"`
	ClaimAttempt   int             `json:"claim_attempt"`
	HeadSHA        string          `json:"head_sha"`
}

func WithPreviewCheckout(next http.Handler, service *PreviewCheckoutService) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != previewCheckoutRoute {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed)
			return
		}
		caller, err := authenticatedResourcePlaneCaller(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized)
			return
		}
		if service == nil || r.Body == nil || r.ContentLength > 2048 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeError(w, http.StatusBadRequest)
			return
		}
		input, err := decodePreviewCheckoutRequest(r)
		if err != nil {
			writeError(w, http.StatusBadRequest)
			return
		}
		identity := PreviewCheckoutIdentity{TenantID: input.TenantID, InstallationID: input.InstallationID,
			RepositoryID: input.RepositoryID, PullRequest: input.PullRequest, Generation: input.Generation,
			ClaimID: input.ClaimID, ClaimAttempt: input.ClaimAttempt, HeadSHA: input.HeadSHA}
		planeID, ok := previewCheckoutCallerPlane(caller)
		if !ok {
			writeError(w, http.StatusUnauthorized)
			return
		}
		identity.ResourcePlaneID = planeID
		grant, err := service.Issue(r.Context(), caller, identity)
		if err != nil {
			writeError(w, http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		repository := grant.Repository()
		writeResponse(w, response{Token: grant.Token(), ExpiresAt: grant.ExpiresAt(), Repository: &repositoryResponse{Owner: repository.Owner, Name: repository.Name}})
	})
}

func decodePreviewCheckoutRequest(r *http.Request) (previewCheckoutRequest, error) {
	var input previewCheckoutRequest
	if r == nil || r.Body == nil {
		return input, errors.New("missing Preview checkout request")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 2049))
	if err != nil || len(body) > 2048 {
		return input, errors.New("Preview checkout request is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return input, errors.New("Preview checkout request must contain one JSON object")
	}
	if !validPreviewCheckoutIdentity(PreviewCheckoutIdentity{TenantID: input.TenantID, InstallationID: input.InstallationID,
		RepositoryID: input.RepositoryID, PullRequest: input.PullRequest, Generation: input.Generation,
		ClaimID: input.ClaimID, ClaimAttempt: input.ClaimAttempt, HeadSHA: input.HeadSHA}) {
		return input, ErrCheckoutDenied
	}
	return input, nil
}

func validPreviewCheckoutIdentity(identity PreviewCheckoutIdentity) bool {
	return identity.TenantID != "" && identity.InstallationID > 0 && identity.RepositoryID > 0 && identity.PullRequest > 0 &&
		identity.Generation > 0 && previewClaimIDPattern.MatchString(identity.ClaimID) && identity.ClaimAttempt > 0 &&
		identity.ClaimAttempt <= 1_000_000 && checkoutCommitPattern.MatchString(identity.HeadSHA) &&
		(identity.ResourcePlaneID == "" || resourcePlaneIDPattern.MatchString(string(identity.ResourcePlaneID)))
}

func samePreviewCheckoutIdentity(a, b PreviewCheckoutIdentity) bool {
	return a.TenantID == b.TenantID && a.InstallationID == b.InstallationID && a.RepositoryID == b.RepositoryID &&
		a.PullRequest == b.PullRequest && a.Generation == b.Generation && a.ClaimID == b.ClaimID &&
		a.ClaimAttempt == b.ClaimAttempt && a.ResourcePlaneID == b.ResourcePlaneID && a.HeadSHA == b.HeadSHA
}

func accessMatchesPreviewIdentity(access githubapp.TenantPrivateKeyAccess, identity PreviewCheckoutIdentity) bool {
	return access.TenantID == string(identity.TenantID) && access.IntegrationID != "" && access.GitHubAppID > 0 &&
		access.ConfigRevision > 0 && access.CredentialGeneration > 0 && access.ReaderRoleARN != "" &&
		access.SecretARN != "" && access.SecretVersionID != ""
}

func previewCheckoutCallerPlane(caller string) (domain.ResourcePlaneID, bool) {
	if !strings.HasPrefix(caller, resourcePlaneURIPrefix) {
		return "", false
	}
	id := strings.TrimPrefix(caller, resourcePlaneURIPrefix)
	validated, err := ResourcePlaneCallerID(id)
	if err != nil || validated != caller {
		return "", false
	}
	return domain.ResourcePlaneID(id), true
}

// HTTPPreviewCheckoutProvider is the Resource Plane's mTLS client for
// generation- and claim-bound Preview source checkout credentials.
type HTTPPreviewCheckoutProvider struct {
	base   string
	client *http.Client
}

func NewHTTPPreviewCheckoutProvider(baseURL string, client *http.Client) (*HTTPPreviewCheckoutProvider, error) {
	validated, err := NewHTTPResolver(baseURL, client)
	if err != nil {
		return nil, err
	}
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if bounded.Timeout <= 0 || bounded.Timeout > 50*time.Second {
		bounded.Timeout = 50 * time.Second
	}
	return &HTTPPreviewCheckoutProvider{base: validated.BaseURL, client: &bounded}, nil
}

func (c *HTTPPreviewCheckoutProvider) CheckoutPreviewToken(ctx context.Context, identity PreviewCheckoutIdentity) (PreviewCheckoutGrant, error) {
	if c == nil || c.client == nil || ctx == nil || !validPreviewCheckoutIdentity(identity) ||
		!resourcePlaneIDPattern.MatchString(string(identity.ResourcePlaneID)) {
		return PreviewCheckoutGrant{}, ErrUnavailable
	}
	body, err := json.Marshal(previewCheckoutRequest{TenantID: identity.TenantID, InstallationID: identity.InstallationID,
		RepositoryID: identity.RepositoryID, PullRequest: identity.PullRequest, Generation: identity.Generation,
		ClaimID: identity.ClaimID, ClaimAttempt: identity.ClaimAttempt, HeadSHA: identity.HeadSHA})
	if err != nil || len(body) > 2048 {
		return PreviewCheckoutGrant{}, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+previewCheckoutRoute, bytes.NewReader(body))
	if err != nil {
		return PreviewCheckoutGrant{}, ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		return PreviewCheckoutGrant{}, ErrUnavailable
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, brokerMaxBody+1))
	if err != nil || len(data) > brokerMaxBody || res.StatusCode < 200 || res.StatusCode >= 300 || res.Header.Get("Cache-Control") != "no-store" {
		return PreviewCheckoutGrant{}, ErrUnavailable
	}
	defer clear(data)
	var out response
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&out) != nil || decoder.Decode(new(any)) != io.EOF || out.Repository == nil ||
		!validBrokerToken(out.Token, out.ExpiresAt, time.Now()) || out.ExpiresAt.After(time.Now().Add(65*time.Minute)) ||
		!validBrokerLogin(out.Repository.Owner) || !validCredentialRepository(out.Repository.Name) {
		return PreviewCheckoutGrant{}, ErrUnavailable
	}
	return PreviewCheckoutGrant{token: out.Token, expiresAt: out.ExpiresAt,
		repository: githubapp.RepositoryAccess{Owner: out.Repository.Owner, Name: out.Repository.Name}}, nil
}
