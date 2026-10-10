package githubbroker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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
	server := httptest.NewUnstartedServer(Handler(service))
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
	if _, err := NewMTLSClient(issue("spiffe://hako/resource-plane"), brokerRoots); err == nil {
		t.Fatal("client accepted another workload identity")
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
