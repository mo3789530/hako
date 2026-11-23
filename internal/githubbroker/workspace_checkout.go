package githubbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
	workspacestore "github.com/mo3789530/hako/internal/store/workspaces"
)

const workspaceCheckoutRoute = "/v1/workspace-checkout/token"

// WorkspaceCheckoutIdentity is deliberately only a Tenant/Workspace pair.
// Repository coordinates and commit pins are always resolved from Control
// Plane state so a Guest cannot select a broader installation grant.
type WorkspaceCheckoutIdentity struct {
	TenantID        domain.TenantID
	WorkspaceID     domain.WorkspaceID
	ResourcePlaneID domain.ResourcePlaneID
}

type WorkspaceCheckoutResolution struct {
	Identity      WorkspaceCheckoutIdentity
	Access        githubapp.TenantPrivateKeyAccess
	Repository    githubapp.RepositoryAccess
	Install       int64
	RepositoryID  int64
	DefaultBranch string
	CommitSHA     string
}

type WorkspaceCheckoutResolver func(context.Context, string, WorkspaceCheckoutIdentity) (WorkspaceCheckoutResolution, error)
type WorkspaceCheckoutTokenMinter func(context.Context, WorkspaceCheckoutResolution) (string, time.Time, string, error)

type WorkspaceCheckoutService struct {
	resolve WorkspaceCheckoutResolver
	mint    WorkspaceCheckoutTokenMinter
	allowed map[string]struct{}
	now     func() time.Time
}

type WorkspaceCheckoutGrant struct {
	token      string
	expiresAt  time.Time
	repository githubapp.RepositoryAccess
	commitSHA  string
}

func (WorkspaceCheckoutGrant) String() string     { return "Workspace GitHub checkout grant [REDACTED]" }
func (g WorkspaceCheckoutGrant) GoString() string { return g.String() }
func (WorkspaceCheckoutGrant) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED Workspace checkout grant]"`), nil
}
func (g WorkspaceCheckoutGrant) Token() string                          { return g.token }
func (g WorkspaceCheckoutGrant) ExpiresAt() time.Time                   { return g.expiresAt }
func (g WorkspaceCheckoutGrant) Repository() githubapp.RepositoryAccess { return g.repository }
func (g WorkspaceCheckoutGrant) CommitSHA() string                      { return g.commitSHA }

func NewWorkspaceCheckoutService(resourcePlaneIDs []string, resolve WorkspaceCheckoutResolver, mint WorkspaceCheckoutTokenMinter) (*WorkspaceCheckoutService, error) {
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
	return &WorkspaceCheckoutService{resolve: resolve, mint: mint, allowed: allowed, now: time.Now}, nil
}

