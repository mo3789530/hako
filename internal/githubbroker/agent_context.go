package githubbroker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var ErrAgentIssueContextUnavailable = errors.New("GitHub Agent Issue context unavailable")

const agentIssueContextRoute = "/v1/agent/issue-context"
const agentIssueContextMaxResponse = 512 << 10

var agentIssueIntegrationPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{1,127}$`)
var agentIssueDeliveryPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var agentIssueCommitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

type AgentIssueContextPin struct {
	Authorization githubwebhook.IssueCommentAgentAuthorization
	IntegrationID string
	App           githubapp.TenantPrivateKeyAccess
}

type AgentIssueContextResolver func(context.Context, string, string) (AgentIssueContextPin, error)
type AgentIssueContextReader func(context.Context, AgentIssueContextPin) (githubapp.AgentIssueContext, error)

type AgentIssueContextService struct {
	resolve AgentIssueContextResolver
	read    AgentIssueContextReader
}

func NewAgentIssueContextService(resolve AgentIssueContextResolver, read AgentIssueContextReader) (*AgentIssueContextService, error) {
	if resolve == nil || read == nil {
		return nil, ErrUnavailable
	}
	return &AgentIssueContextService{resolve: resolve, read: read}, nil
}

func (s *AgentIssueContextService) Resolve(ctx context.Context, integrationID, deliveryID string) (githubapp.AgentIssueContext, error) {
	if s == nil || ctx == nil || !agentIssueIntegrationPattern.MatchString(integrationID) || !agentIssueDeliveryPattern.MatchString(deliveryID) || integrationID == "shared" {
		return githubapp.AgentIssueContext{}, ErrAgentIssueContextUnavailable
	}
	first, err := s.resolve(ctx, integrationID, deliveryID)
	if err != nil || !validAgentIssueContextPin(first, integrationID) {
		return githubapp.AgentIssueContext{}, ErrAgentIssueContextUnavailable
	}
	issue, err := s.read(ctx, first)
	if err != nil || issue.IssueNumber != first.Authorization.IssueNumber || !validAgentContext(issue) {
		return githubapp.AgentIssueContext{}, ErrAgentIssueContextUnavailable
	}
	// Revalidate Tenant membership, repository opt-in and the exact App pin
	// after remote GitHub calls so a revoked actor or rotated App cannot result
	// in a returned command context.
	second, err := s.resolve(ctx, integrationID, deliveryID)
	if err != nil || !sameAgentIssueContextPin(first, second) {
		return githubapp.AgentIssueContext{}, ErrAgentIssueContextUnavailable
	}
	return issue, nil
}

func validAgentIssueContextPin(pin AgentIssueContextPin, integrationID string) bool {
	a := pin.Authorization
	return pin.IntegrationID == integrationID && a.TenantID != "" && a.UserID != "" && a.Installation > 0 && a.Repository.GitHubID > 0 &&
		a.IssueNumber > 0 && a.CommentID > 0 && validBrokerLogin(a.Repository.Owner) && validCredentialRepository(a.Repository.Name) &&
		pin.App.TenantID == string(a.TenantID) && pin.App.IntegrationID == integrationID && pin.App.GitHubAppID > 0 &&
		pin.App.ConfigRevision > 0 && pin.App.CredentialGeneration > 0 && pin.App.ReaderRoleARN != "" && pin.App.SecretARN != "" && pin.App.SecretVersionID != ""
}

func validAgentContext(issue githubapp.AgentIssueContext) bool {
	return issue.IssueNumber > 0 && strings.TrimSpace(issue.Title) != "" && len(issue.Title) <= 1024 && len(issue.Body) <= 64<<10 &&
		issue.DefaultRef != "" && len(issue.DefaultRef) <= 255 && agentIssueCommitPattern.MatchString(issue.CommitSHA)
}

func sameAgentIssueContextPin(a, b AgentIssueContextPin) bool {
	return a.Authorization.TenantID == b.Authorization.TenantID && a.Authorization.UserID == b.Authorization.UserID &&
		a.Authorization.Installation == b.Authorization.Installation && a.Authorization.Repository == b.Authorization.Repository &&
		a.Authorization.IssueNumber == b.Authorization.IssueNumber && a.Authorization.CommentID == b.Authorization.CommentID &&
		a.IntegrationID == b.IntegrationID && a.App == b.App
}

func NewPostgresAgentIssueContextResolver(pool transaction.Beginner, policy transaction.Policy) AgentIssueContextResolver {
	return func(ctx context.Context, integrationID, deliveryID string) (AgentIssueContextPin, error) {
		if pool == nil || !agentIssueIntegrationPattern.MatchString(integrationID) || integrationID == "shared" || !agentIssueDeliveryPattern.MatchString(deliveryID) {
			return AgentIssueContextPin{}, ErrAgentIssueContextUnavailable
		}
		return transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (AgentIssueContextPin, error) {
			authorization, allowed, err := githubwebhook.AuthorizeIssueCommentAgentCommand(ctx, tx, integrationID, deliveryID)
			if err != nil || !allowed {
				return AgentIssueContextPin{}, ErrAgentIssueContextUnavailable
			}
			var generation, revision, appID int64
			if err := tx.QueryRow(ctx, `SELECT d.credential_generation,i.config_revision,i.github_app_id
				FROM github_webhook_deliveries d JOIN tenant_github_integrations i ON i.id=d.integration_id AND i.tenant_id=$3
				WHERE d.integration_id=$1 AND d.delivery_id=$2 AND i.status='active'
				AND i.credential_generation=d.credential_generation`, integrationID, deliveryID, authorization.TenantID).
				Scan(&generation, &revision, &appID); err != nil {
				return AgentIssueContextPin{}, ErrAgentIssueContextUnavailable
			}
			credential, err := githubregistry.ResolveActiveAppCredential(ctx, tx, authorization.TenantID, integrationID, revision, generation)
			if err != nil || credential.GitHubAppID != appID || credential.CredentialGeneration != generation || credential.ConfigRevision != revision {
				return AgentIssueContextPin{}, ErrAgentIssueContextUnavailable
			}
			app := githubapp.TenantPrivateKeyAccess{TenantID: string(credential.TenantID), IntegrationID: credential.IntegrationID,
				GitHubAppID: credential.GitHubAppID, ConfigRevision: credential.ConfigRevision, CredentialGeneration: credential.CredentialGeneration,
				ReaderRoleARN: credential.PrivateKeyReaderRoleARN, SecretARN: credential.PrivateKeySecretARN, SecretVersionID: credential.PrivateKeyVersionID}
			pin := AgentIssueContextPin{Authorization: authorization, IntegrationID: integrationID, App: app}
			if !validAgentIssueContextPin(pin, integrationID) {
				return AgentIssueContextPin{}, ErrAgentIssueContextUnavailable
			}
			return pin, nil
		})
	}
}

func NewAWSAgentIssueContextReader(config aws.Config, pool transaction.Beginner, policy transaction.Policy, httpClient *http.Client) (AgentIssueContextReader, error) {
	if strings.TrimSpace(config.Region) == "" || pool == nil {
		return nil, ErrUnavailable
	}
	authorizer := githubregistry.CredentialAuthorizer{Pool: pool, Policy: policy}
	return func(ctx context.Context, pin AgentIssueContextPin) (githubapp.AgentIssueContext, error) {
		if !validAgentIssueContextPin(pin, pin.IntegrationID) {
			return githubapp.AgentIssueContext{}, ErrAgentIssueContextUnavailable
		}
		client, err := githubapp.NewTenantAppTokenClientFromConfig(pin.App, config, authorizer, httpClient)
		if err != nil {
			return githubapp.AgentIssueContext{}, ErrAgentIssueContextUnavailable
		}
		token, repository, err := client.AgentContextToken(ctx, pin.Authorization.TenantID, pin.Authorization.Installation, pin.Authorization.Repository.GitHubID)
		if err != nil || repository.Owner != pin.Authorization.Repository.Owner || repository.Name != pin.Authorization.Repository.Name {
			return githubapp.AgentIssueContext{}, ErrAgentIssueContextUnavailable
		}
		return client.ResolveAgentIssueContext(ctx, token, repository, pin.Authorization.IssueNumber)
	}, nil
}

type agentIssueContextRequest struct {
	IntegrationID string `json:"integration_id"`
	DeliveryID    string `json:"delivery_id"`
}

// WithAgentIssueContext adds the Control Plane-only mTLS endpoint to a broker
// handler. GitHub identifiers are resolved from the signed inbox row; callers
// cannot supply Tenant, Repository, Installation, or commit coordinates.
func WithAgentIssueContext(next http.Handler, service *AgentIssueContextService) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != agentIssueContextRoute {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed)
			return
		}
		caller, err := authenticatedCaller(r)
		if err != nil || caller != controlPlaneURI {
			writeError(w, http.StatusUnauthorized)
			return
		}
		if service == nil || r.Body == nil || r.ContentLength > 1024 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeError(w, http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 1025))
		if err != nil || len(data) > 1024 {
			writeError(w, http.StatusBadRequest)
			return
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		var input agentIssueContextRequest
		if decoder.Decode(&input) != nil || !agentIssueIntegrationPattern.MatchString(input.IntegrationID) || input.IntegrationID == "shared" ||
			!agentIssueDeliveryPattern.MatchString(input.DeliveryID) {
			writeError(w, http.StatusBadRequest)
			return
		}
		var trailing any
		if decoder.Decode(&trailing) != io.EOF {
			writeError(w, http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
		defer cancel()
		issue, err := service.Resolve(ctx, input.IntegrationID, input.DeliveryID)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeResponse(w, issue)
	})
}

type HTTPAgentIssueContextProvider struct {
	base   string
	client *http.Client
}

func NewHTTPAgentIssueContextProvider(baseURL string, client *http.Client) (*HTTPAgentIssueContextProvider, error) {
	validated, err := NewHTTPResolver(baseURL, client)
	if err != nil {
		return nil, err
	}
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if bounded.Timeout <= 0 || bounded.Timeout > 55*time.Second {
		bounded.Timeout = 55 * time.Second
	}
	return &HTTPAgentIssueContextProvider{base: validated.BaseURL, client: &bounded}, nil
}

func NewAgentIssueContextMTLSClient(certificate tls.Certificate, brokerRoots *x509.CertPool) (*http.Client, error) {
	return newMTLSClient(certificate, brokerRoots, 55*time.Second, controlPlaneURI)
}

func (c *HTTPAgentIssueContextProvider) Resolve(ctx context.Context, integrationID, deliveryID string) (githubapp.AgentIssueContext, error) {
	if c == nil || c.client == nil || ctx == nil || !agentIssueIntegrationPattern.MatchString(integrationID) || integrationID == "shared" || !agentIssueDeliveryPattern.MatchString(deliveryID) {
		return githubapp.AgentIssueContext{}, ErrAgentIssueContextUnavailable
	}
	body, err := json.Marshal(agentIssueContextRequest{IntegrationID: integrationID, DeliveryID: deliveryID})
	if err != nil {
		return githubapp.AgentIssueContext{}, ErrAgentIssueContextUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+agentIssueContextRoute, bytes.NewReader(body))
	if err != nil {
		return githubapp.AgentIssueContext{}, ErrAgentIssueContextUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(ctx, 55*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	response, err := c.client.Do(req)
	if err != nil {
		return githubapp.AgentIssueContext{}, ErrAgentIssueContextUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 || response.Header.Get("Cache-Control") != "no-store" {
		return githubapp.AgentIssueContext{}, ErrAgentIssueContextUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, agentIssueContextMaxResponse+1))
	if err != nil || len(data) == 0 || len(data) > agentIssueContextMaxResponse {
		return githubapp.AgentIssueContext{}, ErrAgentIssueContextUnavailable
	}
	defer clear(data)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var issue githubapp.AgentIssueContext
	if decoder.Decode(&issue) != nil || !validAgentContext(issue) {
		return githubapp.AgentIssueContext{}, ErrAgentIssueContextUnavailable
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return githubapp.AgentIssueContext{}, ErrAgentIssueContextUnavailable
	}
	return issue, nil
}
