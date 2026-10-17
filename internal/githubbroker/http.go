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
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
)

const controlPlaneURI = "spiffe://hako/control-plane"
const resourcePlaneURIPrefix = "spiffe://hako/resource-plane/"
const brokerMaxBody = 16 << 10

var resourcePlaneIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

type request struct {
	Pin            githubregistry.SetupAppPin `json:"pin"`
	TenantID       domain.TenantID            `json:"tenant_id,omitempty"`
	RunID          domain.WorkloadRunID       `json:"run_id,omitempty"`
	State          string                     `json:"state,omitempty"`
	CodeVerifier   string                     `json:"code_verifier,omitempty"`
	Code           string                     `json:"code,omitempty"`
	InstallationID int64                      `json:"installation_id,omitempty"`
}
type response struct {
	SetupURL         string                          `json:"setup_url,omitempty"`
	AuthorizationURL string                          `json:"authorization_url,omitempty"`
	Identity         *githubapp.InstallationIdentity `json:"identity,omitempty"`
	Token            string                          `json:"token,omitempty"`
	ExpiresAt        time.Time                       `json:"expires_at,omitempty"`
	Repository       *repositoryResponse             `json:"repository,omitempty"`
}

type repositoryResponse struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

type checkoutRequest struct {
	TenantID domain.TenantID      `json:"tenant_id"`
	RunID    domain.WorkloadRunID `json:"run_id"`
}

type actionsRunnerTokenRequest struct {
	IntegrationID string `json:"integration_id"`
	WorkflowJobID int64  `json:"workflow_job_id"`
	RunAttempt    int64  `json:"run_attempt"`
	ClaimID       string `json:"claim_id"`
}

type actionsRunnerCleanupRequest struct {
	IntegrationID string `json:"integration_id"`
	WorkflowJobID int64  `json:"workflow_job_id"`
	RunAttempt    int64  `json:"run_attempt"`
	ClaimID       string `json:"claim_id"`
}

// ServerTLSConfig requires a client cert rooted in the supplied CA. The
// handler also checks the workload URI SAN before accepting any request.
func ServerTLSConfig(clientCAs *x509.CertPool) (*tls.Config, error) {
	if clientCAs == nil {
		return nil, ErrUnavailable
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCAs}, nil
}

// NewMTLSClient creates the client used by the API adapter. The supplied
// certificate is a workload certificate whose URI SAN must be controlPlaneURI.
func NewMTLSClient(certificate tls.Certificate, brokerRoots *x509.CertPool) (*http.Client, error) {
	return newMTLSClient(certificate, brokerRoots, 6*time.Second, controlPlaneURI)
}

// NewCheckoutMTLSClient allows the broker's bounded GitHub API exchange while
// retaining the same TLS identity, trust root, TLS version and no-redirect
// behavior as the shorter OAuth control-plane client.
func NewCheckoutMTLSClient(certificate tls.Certificate, brokerRoots *x509.CertPool) (*http.Client, error) {
	return newMTLSClient(certificate, brokerRoots, 50*time.Second, controlPlaneURI)
}

// NewResourcePlaneCheckoutMTLSClient creates the checkout-only client for one
// explicitly named Resource Plane identity. It cannot be used for OAuth or
// GitHub Checks APIs, which remain Control Plane-only.
func NewResourcePlaneCheckoutMTLSClient(resourcePlaneID string, certificate tls.Certificate, brokerRoots *x509.CertPool) (*http.Client, error) {
	caller, err := ResourcePlaneCallerID(resourcePlaneID)
	if err != nil {
		return nil, err
	}
	return newMTLSClient(certificate, brokerRoots, 50*time.Second, caller)
}

func NewChecksMTLSClient(certificate tls.Certificate, brokerRoots *x509.CertPool) (*http.Client, error) {
	return newMTLSClient(certificate, brokerRoots, 50*time.Second, controlPlaneURI)
}

func NewActionsRunnerMTLSClient(certificate tls.Certificate, brokerRoots *x509.CertPool) (*http.Client, error) {
	return newMTLSClient(certificate, brokerRoots, 50*time.Second, controlPlaneURI)
}

func NewActionsRunnerCleanupMTLSClient(resourcePlaneID string, certificate tls.Certificate, brokerRoots *x509.CertPool) (*http.Client, error) {
	caller, err := ResourcePlaneCallerID(resourcePlaneID)
	if err != nil {
		return nil, err
	}
	return newMTLSClient(certificate, brokerRoots, 50*time.Second, caller)
}

