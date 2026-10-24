package githubbroker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mo3789530/hako/internal/deploypolicy"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
)

func environmentApprovalTestPin() EnvironmentApprovalPin {
	return EnvironmentApprovalPin{TenantID: "tenant_approval", ServiceID: "payments-api", Installation: 71, RepositoryID: 81,
		IntegrationID: "integration_approval", App: githubapp.TenantPrivateKeyAccess{TenantID: "tenant_approval", IntegrationID: "integration_approval",
			GitHubAppID: 91, ConfigRevision: 2, CredentialGeneration: 3, ReaderRoleARN: "reader-role", SecretARN: "secret-arn", SecretVersionID: "secret-version"},
		Repository: githubapp.RepositoryAccess{Owner: "acme", Name: "payments"}}
}

func TestEnvironmentApprovalServiceRequiresControlPlaneAndRevalidatesPin(t *testing.T) {
	t.Run("verifies configured environment and rechecks pin", func(t *testing.T) {
		calls, reads := 0, 0
		pin := environmentApprovalTestPin()
		service, err := NewEnvironmentApprovalService(func(_ context.Context, tenant domain.TenantID, serviceID string, installation, repository int64, environment string) (EnvironmentApprovalPin, error) {
			calls++
			if tenant != pin.TenantID || serviceID != pin.ServiceID || installation != pin.Installation || repository != pin.RepositoryID || environment != "production" {
				t.Fatal("resolver received untrusted or unexpected resource coordinates")
			}
			return pin, nil
		}, func(_ context.Context, got EnvironmentApprovalPin, environment string) error {
			reads++
			if got != pin || environment != "production" {
				t.Fatal("reader received an unexpected App or Environment binding")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Verify(context.Background(), controlPlaneURI, pin.TenantID, pin.ServiceID, pin.Installation, pin.RepositoryID, "production"); err != nil {
			t.Fatalf("Verify() error = %v", err)
		}
		if calls != 2 || reads != 1 {
			t.Fatalf("resolver calls=%d reader calls=%d", calls, reads)
		}
	})

	t.Run("denies a changed App pin after GitHub read", func(t *testing.T) {
		pin := environmentApprovalTestPin()
		calls := 0
		service, err := NewEnvironmentApprovalService(func(context.Context, domain.TenantID, string, int64, int64, string) (EnvironmentApprovalPin, error) {
			calls++
			if calls == 2 {
				changed := pin
				changed.App.CredentialGeneration++
				return changed, nil
			}
			return pin, nil
		}, func(context.Context, EnvironmentApprovalPin, string) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Verify(context.Background(), controlPlaneURI, pin.TenantID, pin.ServiceID, pin.Installation, pin.RepositoryID, "production"); !errors.Is(err, ErrEnvironmentApprovalDenied) {
			t.Fatalf("Verify() error = %v, want denied", err)
		}
	})

	t.Run("distinguishes missing approval from provider outage", func(t *testing.T) {
		for _, tt := range []struct {
			name    string
			readErr error
			wantErr error
		}{{"not configured", githubapp.ErrEnvironmentApprovalMissing, ErrEnvironmentApprovalDenied}, {"provider unavailable", errors.New("network failure"), ErrEnvironmentApprovalUnavailable}} {
			t.Run(tt.name, func(t *testing.T) {
				pin := environmentApprovalTestPin()
				service, _ := NewEnvironmentApprovalService(func(context.Context, domain.TenantID, string, int64, int64, string) (EnvironmentApprovalPin, error) {
					return pin, nil
				},
					func(context.Context, EnvironmentApprovalPin, string) error { return tt.readErr })
				if err := service.Verify(context.Background(), controlPlaneURI, pin.TenantID, pin.ServiceID, pin.Installation, pin.RepositoryID, "production"); !errors.Is(err, tt.wantErr) {
					t.Fatalf("Verify() error=%v, want %v", err, tt.wantErr)
				}
			})
		}
	})

	t.Run("rejects non-Control Plane caller before resolution", func(t *testing.T) {
		called := false
		service, _ := NewEnvironmentApprovalService(func(context.Context, domain.TenantID, string, int64, int64, string) (EnvironmentApprovalPin, error) {
			called = true
			return EnvironmentApprovalPin{}, nil
		}, func(context.Context, EnvironmentApprovalPin, string) error { return nil })
		if err := service.Verify(context.Background(), "spiffe://hako/resource-plane/rp-1", "tenant_approval", "payments-api", 71, 81, "production"); !errors.Is(err, ErrEnvironmentApprovalDenied) {
			t.Fatalf("Verify() error = %v", err)
		}
		if called {
			t.Fatal("untrusted caller reached resolver")
		}
	})
}

func TestEnvironmentApprovalHTTPClientUsesBoundedControlPlaneEndpoint(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != environmentApprovalRoute || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, environmentApprovalMaxBody+1))
		if err != nil || len(body) > environmentApprovalMaxBody || !strings.Contains(string(body), `"service_id":"payments-api"`) {
			t.Errorf("unexpected request body %q, error=%v", body, err)
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"verified":true}`))
	}))
	defer server.Close()
	client, err := NewHTTPEnvironmentApprovalVerifier(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Verify(context.Background(), "tenant_approval", "payments-api", 71, 81, "production"); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}

	denied := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"broker_unavailable"}`))
	}))
	defer denied.Close()
	client, err = NewHTTPEnvironmentApprovalVerifier(denied.URL, denied.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Verify(context.Background(), "tenant_approval", "payments-api", 71, 81, "production"); !errors.Is(err, deploypolicy.ErrInvalidToken) {
		t.Fatalf("denied Verify() error = %v", err)
	}
}

