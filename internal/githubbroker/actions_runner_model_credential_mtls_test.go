package githubbroker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestActionsRunnerModelCredentialUsesVerifiedResourcePlaneMTLS(t *testing.T) {
	const (
		planeID = "rp-tokyo-01"
		caller  = "spiffe://hako/resource-plane/rp-tokyo-01"
		secret  = "sk-ant-synthetic-mtls-credential"
	)
	caPublic, caPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Hako test broker CA"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caPublic, caPrivate)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(serial int64, identity string, usage x509.ExtKeyUsage, server bool) tls.Certificate {
		t.Helper()
		publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, err := url.Parse(identity)
		if err != nil {
			t.Fatal(err)
		}
		certificateTemplate := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: identity},
			NotBefore: now.Add(-time.Minute), NotAfter: now.Add(30 * time.Minute), KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{usage}, URIs: []*url.URL{uri}}
		if server {
			certificateTemplate.DNSNames = []string{"localhost"}
			certificateTemplate.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		}
		der, err := x509.CreateCertificate(rand.Reader, certificateTemplate, caCert, publicKey, caPrivate)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey, Leaf: leaf}
	}

	clientRoots := x509.NewCertPool()
	clientRoots.AddCert(caCert)
	serverRoots := x509.NewCertPool()
	serverRoots.AddCert(caCert)
	serverCertificate := issue(2, "spiffe://hako/github-broker", x509.ExtKeyUsageServerAuth, true)
	clientCertificate := issue(3, caller, x509.ExtKeyUsageClientAuth, false)
	issuerCalls := 0
	issuer := modelCredentialIssuerFunc(func(_ context.Context, gotCaller string, request ActionsRunnerModelCredentialRequest) ([]byte, bool, error) {
		issuerCalls++
		if gotCaller != caller || request != (ActionsRunnerModelCredentialRequest{IntegrationID: "integration_tls_123", WorkflowJobID: 42,
			RunAttempt: 2, ClaimID: "claim_tls_123", LambdaMicroVMID: "runner-vm-tls"}) {
			t.Fatalf("issuer got caller/request (%q, %+v)", gotCaller, request)
		}
		return []byte(secret), true, nil
	})
	handler := HandlerWithActionsRunnerExecutionLifecycle(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, []string{caller}, nil, nil, issuer)
	server := httptest.NewUnstartedServer(handler)
	serverTLS, err := ServerTLSConfig(clientRoots)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS.Certificates = []tls.Certificate{serverCertificate}
	server.TLS = serverTLS
	server.StartTLS()
	defer server.Close()

	transport, err := NewActionsRunnerBootstrapTokenMTLSClient(planeID, clientCertificate, serverRoots)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewHTTPActionsRunnerBootstrapTokenClient(server.URL, transport)
	if err != nil {
		t.Fatal(err)
	}
	request := ActionsRunnerRegistrationRequest{IntegrationID: "integration_tls_123", WorkflowJobID: 42, RunAttempt: 2, ClaimID: "claim_tls_123"}
	credential, err := client.IssueModelCredential(context.Background(), request, "runner-vm-tls")
	if err != nil || string(credential) != secret || issuerCalls != 1 {
		t.Fatalf("verified mTLS credential exchange returned %q, calls=%d, err=%v", credential, issuerCalls, err)
	}
	clear(credential)

	wrongCertificate := issue(4, "spiffe://hako/resource-plane/rp-other-01", x509.ExtKeyUsageClientAuth, false)
	wrongTransport, err := NewActionsRunnerBootstrapTokenMTLSClient("rp-other-01", wrongCertificate, serverRoots)
	if err != nil {
		t.Fatal(err)
	}
	wrongClient, err := NewHTTPActionsRunnerBootstrapTokenClient(server.URL, wrongTransport)
	if err != nil {
		t.Fatal(err)
	}
	if value, err := wrongClient.IssueModelCredential(context.Background(), request, "runner-vm-tls"); err == nil || value != nil || issuerCalls != 1 {
		clear(value)
		t.Fatalf("non-allowlisted Resource Plane reached model credential issuer: value-present=%t calls=%d err=%v", value != nil, issuerCalls, err)
	}

	noCertificate := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: serverRoots}}}
	plainClient, err := NewHTTPActionsRunnerModelCredentialClient(server.URL, noCertificate)
	if err != nil {
		t.Fatal(err)
	}
	if value, err := plainClient.IssueModelCredential(context.Background(), request, "runner-vm-tls"); err == nil || value != nil || issuerCalls != 1 {
		clear(value)
		t.Fatalf("client without mTLS identity reached issuer: value-present=%t calls=%d err=%v", value != nil, issuerCalls, err)
	}
}