func newMTLSClient(certificate tls.Certificate, brokerRoots *x509.CertPool, timeout time.Duration, expectedCaller string) (*http.Client, error) {
	if len(certificate.Certificate) == 0 || brokerRoots == nil || certificate.Leaf == nil {
		return nil, ErrUnavailable
	}
	validIdentity := false
	for _, uri := range certificate.Leaf.URIs {
		if uri.String() == expectedCaller {
			validIdentity = true
		}
	}
	if !validIdentity {
		return nil, ErrUnavailable
	}
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: brokerRoots, Certificates: []tls.Certificate{certificate}},
		Proxy:           nil, DisableKeepAlives: false, MaxIdleConnsPerHost: 8,
	}}, nil
}

// ResourcePlaneCallerID returns the SPIFFE identity admitted for the
// checkout-only Resource Plane client.
func ResourcePlaneCallerID(resourcePlaneID string) (string, error) {
	if !resourcePlaneIDPattern.MatchString(resourcePlaneID) {
		return "", ErrUnavailable
	}
	return resourcePlaneURIPrefix + resourcePlaneID, nil
}

func Handler(service *OAuthService) http.Handler {
	return HandlerWithCheckout(service, nil)
}

func HandlerWithCheckout(service *OAuthService, checkout *CheckoutService) http.Handler {
	return HandlerWithServices(service, checkout, nil)
}

func HandlerWithServices(service *OAuthService, checkout *CheckoutService, checks *ChecksService) http.Handler {
	return HandlerWithServicesAndCheckoutCallers(service, checkout, checks, nil)
}

// HandlerWithServicesAndCheckoutCallers keeps OAuth/Checks Control
// Plane-only while allowing explicitly enumerated Resource Plane identities
// to use the narrowly scoped Tenant/Run checkout endpoint.
func HandlerWithServicesAndCheckoutCallers(service *OAuthService, checkout *CheckoutService, checks *ChecksService, checkoutCallers []string) http.Handler {
	return HandlerWithActionsRunner(service, checkout, checks, checkoutCallers, nil)
}

// HandlerWithActionsRunner exposes runner registration only to the verified
// Control Plane certificate identity. Resource Plane identities are never
// admitted to this endpoint.
func HandlerWithActionsRunner(service *OAuthService, checkout *CheckoutService, checks *ChecksService, checkoutCallers []string, actionsRunner *ActionsRunnerRegistrationService) http.Handler {
	return HandlerWithActionsRunnerAndCleanup(service, checkout, checks, checkoutCallers, actionsRunner, nil, nil)
}