// NewPostgresWorkspaceCheckoutResolver authorizes only the repository pinned
// to a non-deleted Workspace placed on the authenticated Resource Plane while
// its checkout is pending, failed, or ready. Ready access supports idempotent
// Ensure redelivery at the already pinned commit. It re-reads Tenant App state.
func NewPostgresWorkspaceCheckoutResolver(pool transaction.Beginner, policy transaction.Policy, now func() time.Time) WorkspaceCheckoutResolver {
	if now == nil {
		now = time.Now
	}
	return func(ctx context.Context, caller string, identity WorkspaceCheckoutIdentity) (WorkspaceCheckoutResolution, error) {
		planeID, ok := previewCheckoutCallerPlane(caller)
		if pool == nil || !ok || planeID != identity.ResourcePlaneID || identity.TenantID == "" || identity.WorkspaceID == "" {
			return WorkspaceCheckoutResolution{}, ErrCheckoutDenied
		}
		resolved, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (WorkspaceCheckoutResolution, error) {
			var value WorkspaceCheckoutResolution
			var state string
			err := tx.QueryRow(ctx, `SELECT c.tenant_id,c.workspace_id,p.resource_plane_id,c.installation_id,c.github_repository_id,
				c.owner_login,c.repository_name,c.default_branch,COALESCE(c.commit_sha,''),c.checkout_state,
				i.id,i.config_revision,i.credential_generation,i.github_app_id,credential.private_key_reader_role_arn,
				credential.private_key_secret_ref,credential.private_key_secret_version_id
				FROM workspace_github_checkouts c
				JOIN workspaces w ON w.tenant_id=c.tenant_id AND w.id=c.workspace_id
				JOIN placements p ON p.workspace_id=w.id AND p.resource_plane_id=$3
				JOIN workspace_status s ON s.workspace_id=w.id AND s.desired_state <> 'deleted' AND s.observed_state <> 'deleted'
				JOIN tenant_github_app_installation_bindings b ON b.tenant_id=c.tenant_id AND b.installation_id=c.installation_id
				JOIN tenant_github_installations installation ON installation.tenant_id=b.tenant_id AND installation.installation_id=b.installation_id
				JOIN tenant_github_integrations i ON i.tenant_id=b.tenant_id AND i.id=b.integration_id
				JOIN github_app_registrations registration ON registration.id=i.app_registration_id AND registration.github_app_id=i.github_app_id
				  AND registration.mode='tenant_owned' AND registration.owner_tenant_id=c.tenant_id
				JOIN github_credential_bindings credential ON credential.tenant_id=i.tenant_id AND credential.id=i.credential_binding_id AND credential.github_app_id=i.github_app_id
				JOIN tenant_github_repositories repository ON repository.tenant_id=c.tenant_id AND repository.installation_id=c.installation_id
				  AND repository.github_repository_id=c.github_repository_id AND repository.owner_login=c.owner_login AND repository.repository_name=c.repository_name
				WHERE c.tenant_id=$1 AND c.workspace_id=$2 AND c.checkout_state IN ('pending','failed','ready')
			  AND i.status='active' AND credential.status='approved'
				  AND installation.status='active' AND installation.suspended_at IS NULL
				  AND b.integration_id=i.id AND i.config_revision>0 AND i.credential_generation>0`,
				identity.TenantID, identity.WorkspaceID, identity.ResourcePlaneID).Scan(
				&value.Identity.TenantID, &value.Identity.WorkspaceID, &value.Identity.ResourcePlaneID,
				&value.Install, &value.RepositoryID, &value.Repository.Owner, &value.Repository.Name, &value.DefaultBranch, &value.CommitSHA, &state,
				&value.Access.IntegrationID, &value.Access.ConfigRevision, &value.Access.CredentialGeneration, &value.Access.GitHubAppID,
				&value.Access.ReaderRoleARN, &value.Access.SecretARN, &value.Access.SecretVersionID)
			if err != nil || state != "pending" && state != "failed" && state != "ready" {
				return WorkspaceCheckoutResolution{}, ErrCheckoutDenied
			}
			credential, err := githubregistry.ResolveActiveAppCredential(ctx, tx, identity.TenantID,
				value.Access.IntegrationID, value.Access.ConfigRevision, value.Access.CredentialGeneration)
			if err != nil {
				return WorkspaceCheckoutResolution{}, ErrCheckoutDenied
			}
			value.Access.TenantID = string(identity.TenantID)
			value.Access.GitHubAppID = credential.GitHubAppID
			value.Access.ReaderRoleARN = credential.PrivateKeyReaderRoleARN
			value.Access.SecretARN = credential.PrivateKeySecretARN
			value.Access.SecretVersionID = credential.PrivateKeyVersionID
			return value, nil
		})
		if err != nil {
			return WorkspaceCheckoutResolution{}, ErrCheckoutDenied
		}
		return resolved, nil
	}
}

