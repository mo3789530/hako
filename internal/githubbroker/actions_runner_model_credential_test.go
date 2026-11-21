package githubbroker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type modelCredentialIssuerFunc func(context.Context, string, ActionsRunnerModelCredentialRequest) ([]byte, bool, error)

func (f modelCredentialIssuerFunc) Issue(ctx context.Context, caller string, request ActionsRunnerModelCredentialRequest) ([]byte, bool, error) {
	return f(ctx, caller, request)
}

func TestActionsRunnerModelCredentialEndpointRequiresPlaneAndRedactsNoConfig(t *testing.T) {
	const caller = "spiffe://hako/resource-plane/rp-tokyo-01"
	want := ActionsRunnerModelCredentialRequest{IntegrationID: "integration_12345678", WorkflowJobID: 42, RunAttempt: 2,
		ClaimID: "claim_12345678", LambdaMicroVMID: "runner-vm-1"}
	issued := 0
	issuer := modelCredentialIssuerFunc(func(_ context.Context, gotCaller string, request ActionsRunnerModelCredentialRequest) ([]byte, bool, error) {
		issued++
		if gotCaller != caller || request != want {
			t.Fatalf("issuer identity = (%q, %+v), want (%q, %+v)", gotCaller, request, caller, want)
		}
		return []byte("sk-ant-synthetic-secret"), true, nil
	})
	handler := HandlerWithActionsRunnerExecutionLifecycle(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, []string{caller}, nil, nil, issuer)
	request := func(cert *x509.Certificate, verified bool, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/actions-runner/model-credential", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
		if verified {
			r.TLS.VerifiedChains = [][]*x509.Certificate{{cert}}
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec
	}

	validBody, _ := json.Marshal(want)
	noCert := request(&x509.Certificate{}, true, string(validBody))
	if noCert.Code != http.StatusUnauthorized || issued != 0 {
		t.Fatalf("missing URI cert: status=%d issuer calls=%d", noCert.Code, issued)
	}
	wrongCert := &x509.Certificate{URIs: []*url.URL{{Scheme: "spiffe", Host: "hako", Path: "/resource-plane/rp-other-01"}}}
	wrong := request(wrongCert, true, string(validBody))
	if wrong.Code != http.StatusUnauthorized || issued != 0 {
		t.Fatalf("wrong Plane cert: status=%d issuer calls=%d", wrong.Code, issued)
	}
	cert := &x509.Certificate{URIs: []*url.URL{{Scheme: "spiffe", Host: "hako", Path: "/resource-plane/rp-tokyo-01"}}}
	unverified := request(cert, false, string(validBody))
	if unverified.Code != http.StatusUnauthorized || issued != 0 {
		t.Fatalf("unverified cert: status=%d issuer calls=%d", unverified.Code, issued)
	}
	valid := request(cert, true, string(validBody))
	if valid.Code != http.StatusOK || issued != 1 || valid.Header().Get("Cache-Control") != "no-store" || strings.Contains(valid.Body.String(), "sk-ant-synthetic-secret") {
		t.Fatalf("credential response unsafe: status=%d issued=%d headers=%v body=%s", valid.Code, issued, valid.Header(), valid.Body.String())
	}
	var encoded struct {
		Credential string `json:"credential"`
	}
	if err := json.Unmarshal(valid.Body.Bytes(), &encoded); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded.Credential)
	if err != nil || string(decoded) != "sk-ant-synthetic-secret" {
		t.Fatalf("encoded credential mismatch: %q err=%v", decoded, err)
	}
	clear(decoded)
	if got := request(cert, true, string(validBody)); got.Code != http.StatusOK {
		t.Fatalf("second trusted issuance returned %d", got.Code)
	}

	noCredentialIssuer := modelCredentialIssuerFunc(func(context.Context, string, ActionsRunnerModelCredentialRequest) ([]byte, bool, error) {
		return nil, false, nil
	})
	noCredentialHandler := HandlerWithActionsRunnerExecutionLifecycle(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, []string{caller}, nil, nil, noCredentialIssuer)
	noCredential := httptest.NewRequest(http.MethodPost, "/v1/actions-runner/model-credential", strings.NewReader(string(validBody)))
	noCredential.Header.Set("Content-Type", "application/json")
	noCredential.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
	noCredentialRecorder := httptest.NewRecorder()
	noCredentialHandler.ServeHTTP(noCredentialRecorder, noCredential)
	if noCredentialRecorder.Code != http.StatusNoContent || noCredentialRecorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unconfigured Tenant credential response: status=%d headers=%v", noCredentialRecorder.Code, noCredentialRecorder.Header())
	}
}

func TestHTTPActionsRunnerModelCredentialClientBoundsAndWipesResponse(t *testing.T) {
	const secret = "sk-ant-synthetic-secret"
	client, err := NewHTTPActionsRunnerModelCredentialClient("https://broker.example/base", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.String() != "https://broker.example/base/v1/actions-runner/model-credential" ||
			r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unsafe request: method=%s url=%s headers=%v", r.Method, r.URL, r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		defer clear(body)
		var got ActionsRunnerModelCredentialRequest
		if err := json.Unmarshal(body, &got); err != nil || got.LambdaMicroVMID != "runner-vm-1" {
			t.Errorf("wrong request identity: %+v err=%v", got, err)
		}
		response, _ := json.Marshal(struct {
			Credential []byte `json:"credential"`
		}{[]byte(secret)})
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Cache-Control": []string{"no-store"}},
			Body: io.NopCloser(strings.NewReader(string(response))), ContentLength: int64(len(response)), Request: r}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	request := ActionsRunnerRegistrationRequest{IntegrationID: "integration_12345678", WorkflowJobID: 42, RunAttempt: 2, ClaimID: "claim_12345678"}
	value, err := client.IssueModelCredential(context.Background(), request, "runner-vm-1")
	if err != nil || string(value) != secret {
		t.Fatalf("credential client value=%q err=%v", value, err)
	}
	clear(value)

	unsafeClient, _ := NewHTTPActionsRunnerModelCredentialClient("http://broker.example", &http.Client{})
	if unsafeClient != nil {
		t.Fatal("HTTP broker URL accepted")
	}
}
