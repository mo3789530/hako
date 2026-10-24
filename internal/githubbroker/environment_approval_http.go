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
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/deploypolicy"
	"github.com/mo3789530/hako/internal/domain"
)

const environmentApprovalMaxBody = 4 << 10

type environmentApprovalRequest struct {
	TenantID     domain.TenantID `json:"tenant_id"`
	ServiceID    string          `json:"service_id"`
	Installation int64           `json:"installation_id"`
	RepositoryID int64           `json:"repository_id"`
	Environment  string          `json:"environment"`
}

type environmentApprovalResponse struct {
	Verified bool `json:"verified"`
}

// WithEnvironmentApproval adds a Control Plane mTLS-only Environment
// protection check. The request carries resource coordinates only; the
// broker re-resolves the active Tenant App, policy and Repository from DSQL.
func WithEnvironmentApproval(next http.Handler, service *EnvironmentApprovalService) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+environmentApprovalRoute, func(w http.ResponseWriter, r *http.Request) {
		caller, err := authenticatedCaller(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized)
			return
		}
		input, ok := decodeEnvironmentApprovalRequest(r)
		if !ok {
			writeError(w, http.StatusBadRequest)
			return
		}
		if service == nil {
			writeError(w, http.StatusServiceUnavailable)
			return
		}
		err = service.Verify(r.Context(), caller, input.TenantID, input.ServiceID, input.Installation, input.RepositoryID, input.Environment)
		switch {
		case err == nil:
			writeResponse(w, environmentApprovalResponse{Verified: true})
		case errors.Is(err, ErrEnvironmentApprovalDenied):
			writeError(w, http.StatusForbidden)
		default:
			writeError(w, http.StatusBadGateway)
		}
	})
	if next != nil {
		mux.Handle("/", next)
	}
	return mux
}

func decodeEnvironmentApprovalRequest(r *http.Request) (environmentApprovalRequest, bool) {
	if r == nil || r.Body == nil || r.ContentLength > environmentApprovalMaxBody {
		return environmentApprovalRequest{}, false
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if mediaType != "application/json" {
		return environmentApprovalRequest{}, false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, environmentApprovalMaxBody+1))
	if err != nil || len(data) == 0 || len(data) > environmentApprovalMaxBody {
		return environmentApprovalRequest{}, false
	}
	defer clear(data)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var input environmentApprovalRequest
	if decoder.Decode(&input) != nil {
		return environmentApprovalRequest{}, false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || input.TenantID == "" || !environmentApprovalServicePattern.MatchString(input.ServiceID) ||
		input.Installation < 1 || input.RepositoryID < 1 || !environmentApprovalNameValid(input.Environment) {
		return environmentApprovalRequest{}, false
	}
	return input, true
}

type HTTPEnvironmentApprovalVerifier struct {
	base string
	http *http.Client
}

func NewHTTPEnvironmentApprovalVerifier(baseURL string, client *http.Client) (*HTTPEnvironmentApprovalVerifier, error) {
	validated, err := NewHTTPResolver(baseURL, client)
	if err != nil {
		return nil, err
	}
	return &HTTPEnvironmentApprovalVerifier{base: validated.BaseURL, http: validated.Client}, nil
}

func (c *HTTPEnvironmentApprovalVerifier) Verify(ctx context.Context, tenant domain.TenantID, serviceID string, installationID, repositoryID int64, environment string) error {
	if c == nil || c.http == nil || ctx == nil || tenant == "" || !environmentApprovalServicePattern.MatchString(serviceID) || installationID < 1 || repositoryID < 1 || !environmentApprovalNameValid(environment) {
		return ErrEnvironmentApprovalDenied
	}
	body, err := json.Marshal(environmentApprovalRequest{TenantID: tenant, ServiceID: serviceID, Installation: installationID, RepositoryID: repositoryID, Environment: environment})
	if err != nil || len(body) > environmentApprovalMaxBody {
		return deploypolicy.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+environmentApprovalRoute, bytes.NewReader(body))
	if err != nil {
		return deploypolicy.ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return deploypolicy.ErrUnavailable
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, environmentApprovalMaxBody+1))
	if err != nil || len(data) > environmentApprovalMaxBody {
		return deploypolicy.ErrUnavailable
	}
	defer clear(data)
	if response.StatusCode == http.StatusForbidden {
		return deploypolicy.ErrInvalidToken
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		return deploypolicy.ErrUnavailable
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		return deploypolicy.ErrUnavailable
	}
	var result environmentApprovalResponse
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || !result.Verified {
		return deploypolicy.ErrUnavailable
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return deploypolicy.ErrUnavailable
	}
	return nil
}

// NewEnvironmentApprovalMTLSClient creates the Control Plane-only transport.
func NewEnvironmentApprovalMTLSClient(certificate tls.Certificate, brokerRoots *x509.CertPool) (*http.Client, error) {
	return newMTLSClient(certificate, brokerRoots, 30*time.Second, controlPlaneURI)
}