func HandlerWithActionsRunnerAndCleanup(service *OAuthService, checkout *CheckoutService, checks *ChecksService, checkoutCallers []string,
	actionsRunner *ActionsRunnerRegistrationService, runnerCleanup *ActionsRunnerCleanupService, cleanupCallers []string) http.Handler {
	allowedCheckoutCallers := map[string]struct{}{controlPlaneURI: {}}
	for _, caller := range checkoutCallers {
		if caller != controlPlaneURI && !strings.HasPrefix(caller, resourcePlaneURIPrefix) {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeError(w, http.StatusServiceUnavailable) })
		}
		if caller != controlPlaneURI {
			id := strings.TrimPrefix(caller, resourcePlaneURIPrefix)
			if _, err := ResourcePlaneCallerID(id); err != nil {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeError(w, http.StatusServiceUnavailable) })
			}
		}
		allowedCheckoutCallers[caller] = struct{}{}
	}
	allowedCleanupCallers := make(map[string]struct{}, len(cleanupCallers))
	for _, caller := range cleanupCallers {
		if !validResourcePlaneCleanupCaller(caller) {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeError(w, http.StatusServiceUnavailable) })
		}
		if _, duplicate := allowedCleanupCallers[caller]; duplicate {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeError(w, http.StatusServiceUnavailable) })
		}
		allowedCleanupCallers[caller] = struct{}{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/setup-url", func(w http.ResponseWriter, r *http.Request) {
		caller, err := authenticatedCaller(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized)
			return
		}
		input, ok := decodeRequest(w, r)
		if !ok || input.State == "" || len(input.State) > 128 {
			writeError(w, http.StatusBadRequest)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), callerContextKey{}, caller))
		pin := input.Pin
		setup, _, err := service.URLs(r.Context(), pin, input.State, strings.Repeat("v", 43))
		if err != nil {
			writeError(w, http.StatusBadGateway)
			return
		}
		writeResponse(w, response{SetupURL: setup})
	})
	mux.HandleFunc("POST /v1/oauth/authorize-url", func(w http.ResponseWriter, r *http.Request) {
		caller, err := authenticatedCaller(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized)
			return
		}
		input, ok := decodeRequest(w, r)
		if !ok || input.State == "" || len(input.State) > 128 || len(input.CodeVerifier) < 43 || len(input.CodeVerifier) > 128 {
			writeError(w, http.StatusBadRequest)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), callerContextKey{}, caller))
		pin := input.Pin
		_, auth, err := service.URLs(r.Context(), pin, input.State, input.CodeVerifier)
		if err != nil {
			writeError(w, http.StatusBadGateway)
			return
		}
		writeResponse(w, response{AuthorizationURL: auth})
	})
	mux.HandleFunc("POST /v1/oauth/verify", func(w http.ResponseWriter, r *http.Request) {
		caller, err := authenticatedCaller(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized)
			return
		}
		input, ok := decodeRequest(w, r)
		if !ok {
			writeError(w, http.StatusBadRequest)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), callerContextKey{}, caller))
		pin := input.Pin
		identity, err := service.Verify(r.Context(), pin, input.Code, input.InstallationID, input.CodeVerifier)
		if errors.Is(err, githubapp.ErrAuthorizationRejected) {
			writeError(w, http.StatusUnauthorized)
			return
		}
		if err != nil {
			writeError(w, http.StatusBadGateway)
			return
		}
		writeResponse(w, response{Identity: &identity})
	})
	if checkout != nil {
		mux.HandleFunc("POST /v1/checkout/token", func(w http.ResponseWriter, r *http.Request) {
			caller, err := authenticatedCheckoutCaller(r, allowedCheckoutCallers)
			if err != nil {
				writeError(w, http.StatusUnauthorized)
				return
			}
			input, ok := decodeCheckoutRequest(w, r)
			if !ok {
				writeError(w, http.StatusBadRequest)
				return
			}
			grant, err := checkout.Issue(r.Context(), caller, input.TenantID, input.RunID)
			if err != nil {
				writeError(w, http.StatusForbidden)
				return
			}
			writeResponse(w, response{Token: grant.Token(), ExpiresAt: grant.ExpiresAt()})
		})
	}
	if checks != nil {
		mux.HandleFunc("POST /v1/checks/token", func(w http.ResponseWriter, r *http.Request) {
			caller, err := authenticatedCaller(r)
			if err != nil {
				writeError(w, http.StatusUnauthorized)
				return
			}
			input, ok := decodeChecksRequest(r)
			if !ok {
				writeError(w, http.StatusBadRequest)
				return
			}
			grant, err := checks.Issue(r.Context(), caller, input.TenantID, input.RunID)
			if err != nil {
				writeError(w, http.StatusForbidden)
				return
			}
			repository := grant.Repository()
			writeResponse(w, response{Token: grant.Token(), ExpiresAt: grant.ExpiresAt(), Repository: &repositoryResponse{Owner: repository.Owner, Name: repository.Name}})
		})
	}
	if actionsRunner != nil {
		mux.HandleFunc("POST /v1/actions-runner/registration-token", func(w http.ResponseWriter, r *http.Request) {
			caller, err := authenticatedCaller(r)
			if err != nil {
				writeError(w, http.StatusUnauthorized)
				return
			}
			input, ok := decodeActionsRunnerTokenRequest(r)
			if !ok {
				writeError(w, http.StatusBadRequest)
				return
			}
			grant, err := actionsRunner.Issue(r.Context(), caller, ActionsRunnerRegistrationRequest{
				IntegrationID: input.IntegrationID, WorkflowJobID: input.WorkflowJobID, RunAttempt: input.RunAttempt, ClaimID: input.ClaimID,
			})
			if err != nil {
				writeError(w, http.StatusForbidden)
				return
			}
			repository := grant.Repository
			writeResponse(w, response{Token: grant.Token.Value(), ExpiresAt: grant.Token.ExpiresAt(), Repository: &repositoryResponse{Owner: repository.Owner, Name: repository.Name}})
		})
	}
	if runnerCleanup != nil && len(allowedCleanupCallers) > 0 {
		mux.HandleFunc("POST /v1/actions-runner/deregister", func(w http.ResponseWriter, r *http.Request) {
			caller, err := authenticatedCleanupCaller(r, allowedCleanupCallers)
			if err != nil {
				writeError(w, http.StatusUnauthorized)
				return
			}
			input, ok := decodeActionsRunnerCleanupRequest(r)
			if !ok {
				writeError(w, http.StatusBadRequest)
				return
			}
			if err := runnerCleanup.Deregister(r.Context(), caller, input); err != nil {
				writeError(w, http.StatusForbidden)
				return
			}
			writeResponse(w, struct{}{})
		})
	}
	return mux
}

