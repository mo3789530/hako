package githubbroker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
)

const tenantAppProofRoute = "/v1/tenant-apps/verify"

var ErrTenantAppProofDenied = errors.New("Tenant GitHub App proof denied")

type TenantAppProofRequest struct {
	TenantID             domain.TenantID `json:"tenant_id"`
	IntegrationID        string          `json:"integration_id"`
	GitHubAppID          int64           `json:"github_app_id"`
	ConfigRevision       int64           `json:"config_revision"`
	CredentialGeneration int64           `json:"credential_generation"`
}

type tenantAppProofResponse struct {
	Verified bool `json:"verified"`
}

type TenantAppProofService struct {
	resolve func(context.Context, TenantAppProofRequest) (githubapp.TenantPrivateKeyAccess, error)
	verify  func(context.Context, githubapp.TenantPrivateKeyAccess) error
}

func NewTenantAppProofService(resolve func(context.Context, TenantAppProofRequest) (githubapp.TenantPrivateKeyAccess, error),
	verify func(context.Context, githubapp.TenantPrivateKeyAccess) error) (*TenantAppProofService, error) {
	if resolve == nil || verify == nil {
		return nil, ErrUnavailable
	}
	return &TenantAppProofService{resolve: resolve, verify: verify}, nil
}

func NewPostgresTenantAppProofResolver(pool transaction.Beginner, policy transaction.Policy) func(context.Context, TenantAppProofRequest) (githubapp.TenantPrivateKeyAccess, error) {
	return func(ctx context.Context, request TenantAppProofRequest) (githubapp.TenantPrivateKeyAccess, error) {
		if pool == nil || ctx == nil || !validTenantAppProofRequest(request) {
			return githubapp.TenantPrivateKeyAccess{}, ErrTenantAppProofDenied
		}
		credential, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (githubregistry.AppProofCredential, error) {
			return githubregistry.ResolveAppProofCredential(ctx, tx, request.TenantID, request.IntegrationID,
				request.ConfigRevision, request.CredentialGeneration)
		})
		if err != nil || credential.GitHubAppID != request.GitHubAppID {
			return githubapp.TenantPrivateKeyAccess{}, ErrTenantAppProofDenied
		}
		return githubapp.TenantPrivateKeyAccess{TenantID: string(credential.TenantID), IntegrationID: credential.IntegrationID,
			GitHubAppID: credential.GitHubAppID, ConfigRevision: credential.ConfigRevision,
			CredentialGeneration: credential.CredentialGeneration, ReaderRoleARN: credential.PrivateKeyReaderRoleARN,
			SecretARN: credential.PrivateKeySecretARN, SecretVersionID: credential.PrivateKeyVersionID}, nil
	}
}

func (s *TenantAppProofService) Verify(ctx context.Context, caller string, request TenantAppProofRequest) error {
	if s == nil || s.resolve == nil || s.verify == nil || ctx == nil || ctx.Err() != nil || caller != controlPlaneURI || !validTenantAppProofRequest(request) {
		return ErrTenantAppProofDenied
	}
	access, err := s.resolve(ctx, request)
	if err != nil || !proofAccessMatchesRequest(access, request) {
		return ErrTenantAppProofDenied
	}
	if err := s.verify(ctx, access); err != nil {
		return ErrTenantAppProofDenied
	}
	return nil
}

func validTenantAppProofRequest(request TenantAppProofRequest) bool {
	return request.TenantID != "" && validCheckoutRunID(domain.WorkloadRunID(request.IntegrationID)) && request.GitHubAppID > 0 &&
		request.ConfigRevision > 0 && request.CredentialGeneration > 0
}

func proofAccessMatchesRequest(access githubapp.TenantPrivateKeyAccess, request TenantAppProofRequest) bool {
	return access.TenantID == string(request.TenantID) && access.IntegrationID == request.IntegrationID &&
		access.GitHubAppID == request.GitHubAppID && access.ConfigRevision == request.ConfigRevision &&
		access.CredentialGeneration == request.CredentialGeneration && access.ReaderRoleARN != "" &&
		access.SecretARN != "" && access.SecretVersionID != ""
}

func WithTenantAppProof(next http.Handler, service *TenantAppProofService) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tenantAppProofRoute {
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
		input, ok := decodeTenantAppProofRequest(r)
		if !ok {
			writeError(w, http.StatusBadRequest)
			return
		}
		if service == nil {
			writeError(w, http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		if err := service.Verify(ctx, caller, input); err != nil {
			writeError(w, http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeResponse(w, tenantAppProofResponse{Verified: true})
	})
}

func decodeTenantAppProofRequest(r *http.Request) (TenantAppProofRequest, bool) {
	if r == nil || r.Body == nil || r.ContentLength > 2048 {
		return TenantAppProofRequest{}, false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return TenantAppProofRequest{}, false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 2049))
	if err != nil || len(data) == 0 || len(data) > 2048 {
		return TenantAppProofRequest{}, false
	}
	defer clear(data)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request TenantAppProofRequest
	if decoder.Decode(&request) != nil || !validTenantAppProofRequest(request) {
		return TenantAppProofRequest{}, false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return TenantAppProofRequest{}, false
	}
	return request, true
}

type HTTPTenantAppProofVerifier struct {
	base string
	http *http.Client
}

func NewHTTPTenantAppProofVerifier(baseURL string, client *http.Client) (*HTTPTenantAppProofVerifier, error) {
	resolver, err := NewHTTPResolver(baseURL, client)
	if err != nil {
		return nil, err
	}
	return &HTTPTenantAppProofVerifier{base: resolver.BaseURL, http: resolver.Client}, nil
}

// VerifyApp sends only the public App identity and immutable metadata pins.
// Secret ARN, reader Role ARN, and Version ID are deliberately resolved again
// by the broker from its own database connection.
func (c *HTTPTenantAppProofVerifier) VerifyApp(ctx context.Context, credential githubregistry.AppProofCredential) error {
	if c == nil || c.http == nil || ctx == nil || credential.TenantID == "" || !validCheckoutRunID(domain.WorkloadRunID(credential.IntegrationID)) ||
		credential.GitHubAppID < 1 || credential.ConfigRevision < 1 || credential.CredentialGeneration < 1 {
		return ErrUnavailable
	}
	requestBody, err := json.Marshal(TenantAppProofRequest{TenantID: credential.TenantID, IntegrationID: credential.IntegrationID,
		GitHubAppID: credential.GitHubAppID, ConfigRevision: credential.ConfigRevision, CredentialGeneration: credential.CredentialGeneration})
	if err != nil || len(requestBody) > 2048 {
		return ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+tenantAppProofRoute, bytes.NewReader(requestBody))
	defer clear(requestBody)
	if err != nil {
		return ErrUnavailable
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return ErrUnavailable
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2049))
	if err != nil || len(data) > 2048 {
		return ErrUnavailable
	}
	defer clear(data)
	if response.StatusCode == http.StatusForbidden {
		return ErrTenantAppProofDenied
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		return ErrUnavailable
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return ErrUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var result tenantAppProofResponse
	if decoder.Decode(&result) != nil || !result.Verified {
		return ErrUnavailable
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return ErrUnavailable
	}
	return nil
}

func NewTenantAppProofMTLSClient(certificate tls.Certificate, brokerRoots *x509.CertPool) (*http.Client, error) {
	return newMTLSClient(certificate, brokerRoots, 45*time.Second, controlPlaneURI)
}