func NewAWSWorkspaceCheckoutTokenMinter(config aws.Config, pool transaction.Beginner, policy transaction.Policy, httpClient *http.Client) (WorkspaceCheckoutTokenMinter, error) {
	if strings.TrimSpace(config.Region) == "" || pool == nil {
		return nil, ErrUnavailable
	}
	authorizer := githubregistry.CredentialAuthorizer{Pool: pool, Policy: policy}
	return func(ctx context.Context, resolved WorkspaceCheckoutResolution) (string, time.Time, string, error) {
		client, err := githubapp.NewTenantAppTokenClientFromConfig(resolved.Access, config, authorizer, httpClient)
		if err != nil {
			return "", time.Time{}, "", ErrUnavailable
		}
		token, err := client.CheckoutToken(ctx, resolved.Identity.TenantID, resolved.Install, resolved.RepositoryID)
		if err != nil {
			return "", time.Time{}, "", ErrUnavailable
		}
		if resolved.CommitSHA != "" {
			return token.Value(), token.ExpiresAt(), resolved.CommitSHA, nil
		}
		head, err := client.ResolveRepositoryDefaultBranch(ctx, token, resolved.Repository)
		if err != nil || head.Name != resolved.DefaultBranch {
			return "", time.Time{}, "", ErrUnavailable
		}
		if err := workspacestore.PinWorkspaceRepositoryCommit(ctx, pool, policy, resolved.Identity.TenantID,
			resolved.Identity.WorkspaceID, head.CommitSHA, time.Now().UTC()); err != nil {
			return "", time.Time{}, "", ErrUnavailable
		}
		return token.Value(), token.ExpiresAt(), head.CommitSHA, nil
	}, nil
}

func (s *WorkspaceCheckoutService) Issue(ctx context.Context, caller string, identity WorkspaceCheckoutIdentity) (WorkspaceCheckoutGrant, error) {
	planeID, ok := previewCheckoutCallerPlane(caller)
	if s == nil || ctx == nil || !ok || planeID != identity.ResourcePlaneID {
		return WorkspaceCheckoutGrant{}, ErrCheckoutDenied
	}
	if _, ok := s.allowed[caller]; !ok {
		return WorkspaceCheckoutGrant{}, ErrCheckoutDenied
	}
	first, err := s.resolve(ctx, caller, identity)
	if err != nil || first.Identity != identity || first.CommitSHA != "" && !checkoutCommitPattern.MatchString(first.CommitSHA) || first.DefaultBranch == "" ||
		!accessMatchesPreviewIdentity(first.Access, PreviewCheckoutIdentity{TenantID: identity.TenantID}) ||
		!validBrokerLogin(first.Repository.Owner) || !validCredentialRepository(first.Repository.Name) || first.Install < 1 || first.RepositoryID < 1 {
		return WorkspaceCheckoutGrant{}, ErrCheckoutDenied
	}
	token, expiresAt, commitSHA, err := s.mint(ctx, first)
	if err != nil || !validBrokerToken(token, expiresAt, s.now()) || !checkoutCommitPattern.MatchString(commitSHA) ||
		first.CommitSHA != "" && first.CommitSHA != commitSHA {
		return WorkspaceCheckoutGrant{}, ErrCheckoutDenied
	}
	current, err := s.resolve(ctx, caller, identity)
	if err != nil || current.Identity != first.Identity || current.Access != first.Access || current.Repository != first.Repository ||
		current.Install != first.Install || current.RepositoryID != first.RepositoryID || current.DefaultBranch != first.DefaultBranch || current.CommitSHA != commitSHA {
		return WorkspaceCheckoutGrant{}, ErrCheckoutDenied
	}
	return WorkspaceCheckoutGrant{token: token, expiresAt: expiresAt, repository: first.Repository, commitSHA: commitSHA}, nil
}

type workspaceCheckoutRequest struct {
	TenantID    domain.TenantID    `json:"tenant_id"`
	WorkspaceID domain.WorkspaceID `json:"workspace_id"`
}