func authenticatedCaller(r *http.Request) (string, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.VerifiedChains) == 0 {
		return "", ErrUnavailable
	}
	for _, uri := range r.TLS.PeerCertificates[0].URIs {
		if uri.String() == controlPlaneURI {
			return controlPlaneURI, nil
		}
	}
	return "", ErrUnavailable
}

func authenticatedCheckoutCaller(r *http.Request, allowed map[string]struct{}) (string, error) {
	if r == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.VerifiedChains) == 0 || len(allowed) == 0 {
		return "", ErrUnavailable
	}
	for _, uri := range r.TLS.PeerCertificates[0].URIs {
		caller := uri.String()
		if _, ok := allowed[caller]; ok {
			return caller, nil
		}
	}
	return "", ErrUnavailable
}

func authenticatedCleanupCaller(r *http.Request, allowed map[string]struct{}) (string, error) {
	if r == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.VerifiedChains) == 0 || len(allowed) == 0 {
		return "", ErrUnavailable
	}
	for _, uri := range r.TLS.PeerCertificates[0].URIs {
		caller := uri.String()
		if _, ok := allowed[caller]; ok && validResourcePlaneCleanupCaller(caller) {
			return caller, nil
		}
	}
	return "", ErrUnavailable
}

func decodeRequest(w http.ResponseWriter, r *http.Request) (request, bool) {
	var input request
	if r.Body == nil || r.ContentLength > brokerMaxBody || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return input, false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, brokerMaxBody+1))
	if err != nil || len(data) > brokerMaxBody || json.Unmarshal(data, &input) != nil || input.Pin.TenantID == "" || input.Pin.IntegrationID == "" || input.Pin.AppRegistrationID == "" || input.Pin.GitHubAppID <= 0 || input.Pin.ConfigRevision <= 0 || input.Pin.CredentialGeneration <= 0 {
		return request{}, false
	}
	return input, true
}

