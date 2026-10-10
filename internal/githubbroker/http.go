package githubbroker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubregistry"
)

const controlPlaneURI = "spiffe://hako/control-plane"
const brokerMaxBody = 16 << 10

type request struct {
	Pin            githubregistry.SetupAppPin `json:"pin"`
	State          string                     `json:"state,omitempty"`
	CodeVerifier   string                     `json:"code_verifier,omitempty"`
	Code           string                     `json:"code,omitempty"`
	InstallationID int64                      `json:"installation_id,omitempty"`
}
type response struct {
	SetupURL         string                          `json:"setup_url,omitempty"`
	AuthorizationURL string                          `json:"authorization_url,omitempty"`
	Identity         *githubapp.InstallationIdentity `json:"identity,omitempty"`
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
	if len(certificate.Certificate) == 0 || brokerRoots == nil || certificate.Leaf == nil {
		return nil, ErrUnavailable
	}
	validIdentity := false
	for _, uri := range certificate.Leaf.URIs {
		if uri.String() == controlPlaneURI {
			validIdentity = true
		}
	}
	if !validIdentity {
		return nil, ErrUnavailable
	}
	return &http.Client{Timeout: 6 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: brokerRoots, Certificates: []tls.Certificate{certificate}},
		Proxy:           nil, DisableKeepAlives: false, MaxIdleConnsPerHost: 8,
	}}, nil
}

func Handler(service *OAuthService) http.Handler {
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
func writeError(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"broker_unavailable"}`))
}
func writeResponse(w http.ResponseWriter, v response) {
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
