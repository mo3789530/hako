package githubbroker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubregistry"
)

func TestBrokerHTTPRequiresVerifiedControlPlaneCertificateBeforeResolution(t *testing.T) {
	pin := githubregistry.SetupAppPin{TenantID: "tenant_a", IntegrationID: "integration_a", AppRegistrationID: "app_a", GitHubAppID: 7, ConfigRevision: 1, CredentialGeneration: 2}
	resolved := 0
	service, err := NewOAuthService(func(ctx context.Context, p githubregistry.SetupAppPin) error {
		caller, ok := CallerFromContext(ctx)
		if !ok || caller != controlPlaneURI || p != pin {
			return ErrUnavailable
		}
		return nil
	}, func(_ context.Context, p githubregistry.SetupAppPin) (githubregistry.SetupCredential, error) {
		resolved++
		return githubregistry.SetupCredential{Pin: p, Slug: "test-app", ClientID: "client"}, nil
	}, func(context.Context, githubregistry.SetupCredential) (string, error) {
		t.Fatal("setup URL read a secret")
		return "", nil
	}, "https://hako.example/callback")
	if err != nil {
		t.Fatal(err)
	}
	handler := Handler(service)
	body, _ := json.Marshal(request{Pin: pin, State: "state"})
	invoke := func(cert *x509.Certificate, verified bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/oauth/setup-url", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		if cert != nil {
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
			if verified {
				req.TLS.VerifiedChains = [][]*x509.Certificate{{cert}}
			}
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if got := invoke(nil, false); got.Code != http.StatusUnauthorized || resolved != 0 {
		t.Fatalf("no cert: status=%d resolver=%d", got.Code, resolved)
	}
	wrong := &x509.Certificate{URIs: []*url.URL{{Scheme: "spiffe", Host: "hako", Path: "/resource-plane"}}}
	if got := invoke(wrong, true); got.Code != http.StatusUnauthorized || resolved != 0 {
		t.Fatalf("wrong identity: status=%d resolver=%d", got.Code, resolved)
	}
	control := &x509.Certificate{URIs: []*url.URL{{Scheme: "spiffe", Host: "hako", Path: "/control-plane"}}}
	if got := invoke(control, false); got.Code != http.StatusUnauthorized || resolved != 0 {
		t.Fatalf("unverified identity: status=%d resolver=%d", got.Code, resolved)
	}
	got := invoke(control, true)
	if got.Code != http.StatusOK || resolved != 1 || got.Header().Get("Cache-Control") != "no-store" || !strings.Contains(got.Body.String(), "setup_url") {
		t.Fatalf("authorized request failed: status=%d resolver=%d body=%s", got.Code, resolved, got.Body.String())
	}
}

func TestBrokerMTLSHandshakeAndHTTPResolver(t *testing.T) {
	clientRoot, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test broker client CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, clientRoot, clientKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	clientRoots := x509.NewCertPool()
	clientRoots.AddCert(caCert)
	issue := func(uri string) tls.Certificate {
		pub, key, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		parsed, e := url.Parse(uri)
		if e != nil {
			t.Fatal(e)
		}
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "test workload"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{parsed}}
		der, e := x509.CreateCertificate(rand.Reader, tmpl, caCert, pub, clientKey)
		if e != nil {
			t.Fatal(e)
		}
		leaf, e := x509.ParseCertificate(der)
		if e != nil {
			t.Fatal(e)
		}
		return tls.Certificate{Certificate: [][]byte{der, caDER}, PrivateKey: key, Leaf: leaf}
	}
	pin := githubregistry.SetupAppPin{TenantID: "tenant_tls", IntegrationID: "integration_tls", AppRegistrationID: "app_tls", GitHubAppID: 11, ConfigRevision: 2, CredentialGeneration: 3}
	resolved := 0
	service, err := NewOAuthService(func(ctx context.Context, p githubregistry.SetupAppPin) error {
		caller, ok := CallerFromContext(ctx)
		if !ok || caller != controlPlaneURI || p != pin {
			return ErrUnavailable
		}
		return nil
	}, func(_ context.Context, p githubregistry.SetupAppPin) (githubregistry.SetupCredential, error) {
		resolved++
		return githubregistry.SetupCredential{Pin: p, Slug: "test-app", ClientID: "client"}, nil
	}, func(context.Context, githubregistry.SetupCredential) (string, error) {
		t.Fatal("URL operation read a Secret")
		return "", nil
	}, "https://hako.example/callback")
	if err != nil {
		t.Fatal(err)
	}
	run, access := checkoutFixture()
	checkout, err := NewCheckoutService(func(context.Context, string, domain.TenantID, domain.WorkloadRunID) error { return nil },
		func(_ context.Context, tenant domain.TenantID, runID domain.WorkloadRunID) (domain.WorkloadRun, githubapp.TenantPrivateKeyAccess, error) {
			if tenant != run.TenantID || runID != run.ID {
				return domain.WorkloadRun{}, githubapp.TenantPrivateKeyAccess{}, ErrCheckoutDenied
			}
			return run, access, nil
		}, func(context.Context, domain.WorkloadRun, githubapp.TenantPrivateKeyAccess) (string, time.Time, error) {
			return "ghs_mtls_checkout_token_value", time.Now().Add(time.Hour), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	checksIdentity := checksFixture()
	checksIdentity.Run = run
	checksIdentity.Access = access
	checksIdentity.Repository = githubapp.RepositoryAccess{Owner: "acme", Name: "api"}
	checksService, err := NewChecksService(func(context.Context, string, domain.TenantID, domain.WorkloadRunID) error { return nil },
		func(_ context.Context, tenant domain.TenantID, runID domain.WorkloadRunID) (ChecksRunIdentity, error) {
			if tenant != run.TenantID || runID != run.ID {
				return ChecksRunIdentity{}, ErrChecksDenied
			}
			return checksIdentity, nil
		}, func(context.Context, ChecksRunIdentity) (githubapp.CheckToken, error) {
			return fakeBrokerChecksToken{value: "ghs_mtls_checks_write_token", expiry: time.Now().Add(time.Hour)}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(HandlerWithServices(service, checkout, checksService))
	serverCAs, _ := ServerTLSConfig(clientRoots)
	server.TLS = serverCAs
	server.StartTLS()
	defer server.Close()
	brokerRoots := x509.NewCertPool()
	brokerRoots.AddCert(server.Certificate())
	goodClient, err := NewMTLSClient(issue(controlPlaneURI), brokerRoots)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &HTTPResolver{BaseURL: server.URL, Client: goodClient}
	verifier, err := resolver.ResolveSetupVerifier(context.Background(), pin)
	if err != nil {
		t.Fatal(err)
	}
	if got := verifier.SetupURL("opaque-state"); !strings.Contains(got, "test-app") {
		t.Fatalf("mTLS setup URL failed: %q", got)
	}
	if resolved != 1 {
		t.Fatalf("unexpected credential resolution count %d", resolved)
	}
	checkoutClient, err := NewCheckoutMTLSClient(issue(controlPlaneURI), brokerRoots)
	if err != nil {
		t.Fatal(err)
	}
	checkoutProvider, err := NewHTTPCheckoutProvider(server.URL, checkoutClient)
	if err != nil {
		t.Fatal(err)
	}
	if token, err := checkoutProvider.CheckoutRunToken(context.Background(), run); err != nil || token != "ghs_mtls_checkout_token_value" {
		t.Fatalf("mTLS checkout token request failed: token=%q err=%v", token, err)
	}
	checksClient, err := NewChecksMTLSClient(issue(controlPlaneURI), brokerRoots)
	if err != nil {
		t.Fatal(err)
	}
	checksProvider, err := NewHTTPChecksCredentialProvider(server.URL, checksClient)
	if err != nil {
		t.Fatal(err)
	}
	checkToken, repository, err := checksProvider.ChecksRunToken(context.Background(), run)
	if err != nil || checkToken.Value() != "ghs_mtls_checks_write_token" || repository != checksIdentity.Repository {
		t.Fatalf("mTLS Checks credential request failed: repository=%+v err=%v", repository, err)
	}
	if fmt.Sprint(checkToken) == checkToken.Value() {
		t.Fatal("HTTP Checks credential formatting exposed the token")
	}
	if _, err := NewMTLSClient(issue("spiffe://hako/resource-plane"), brokerRoots); err == nil {
		t.Fatal("client accepted another workload identity")
	}
	resourcePlaneClient, err := NewResourcePlaneCheckoutMTLSClient("rp-tokyo-01", issue("spiffe://hako/resource-plane/rp-tokyo-01"), brokerRoots)
	if err != nil || resourcePlaneClient == nil {
		t.Fatalf("Resource Plane checkout mTLS client failed: %v", err)
	}
	if _, err := NewResourcePlaneCheckoutMTLSClient("rp-tokyo-01", issue(controlPlaneURI), brokerRoots); err == nil {
		t.Fatal("Resource Plane checkout client accepted a Control Plane certificate")
	}
	if _, err := NewResourcePlaneCheckoutMTLSClient("rp-tokyo-01", issue("spiffe://hako/resource-plane/rp-other"), brokerRoots); err == nil {
		t.Fatal("Resource Plane checkout client accepted another Plane certificate")
	}
	wrongTransport := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: brokerRoots, Certificates: []tls.Certificate{issue("spiffe://hako/resource-plane")}}}}
	wrongResponse, err := wrongTransport.Post(server.URL+"/v1/oauth/setup-url", "application/json", strings.NewReader(`{"pin":{},"state":"state"}`))
	if err == nil {
		wrongResponse.Body.Close()
		if wrongResponse.StatusCode != http.StatusUnauthorized || resolved != 1 {
			t.Fatal("server accepted wrong workload SAN")
		}
	}
	// A TLS client without a certificate cannot complete the required handshake.
	plain := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: brokerRoots, MinVersion: tls.VersionTLS13}}}
	if _, err := plain.Get(server.URL); err == nil {
		t.Fatal("client without a certificate completed mTLS")
	}
}