func decodeCheckoutRequest(w http.ResponseWriter, r *http.Request) (request, bool) {
	var input request
	if r.Body == nil || r.ContentLength > 1024 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return input, false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	if err != nil || len(data) > 1024 {
		return input, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var decoded checkoutRequest
	if decoder.Decode(&decoded) != nil || decoded.TenantID == "" || !validCheckoutRunID(decoded.RunID) {
		return input, false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return input, false
	}
	return request{TenantID: decoded.TenantID, RunID: decoded.RunID}, true
}

func decodeChecksRequest(r *http.Request) (checkoutRequest, bool) {
	var decoded checkoutRequest
	if r.Body == nil || r.ContentLength > 1024 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return decoded, false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	if err != nil || len(data) > 1024 {
		return decoded, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&decoded) != nil || decoded.TenantID == "" || !validCheckoutRunID(decoded.RunID) {
		return checkoutRequest{}, false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return checkoutRequest{}, false
	}
	return decoded, true
}

func decodeActionsRunnerTokenRequest(r *http.Request) (actionsRunnerTokenRequest, bool) {
	var decoded actionsRunnerTokenRequest
	if r == nil || r.Body == nil || r.ContentLength > 1024 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return decoded, false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	if err != nil || len(data) > 1024 {
		return decoded, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&decoded) != nil || !validActionsRunnerRegistrationRequest(ActionsRunnerRegistrationRequest{
		IntegrationID: decoded.IntegrationID, WorkflowJobID: decoded.WorkflowJobID, RunAttempt: decoded.RunAttempt, ClaimID: decoded.ClaimID,
	}) {
		return actionsRunnerTokenRequest{}, false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return actionsRunnerTokenRequest{}, false
	}
	return decoded, true
}

func decodeActionsRunnerCleanupRequest(r *http.Request) (ActionsRunnerCleanupRequest, bool) {
	var decoded actionsRunnerCleanupRequest
	if r == nil || r.Body == nil || r.ContentLength > 1024 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return ActionsRunnerCleanupRequest{}, false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	if err != nil || len(data) > 1024 {
		return ActionsRunnerCleanupRequest{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&decoded) != nil || !validCleanupRequest(ActionsRunnerCleanupRequest(decoded)) {
		return ActionsRunnerCleanupRequest{}, false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return ActionsRunnerCleanupRequest{}, false
	}
	return ActionsRunnerCleanupRequest(decoded), true
}

type HTTPActionsRunnerRegistrationClient struct {
	base   string
	client *http.Client
}

type HTTPActionsRunnerCleanupClient struct {
	base            string
	resourcePlaneID string
	client          *http.Client
}

func NewHTTPActionsRunnerCleanupClient(baseURL, resourcePlaneID string, client *http.Client) (*HTTPActionsRunnerCleanupClient, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if client == nil || err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" ||
		!resourcePlaneIDPattern.MatchString(resourcePlaneID) {
		return nil, ErrUnavailable
	}
	return &HTTPActionsRunnerCleanupClient{base: strings.TrimRight(base.String(), "/"), resourcePlaneID: resourcePlaneID, client: client}, nil
}

// Deregister sends only the durable cleanup key. Tenant/Installation/
// Repository/Runner coordinates are resolved and lease-checked by the broker.
func (c *HTTPActionsRunnerCleanupClient) Deregister(ctx context.Context, job githubwebhook.ActionsRunnerCleanupJob) error {
	if c == nil || c.client == nil || ctx == nil || job.ResourcePlaneID == nil || *job.ResourcePlaneID != c.resourcePlaneID ||
		job.RunnerID == nil || job.State != "claimed" || !validCleanupRequest(ActionsRunnerCleanupRequest{
		IntegrationID: job.IntegrationID, WorkflowJobID: job.WorkflowJobID, RunAttempt: job.RunAttempt, ClaimID: job.ClaimID}) {
		return ErrActionsRunnerCleanupDenied
	}
	input := ActionsRunnerCleanupRequest{IntegrationID: job.IntegrationID, WorkflowJobID: job.WorkflowJobID, RunAttempt: job.RunAttempt, ClaimID: job.ClaimID}
	body, err := json.Marshal(input)
	if err != nil {
		return ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/actions-runner/deregister", bytes.NewReader(body))
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > 1024 || resp.Header.Get("Cache-Control") != "no-store" {
		return ErrActionsRunnerCleanupDenied
	}
	var decoded struct{}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return ErrUnavailable
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return ErrUnavailable
	}
	return nil
}

func NewHTTPActionsRunnerRegistrationClient(baseURL string, client *http.Client) (*HTTPActionsRunnerRegistrationClient, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if client == nil || err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, ErrUnavailable
	}
	return &HTTPActionsRunnerRegistrationClient{base: strings.TrimRight(base.String(), "/"), client: client}, nil
}

// Issue requests one claim-bound registration token through the Control
// Plane-only mTLS broker endpoint.
func (c *HTTPActionsRunnerRegistrationClient) Issue(ctx context.Context, request ActionsRunnerRegistrationRequest) (ActionsRunnerRegistrationGrant, error) {
	if c == nil || c.client == nil || ctx == nil || !validActionsRunnerRegistrationRequest(request) {
		return ActionsRunnerRegistrationGrant{}, ErrActionsRunnerDenied
	}
	body, err := json.Marshal(actionsRunnerTokenRequest{IntegrationID: request.IntegrationID, WorkflowJobID: request.WorkflowJobID, RunAttempt: request.RunAttempt, ClaimID: request.ClaimID})
	if err != nil {
		return ActionsRunnerRegistrationGrant{}, ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/actions-runner/registration-token", bytes.NewReader(body))
	if err != nil {
		return ActionsRunnerRegistrationGrant{}, ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return ActionsRunnerRegistrationGrant{}, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > 16<<10 || resp.Header.Get("Cache-Control") != "no-store" {
		return ActionsRunnerRegistrationGrant{}, ErrActionsRunnerDenied
	}
	var decoded struct {
		Token      string             `json:"token"`
		ExpiresAt  time.Time          `json:"expires_at"`
		Repository repositoryResponse `json:"repository"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 16<<10))
	if err := decoder.Decode(&decoded); err != nil || decoded.Repository.Owner == "" || decoded.Repository.Name == "" {
		return ActionsRunnerRegistrationGrant{}, ErrUnavailable
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return ActionsRunnerRegistrationGrant{}, ErrUnavailable
	}
	token, err := githubapp.NewRunnerRegistrationToken(decoded.Token, decoded.ExpiresAt)
	if err != nil || !token.ExpiresAt().After(time.Now().Add(2*time.Minute)) || token.ExpiresAt().After(time.Now().Add(65*time.Minute)) {
		return ActionsRunnerRegistrationGrant{}, ErrUnavailable
	}
	return ActionsRunnerRegistrationGrant{Token: token, Repository: githubapp.RepositoryAccess{Owner: decoded.Repository.Owner, Name: decoded.Repository.Name}}, nil
}

func writeError(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"broker_unavailable"}`))
}
func writeResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// HTTPResolver uses mutual TLS; its TLS config must contain a control-plane
// client cert issued by the broker's dedicated trust root.
type HTTPResolver struct {
	BaseURL string
	Client  *http.Client
}

func NewHTTPResolver(baseURL string, client *http.Client) (*HTTPResolver, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if client == nil || err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, ErrUnavailable
	}
	return &HTTPResolver{BaseURL: strings.TrimRight(base.String(), "/"), Client: client}, nil
}

func (c *HTTPResolver) ResolveSetupVerifier(_ context.Context, pin githubregistry.SetupAppPin) (githubapp.SetupVerifier, error) {
	if c == nil {
		return nil, ErrUnavailable
	}
	validated, err := NewHTTPResolver(c.BaseURL, c.Client)
	if err != nil {
		return nil, ErrUnavailable
	}
	return &httpVerifier{base: validated.BaseURL, client: c.Client, pin: pin}, nil
}

type httpVerifier struct {
	base   string
	client *http.Client
	pin    githubregistry.SetupAppPin
}

// HTTPCheckoutProvider implements repositorycheckout.RunCredentialProvider.
// The mTLS request carries only Tenant/Run identifiers; the broker resolves
// immutable credential identity from its database.
type HTTPCheckoutProvider struct {
	base   string
	client *http.Client
}

type HTTPChecksCredentialProvider struct {
	base   string
	client *http.Client
}

type brokerChecksToken struct {
	value     string
	expiresAt time.Time
}

func (brokerChecksToken) String() string         { return "GitHub Checks token [REDACTED]" }
func (t brokerChecksToken) GoString() string     { return t.String() }
func (t brokerChecksToken) Value() string        { return t.value }
func (t brokerChecksToken) ExpiresAt() time.Time { return t.expiresAt }
func (brokerChecksToken) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED GitHub Checks token]"`), nil
}