func WithWorkspaceCheckout(next http.Handler, service *WorkspaceCheckoutService) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != workspaceCheckoutRoute {
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
		body, err := io.ReadAll(io.LimitReader(r.Body, 2049))
		var input workspaceCheckoutRequest
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err != nil || len(body) > 2048 || decoder.Decode(&input) != nil || decoder.Decode(&struct{}{}) != io.EOF || input.TenantID == "" || input.WorkspaceID == "" {
			writeError(w, http.StatusBadRequest)
			return
		}
		planeID, ok := previewCheckoutCallerPlane(caller)
		if !ok {
			writeError(w, http.StatusUnauthorized)
			return
		}
		grant, err := service.Issue(r.Context(), caller, WorkspaceCheckoutIdentity{TenantID: input.TenantID, WorkspaceID: input.WorkspaceID, ResourcePlaneID: planeID})
		if err != nil {
			writeError(w, http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeResponse(w, struct {
			Token      string                     `json:"token"`
			ExpiresAt  time.Time                  `json:"expires_at"`
			Repository githubapp.RepositoryAccess `json:"repository"`
			CommitSHA  string                     `json:"commit_sha"`
		}{
			Token: grant.Token(), ExpiresAt: grant.ExpiresAt(), Repository: grant.Repository(), CommitSHA: grant.CommitSHA()})
	})
}

// HTTPWorkspaceCheckoutProvider is the Resource Plane's checkout-only mTLS
// client. The client certificate carries the Plane identity; the JSON request
// carries only Tenant and Workspace IDs.
type HTTPWorkspaceCheckoutProvider struct {
	base   string
	client *http.Client
	now    func() time.Time
}

func NewHTTPWorkspaceCheckoutProvider(baseURL string, client *http.Client) (*HTTPWorkspaceCheckoutProvider, error) {
	validated, err := NewHTTPResolver(baseURL, client)
	if err != nil {
		return nil, err
	}
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if bounded.Timeout <= 0 || bounded.Timeout > 50*time.Second {
		bounded.Timeout = 50 * time.Second
	}
	return &HTTPWorkspaceCheckoutProvider{base: validated.BaseURL, client: &bounded, now: time.Now}, nil
}

func (c *HTTPWorkspaceCheckoutProvider) WorkspaceCheckoutToken(ctx context.Context, identity WorkspaceCheckoutIdentity) (WorkspaceCheckoutGrant, error) {
	if c == nil || c.client == nil || ctx == nil || identity.TenantID == "" || identity.WorkspaceID == "" ||
		!resourcePlaneIDPattern.MatchString(string(identity.ResourcePlaneID)) {
		return WorkspaceCheckoutGrant{}, ErrUnavailable
	}
	body, err := json.Marshal(workspaceCheckoutRequest{TenantID: identity.TenantID, WorkspaceID: identity.WorkspaceID})
	if err != nil || len(body) > 2048 {
		return WorkspaceCheckoutGrant{}, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+workspaceCheckoutRoute, bytes.NewReader(body))
	if err != nil {
		return WorkspaceCheckoutGrant{}, ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		return WorkspaceCheckoutGrant{}, ErrUnavailable
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, brokerMaxBody+1))
	if err != nil || len(data) > brokerMaxBody || res.StatusCode < 200 || res.StatusCode >= 300 || res.Header.Get("Cache-Control") != "no-store" {
		return WorkspaceCheckoutGrant{}, ErrUnavailable
	}
	defer clear(data)
	var out struct {
		Token      string              `json:"token"`
		ExpiresAt  time.Time           `json:"expires_at"`
		Repository *repositoryResponse `json:"repository"`
		CommitSHA  string              `json:"commit_sha"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&out) != nil || decoder.Decode(new(any)) != io.EOF || out.Repository == nil ||
		!validBrokerToken(out.Token, out.ExpiresAt, c.now()) || out.ExpiresAt.After(c.now().Add(65*time.Minute)) ||
		!validBrokerLogin(out.Repository.Owner) || !validCredentialRepository(out.Repository.Name) || !checkoutCommitPattern.MatchString(out.CommitSHA) {
		return WorkspaceCheckoutGrant{}, ErrUnavailable
	}
	return WorkspaceCheckoutGrant{token: out.Token, expiresAt: out.ExpiresAt, repository: githubapp.RepositoryAccess{Owner: out.Repository.Owner, Name: out.Repository.Name}, commitSHA: strings.ToLower(out.CommitSHA)}, nil
}