func TestBrokerTLSConfigurationRequiresClientTrust(t *testing.T) {
	if _, err := ServerTLSConfig(nil); err == nil {
		t.Fatal("nil trust root accepted")
	}
	roots := x509.NewCertPool()
	cfg, err := ServerTLSConfig(roots)
	if err != nil || cfg.MinVersion != tls.VersionTLS13 || cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatal("server did not require TLS client authentication")
	}
	if _, err := NewMTLSClient(tls.Certificate{}, roots); err == nil {
		t.Fatal("empty workload certificate accepted")
	}
}

func TestCheckoutEndpointRequiresVerifiedControlPlaneAndNeverAcceptsCredentialPins(t *testing.T) {
	run, access := checkoutFixture()
	resolveCalls, mintCalls := 0, 0
	service, err := NewCheckoutService(func(context.Context, string, domain.TenantID, domain.WorkloadRunID) error { return nil },
		func(_ context.Context, tenant domain.TenantID, runID domain.WorkloadRunID) (domain.WorkloadRun, githubapp.TenantPrivateKeyAccess, error) {
			resolveCalls++
			if tenant != run.TenantID || runID != run.ID {
				return domain.WorkloadRun{}, githubapp.TenantPrivateKeyAccess{}, ErrCheckoutDenied
			}
			return run, access, nil
		}, func(context.Context, domain.WorkloadRun, githubapp.TenantPrivateKeyAccess) (string, time.Time, error) {
			mintCalls++
			return "ghs_checkout_token_never_log_this", time.Now().Add(time.Hour), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	handler := HandlerWithCheckout(nil, service)
	invoke := func(body string, cert *x509.Certificate, verified bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/checkout/token", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cert != nil {
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
			if verified {
				req.TLS.VerifiedChains = [][]*x509.Certificate{{cert}}
			}
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	control := &x509.Certificate{URIs: []*url.URL{{Scheme: "spiffe", Host: "hako", Path: "/control-plane"}}}
	body := `{"tenant_id":"tenant_checkout","run_id":"run_checkout"}`
	if got := invoke(body, nil, false); got.Code != http.StatusUnauthorized || resolveCalls != 0 || mintCalls != 0 {
		t.Fatal("checkout endpoint processed an unauthenticated caller")
	}
	if got := invoke(`{"tenant_id":"tenant_checkout","run_id":"run_checkout","pin":{"private_key_secret_arn":"secret"}}`, control, true); got.Code != http.StatusBadRequest || resolveCalls != 0 || mintCalls != 0 {
		t.Fatal("checkout endpoint accepted caller-supplied credential data")
	}
	got := invoke(body, control, true)
	if got.Code != http.StatusOK || got.Header().Get("Cache-Control") != "no-store" || resolveCalls != 2 || mintCalls != 1 || !strings.Contains(got.Body.String(), "ghs_checkout_token_never_log_this") {
		t.Fatalf("authorized checkout response status=%d resolve=%d mint=%d body=%s", got.Code, resolveCalls, mintCalls, got.Body.String())
	}
}

func TestResourcePlaneIdentityIsCheckoutOnlyAndRequiresExactAllowlist(t *testing.T) {
	run, access := checkoutFixture()
	service, err := NewCheckoutService(func(_ context.Context, caller string, _ domain.TenantID, _ domain.WorkloadRunID) error {
		if caller != controlPlaneURI && caller != resourcePlaneURIPrefix+"rp-tokyo-01" {
			return ErrCheckoutDenied
		}
		return nil
	}, func(_ context.Context, tenant domain.TenantID, runID domain.WorkloadRunID) (domain.WorkloadRun, githubapp.TenantPrivateKeyAccess, error) {
		if tenant != run.TenantID || runID != run.ID {
			return domain.WorkloadRun{}, githubapp.TenantPrivateKeyAccess{}, ErrCheckoutDenied
		}
		return run, access, nil
	}, func(context.Context, domain.WorkloadRun, githubapp.TenantPrivateKeyAccess) (string, time.Time, error) {
		return "ghs_resource_plane_checkout_token", time.Now().Add(time.Minute), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := ResourcePlaneCallerID("rp-tokyo-01")
	if err != nil || identity != "spiffe://hako/resource-plane/rp-tokyo-01" {
		t.Fatalf("ResourcePlaneCallerID = %q, %v", identity, err)
	}
	for _, invalid := range []string{"", "RP-TOKYO", "../control-plane", "resource_plane"} {
		if _, err := ResourcePlaneCallerID(invalid); err == nil {
			t.Fatalf("invalid Resource Plane identity %q was accepted", invalid)
		}
	}
	handler := HandlerWithServicesAndCheckoutCallers(nil, service, nil, []string{identity})
	invoke := func(path, caller string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"tenant_id":"tenant_checkout","run_id":"run_checkout"}`))
		req.Header.Set("Content-Type", "application/json")
		cert := &x509.Certificate{URIs: []*url.URL{{Scheme: "spiffe", Host: "hako", Path: "/resource-plane/rp-tokyo-01"}}}
		if caller == "spiffe://hako/resource-plane/rp-other" {
			cert.URIs[0] = &url.URL{Scheme: "spiffe", Host: "hako", Path: "/resource-plane/rp-other"}
		}
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if got := invoke("/v1/checkout/token", identity); got.Code != http.StatusOK {
		t.Fatalf("allowlisted Resource Plane checkout status = %d body=%s", got.Code, got.Body.String())
	}
	if got := invoke("/v1/checkout/token", "spiffe://hako/resource-plane/rp-other"); got.Code != http.StatusUnauthorized {
		t.Fatalf("unregistered Resource Plane checkout status = %d", got.Code)
	}
	if got := invoke("/v1/oauth/setup-url", identity); got.Code != http.StatusUnauthorized {
		t.Fatalf("Resource Plane identity must not access OAuth endpoint: status=%d", got.Code)
	}
}

func TestHTTPCheckoutProviderUsesPinnedRunIDsAndRejectsRedirectOrCacheableResponse(t *testing.T) {
	run, _ := checkoutFixture()
	for _, mode := range []string{"success", "cacheable", "redirect", "expired"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/checkout/token" || r.Method != http.MethodPost {
					t.Errorf("unexpected broker request %s %s", r.Method, r.URL.Path)
				}
				var payload map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || len(payload) != 2 || string(payload["tenant_id"]) != `"tenant_checkout"` || string(payload["run_id"]) != `"run_checkout"` {
					t.Errorf("broker request did not contain only canonical identifiers: %s", fmt.Sprint(payload))
				}
				if mode == "redirect" {
					http.Redirect(w, r, "/capture", http.StatusTemporaryRedirect)
					return
				}
				if mode != "cacheable" {
					w.Header().Set("Cache-Control", "no-store")
				}
				expires := time.Now().Add(time.Hour)
				if mode == "expired" {
					expires = time.Now().Add(-time.Minute)
				}
				_ = json.NewEncoder(w).Encode(response{Token: "ghs_checkout_token_never_log_this", ExpiresAt: expires})
			}))
			defer server.Close()
			client := server.Client()
			provider, err := NewHTTPCheckoutProvider(server.URL, client)
			if err != nil {
				t.Fatal(err)
			}
			token, err := provider.CheckoutRunToken(context.Background(), run)
			if mode == "success" {
				if err != nil || token != "ghs_checkout_token_never_log_this" {
					t.Fatalf("CheckoutRunToken() token=%q err=%v", token, err)
				}
			} else if err == nil || token != "" {
				t.Fatal("unsafe broker response was accepted")
			}
		})
	}
}