func TestEnvironmentApprovalRouteRequiresControlPlaneIdentityAndReturnsOnlyVerification(t *testing.T) {
	pin := environmentApprovalTestPin()
	resolved := 0
	service, err := NewEnvironmentApprovalService(func(context.Context, domain.TenantID, string, int64, int64, string) (EnvironmentApprovalPin, error) {
		resolved++
		return pin, nil
	}, func(context.Context, EnvironmentApprovalPin, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	handler := WithEnvironmentApproval(http.NotFoundHandler(), service)
	invoke := func(cert *x509.Certificate, verified bool, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, environmentApprovalRoute, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cert != nil {
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
			if verified {
				req.TLS.VerifiedChains = [][]*x509.Certificate{{cert}}
			}
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	controlPlane := &x509.Certificate{URIs: []*url.URL{{Scheme: "spiffe", Host: "hako", Path: "/control-plane"}}}
	resourcePlane := &x509.Certificate{URIs: []*url.URL{{Scheme: "spiffe", Host: "hako", Path: "/resource-plane/rp-tokyo-01"}}}
	body := `{"tenant_id":"tenant_approval","service_id":"payments-api","installation_id":71,"repository_id":81,"environment":"production"}`
	if got := invoke(nil, false, body); got.Code != http.StatusUnauthorized || resolved != 0 {
		t.Fatalf("unauthenticated request status=%d resolved=%d", got.Code, resolved)
	}
	if got := invoke(resourcePlane, true, body); got.Code != http.StatusUnauthorized || resolved != 0 {
		t.Fatalf("Resource Plane request status=%d resolved=%d", got.Code, resolved)
	}
	got := invoke(controlPlane, true, body)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"verified":true`) || strings.Contains(got.Body.String(), "token") || resolved != 2 {
		t.Fatalf("Control Plane result status=%d body=%s resolved=%d", got.Code, got.Body.String(), resolved)
	}
	if got := invoke(controlPlane, true, `{"tenant_id":"tenant_approval","service_id":"payments-api","installation_id":71,"repository_id":81,"environment":"production","secret":"unexpected"}`); got.Code != http.StatusBadRequest {
		t.Fatalf("unknown request field status=%d body=%s", got.Code, got.Body.String())
	}
}
