package githubbroker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubregistry"
)

func proofRequestFixture() TenantAppProofRequest {
	return TenantAppProofRequest{TenantID: "tenant_proof", IntegrationID: "gint_proof", GitHubAppID: 123,
		ConfigRevision: 4, CredentialGeneration: 7}
}

func proofAccessFixture(request TenantAppProofRequest) githubapp.TenantPrivateKeyAccess {
	return githubapp.TenantPrivateKeyAccess{TenantID: string(request.TenantID), IntegrationID: request.IntegrationID,
		GitHubAppID: request.GitHubAppID, ConfigRevision: request.ConfigRevision, CredentialGeneration: request.CredentialGeneration,
		ReaderRoleARN:   "arn:aws:iam::123456789012:role/hako/tenant-app-reader/test",
		SecretARN:       "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/tenant-app/test",
		SecretVersionID: "0123456789abcdef0123456789abcdef"}
}

func TestTenantAppProofServiceRequiresControlPlaneAndExactResolvedIdentity(t *testing.T) {
	request := proofRequestFixture()
	access := proofAccessFixture(request)
	resolveCalls, verifyCalls := 0, 0
	service, err := NewTenantAppProofService(func(_ context.Context, got TenantAppProofRequest) (githubapp.TenantPrivateKeyAccess, error) {
		resolveCalls++
		if got != request {
			return githubapp.TenantPrivateKeyAccess{}, ErrTenantAppProofDenied
		}
		return access, nil
	}, func(_ context.Context, got githubapp.TenantPrivateKeyAccess) error {
		verifyCalls++
		if got != access {
			return ErrTenantAppProofDenied
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Verify(context.Background(), controlPlaneURI, request); err != nil || resolveCalls != 1 || verifyCalls != 1 {
		t.Fatalf("valid proof: resolve=%d verify=%d error=%v", resolveCalls, verifyCalls, err)
	}
	if err := service.Verify(context.Background(), "spiffe://hako/resource-plane/rp-test", request); err == nil || resolveCalls != 1 || verifyCalls != 1 {
		t.Fatalf("Resource Plane caller reached proof resolver: resolve=%d verify=%d error=%v", resolveCalls, verifyCalls, err)
	}
	wrong := request
	wrong.GitHubAppID++
	if err := service.Verify(context.Background(), controlPlaneURI, wrong); err == nil || verifyCalls != 1 {
		t.Fatalf("wrong App identity accepted: verify=%d error=%v", verifyCalls, err)
	}

	service, err = NewTenantAppProofService(func(context.Context, TenantAppProofRequest) (githubapp.TenantPrivateKeyAccess, error) {
		mismatched := access
		mismatched.CredentialGeneration++
		return mismatched, nil
	}, func(context.Context, githubapp.TenantPrivateKeyAccess) error {
		t.Fatal("mismatched resolved credential reached GitHub")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Verify(context.Background(), controlPlaneURI, request); err == nil {
		t.Fatal("mismatched credential generation was accepted")
	}
}

func TestTenantAppProofRouteRequiresControlPlaneAndStrictIdentifierOnlyRequest(t *testing.T) {
	request := proofRequestFixture()
	verifyCalls := 0
	service, err := NewTenantAppProofService(func(context.Context, TenantAppProofRequest) (githubapp.TenantPrivateKeyAccess, error) {
		return proofAccessFixture(request), nil
	}, func(context.Context, githubapp.TenantPrivateKeyAccess) error { verifyCalls++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	handler := WithTenantAppProof(http.NotFoundHandler(), service)
	invoke := func(method, body, contentType, caller string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, tenantAppProofRoute, strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		if caller != "" {
			u, _ := url.Parse(caller)
			cert := &x509.Certificate{URIs: []*url.URL{u}}
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder
	}
	valid, _ := json.Marshal(request)
	response := invoke(http.MethodPost, string(valid), "application/json", controlPlaneURI)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || verifyCalls != 1 {
		t.Fatalf("valid Control Plane proof response: status=%d calls=%d headers=%v body=%s", response.Code, verifyCalls, response.Header(), response.Body.String())
	}
	if got := invoke(http.MethodPost, string(valid), "application/json", "spiffe://hako/resource-plane/rp-test"); got.Code != http.StatusUnauthorized || verifyCalls != 1 {
		t.Fatalf("Resource Plane proof response: status=%d calls=%d", got.Code, verifyCalls)
	}
	for _, body := range []string{
		`{"tenant_id":"tenant_proof","integration_id":"gint_proof","github_app_id":123,"config_revision":4,"credential_generation":7,"secret_arn":"arn:secret"}`,
		string(valid) + `{}`,
		strings.Repeat(" ", 2049),
	} {
		if got := invoke(http.MethodPost, body, "application/json", controlPlaneURI); got.Code != http.StatusBadRequest {
			t.Fatalf("invalid proof request accepted: status=%d body=%s", got.Code, body)
		}
	}
	if got := invoke(http.MethodGet, string(valid), "application/json", controlPlaneURI); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET proof status=%d", got.Code)
	}
}

func TestHTTPTenantAppProofVerifierSendsOnlyPinsAndRejectsCacheableResponse(t *testing.T) {
	request := proofRequestFixture()
	access := proofAccessFixture(request)
	for _, test := range []struct {
		name    string
		status  int
		cache   string
		body    string
		wantErr bool
	}{
		{name: "verified", status: http.StatusOK, cache: "no-store", body: `{"verified":true}`},
		{name: "cacheable", status: http.StatusOK, cache: "public", body: `{"verified":true}`, wantErr: true},
		{name: "denied", status: http.StatusForbidden, body: `{"error":"denied"}`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != tenantAppProofRoute {
					t.Errorf("unexpected broker request %s %s", r.Method, r.URL.Path)
				}
				var fields map[string]json.RawMessage
				if json.NewDecoder(r.Body).Decode(&fields) != nil || len(fields) != 5 {
					t.Errorf("request fields = %v", fields)
				}
				for _, forbidden := range []string{"secret_arn", "reader_role_arn", "secret_version_id"} {
					if _, exists := fields[forbidden]; exists {
						t.Errorf("private credential reference %q was sent to broker", forbidden)
					}
				}
				if test.cache != "" {
					w.Header().Set("Cache-Control", test.cache)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			verifier, err := NewHTTPTenantAppProofVerifier(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			err = verifier.VerifyApp(context.Background(), githubregistry.AppProofCredential{TenantID: request.TenantID,
				IntegrationID: request.IntegrationID, GitHubAppID: request.GitHubAppID, ConfigRevision: request.ConfigRevision,
				CredentialGeneration: request.CredentialGeneration, PrivateKeySecretARN: access.SecretARN,
				PrivateKeyReaderRoleARN: access.ReaderRoleARN, PrivateKeyVersionID: access.SecretVersionID})
			if (err != nil) != test.wantErr {
				t.Fatalf("VerifyApp error=%v wantErr=%t", err, test.wantErr)
			}
		})
	}
}
