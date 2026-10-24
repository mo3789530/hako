package githubbroker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/modelcredentials"
)

type agentModelIssuerFunc func(context.Context, string, modelcredentials.AgentRunIdentity) ([]byte, error)

func (f agentModelIssuerFunc) Issue(ctx context.Context, caller string, identity modelcredentials.AgentRunIdentity) ([]byte, error) {
	return f(ctx, caller, identity)
}

func TestAgentModelCredentialRouteRequiresVerifiedPlaneAndNoStore(t *testing.T) {
	identity := modelcredentials.AgentRunIdentity{TenantID: domain.TenantID("tenant_agent"), WorkloadRunID: domain.WorkloadRunID("run_agent"),
		AttemptID: "attempt_agent", Generation: 1, ResourcePlaneID: domain.ResourcePlaneID("rp-tokyo-01")}
	calls := 0
	handler := WithAgentModelCredential(http.NotFoundHandler(), agentModelIssuerFunc(func(_ context.Context, caller string, got modelcredentials.AgentRunIdentity) ([]byte, error) {
		calls++
		if caller != resourcePlaneURIPrefix+"rp-tokyo-01" || got != identity {
			return nil, ErrAgentTaskUnavailable
		}
		return []byte("sk-ant-synthetic-key"), nil
	}))
	call := func(body, caller string, verified bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, agentModelCredentialRoute, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if caller != "" {
			u, _ := url.Parse(caller)
			cert := &x509.Certificate{URIs: []*url.URL{u}}
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
			if verified {
				req.TLS.VerifiedChains = [][]*x509.Certificate{{cert}}
			}
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	body := `{"tenant_id":"tenant_agent","workload_run_id":"run_agent","attempt_id":"attempt_agent","generation":1,"resource_plane_id":"rp-tokyo-01"}`
	if got := call(body, resourcePlaneURIPrefix+"rp-tokyo-01", false); got.Code != http.StatusUnauthorized || calls != 0 {
		t.Fatalf("unverified mTLS caller status=%d calls=%d", got.Code, calls)
	}
	if got := call(strings.TrimSuffix(body, "}")+`,"credential":"injected"}`, resourcePlaneURIPrefix+"rp-tokyo-01", true); got.Code != http.StatusBadRequest || calls != 0 {
		t.Fatalf("unknown credential request field status=%d calls=%d", got.Code, calls)
	}
	response := call(body, resourcePlaneURIPrefix+"rp-tokyo-01", true)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), "c2stYW50LXN5bnRoZXRpYy1rZXk=") || calls != 1 {
		t.Fatalf("valid credential response status=%d cache=%q body=%q calls=%d", response.Code, response.Header().Get("Cache-Control"), response.Body.String(), calls)
	}
	get := httptest.NewRequest(http.MethodGet, agentModelCredentialRoute, nil)
	get.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{URIs: []*url.URL{{Scheme: "spiffe", Host: "hako", Path: "/resource-plane/rp-tokyo-01"}}}}, VerifiedChains: [][]*x509.Certificate{{{}}}}
	methodResponse := httptest.NewRecorder()
	handler.ServeHTTP(methodResponse, get)
	if methodResponse.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d", methodResponse.Code)
	}
}

type agentModelRoundTripFunc func(*http.Request) (*http.Response, error)

func (f agentModelRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestHTTPAgentModelCredentialClientBoundsAndWipesResponse(t *testing.T) {
	identity := modelcredentials.AgentRunIdentity{TenantID: "tenant_agent", WorkloadRunID: "run_agent", AttemptID: "attempt_agent", Generation: 1, ResourcePlaneID: "rp-tokyo-01"}
	client, err := NewHTTPAgentModelCredentialClient("https://broker.example/base", &http.Client{Transport: agentModelRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.String() != "https://broker.example/base"+agentModelCredentialRoute || req.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("request = %s %s cache=%q", req.Method, req.URL.String(), req.Header.Get("Cache-Control"))
		}
		var got modelcredentials.AgentRunIdentity
		if err := json.NewDecoder(req.Body).Decode(&got); err != nil || got != identity {
			t.Errorf("request identity=%+v error=%v", got, err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Cache-Control": []string{"no-store"}},
			Body: io.NopCloser(strings.NewReader(`{"credential":"c2stYW50LXN5bnRoZXRpYy1rZXk="}`)), ContentLength: 47}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := client.Issue(context.Background(), identity)
	if err != nil || string(credential) != "sk-ant-synthetic-key" {
		t.Fatalf("Issue() = %q, error=%v", credential, err)
	}
	for i := range credential {
		credential[i] = 0
	}
	unsafe, err := NewHTTPAgentModelCredentialClient("http://broker.example", &http.Client{})
	if err == nil || unsafe != nil {
		t.Fatalf("accepted non-TLS broker URL: client=%v error=%v", unsafe, err)
	}
}