func NewHTTPChecksCredentialProvider(baseURL string, client *http.Client) (*HTTPChecksCredentialProvider, error) {
	validated, err := NewHTTPResolver(baseURL, client)
	if err != nil {
		return nil, err
	}
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if bounded.Timeout <= 0 || bounded.Timeout > 50*time.Second {
		bounded.Timeout = 50 * time.Second
	}
	return &HTTPChecksCredentialProvider{base: validated.BaseURL, client: &bounded}, nil
}

func (c *HTTPChecksCredentialProvider) ChecksRunToken(ctx context.Context, run domain.WorkloadRun) (githubapp.CheckToken, githubapp.RepositoryAccess, error) {
	if c == nil || c.client == nil || ctx == nil || run.TenantID == "" || !validCheckoutRunID(run.ID) ||
		run.GitHubIntegrationID == "" || run.GitHubConfigRevision < 1 || run.GitHubCredentialGeneration < 1 ||
		run.GitHubInstallation < 1 || run.GitHubRepository < 1 {
		return nil, githubapp.RepositoryAccess{}, ErrUnavailable
	}
	body, err := json.Marshal(checkoutRequest{TenantID: run.TenantID, RunID: run.ID})
	if err != nil {
		return nil, githubapp.RepositoryAccess{}, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/checks/token", strings.NewReader(string(body)))
	if err != nil {
		return nil, githubapp.RepositoryAccess{}, ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		return nil, githubapp.RepositoryAccess{}, ErrUnavailable
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, brokerMaxBody+1))
	if err != nil || len(data) > brokerMaxBody || res.StatusCode < 200 || res.StatusCode >= 300 || res.Header.Get("Cache-Control") != "no-store" {
		return nil, githubapp.RepositoryAccess{}, ErrUnavailable
	}
	var out response
	if json.Unmarshal(data, &out) != nil || out.Repository == nil || !validBrokerToken(out.Token, out.ExpiresAt, time.Now()) ||
		out.ExpiresAt.After(time.Now().Add(65*time.Minute)) || !validBrokerLogin(out.Repository.Owner) || !validCredentialRepository(out.Repository.Name) {
		return nil, githubapp.RepositoryAccess{}, ErrUnavailable
	}
	token := brokerChecksToken{value: out.Token, expiresAt: out.ExpiresAt}
	return token, githubapp.RepositoryAccess{Owner: out.Repository.Owner, Name: out.Repository.Name}, nil
}

func validBrokerLogin(value string) bool {
	if value == "" || len(value) > 39 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-') {
			return false
		}
	}
	return true
}

func validCredentialRepository(value string) bool {
	if value == "" || len(value) > 100 || value == "." || value == ".." {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.') {
			return false
		}
	}
	return true
}

func NewHTTPCheckoutProvider(baseURL string, client *http.Client) (*HTTPCheckoutProvider, error) {
	validated, err := NewHTTPResolver(baseURL, client)
	if err != nil {
		return nil, err
	}
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if bounded.Timeout <= 0 || bounded.Timeout > 50*time.Second {
		bounded.Timeout = 50 * time.Second
	}
	return &HTTPCheckoutProvider{base: validated.BaseURL, client: &bounded}, nil
}

func (c *HTTPCheckoutProvider) CheckoutRunToken(ctx context.Context, run domain.WorkloadRun) (string, error) {
	if c == nil || c.client == nil || ctx == nil || run.TenantID == "" || run.ID == "" || run.GitHubIntegrationID == "" ||
		run.GitHubConfigRevision < 1 || run.GitHubCredentialGeneration < 1 || run.GitHubInstallation < 1 || run.GitHubRepository < 1 {
		return "", ErrUnavailable
	}
	body, err := json.Marshal(checkoutRequest{TenantID: run.TenantID, RunID: run.ID})
	if err != nil {
		return "", ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/checkout/token", strings.NewReader(string(body)))
	if err != nil {
		return "", ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		return "", ErrUnavailable
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, brokerMaxBody+1))
	if err != nil || len(data) > brokerMaxBody || res.StatusCode < 200 || res.StatusCode >= 300 || res.Header.Get("Cache-Control") != "no-store" {
		return "", ErrUnavailable
	}
	var out response
	if json.Unmarshal(data, &out) != nil || !validBrokerToken(out.Token, out.ExpiresAt, time.Now()) {
		return "", ErrUnavailable
	}
	return out.Token, nil
}

func (v *httpVerifier) call(ctx context.Context, path string, in request) (response, error) {
	in.Pin = v.pin
	data, err := json.Marshal(in)
	if err != nil {
		return response{}, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.base+path, strings.NewReader(string(data)))
	if err != nil {
		return response{}, ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := v.client.Do(req)
	if err != nil {
		return response{}, ErrUnavailable
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, brokerMaxBody))
	if err != nil || res.StatusCode < 200 || res.StatusCode >= 300 {
		return response{}, ErrUnavailable
	}
	var out response
	if json.Unmarshal(body, &out) != nil {
		return response{}, ErrUnavailable
	}
	return out, nil
}
func (v *httpVerifier) SetupURL(state string) string {
	out, err := v.call(context.Background(), "/v1/oauth/setup-url", request{State: state})
	if err != nil {
		return ""
	}
	return out.SetupURL
}
func (v *httpVerifier) AuthorizationURL(state, verifier string) string {
	out, err := v.call(context.Background(), "/v1/oauth/authorize-url", request{State: state, CodeVerifier: verifier})
	if err != nil {
		return ""
	}
	return out.AuthorizationURL
}
func (v *httpVerifier) VerifyInstallation(ctx context.Context, code string, id int64, verifier string) (githubapp.InstallationIdentity, error) {
	out, err := v.call(ctx, "/v1/oauth/verify", request{Code: code, InstallationID: id, CodeVerifier: verifier})
	if err != nil || out.Identity == nil {
		return githubapp.InstallationIdentity{}, ErrUnavailable
	}
	return *out.Identity, nil
}
