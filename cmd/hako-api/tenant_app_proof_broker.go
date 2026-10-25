package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"strings"

	"github.com/mo3789530/hako/internal/api"
	"github.com/mo3789530/hako/internal/githubbroker"
)

// tenantAppProofBrokerFromEnv configures only the Control Plane mTLS client.
// The Broker resolves and reads the exact Tenant private-key Secret.
func tenantAppProofBrokerFromEnv() (api.TenantGitHubAppProofVerifier, error) {
	values := []string{
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_APP_PROOF_BROKER_URL")),
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_APP_PROOF_CLIENT_CERT_FILE")),
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_APP_PROOF_CLIENT_KEY_FILE")),
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_APP_PROOF_BROKER_CA_FILE")),
	}
	configured := 0
	for _, value := range values {
		if value != "" {
			configured++
		}
	}
	if configured == 0 {
		return nil, nil
	}
	if configured != len(values) {
		return nil, errors.New("incomplete Tenant GitHub App proof Broker configuration")
	}
	certificate, err := tls.LoadX509KeyPair(values[1], values[2])
	if err != nil {
		return nil, errors.New("invalid Tenant GitHub App proof Broker client certificate")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, errors.New("invalid Tenant GitHub App proof Broker client certificate")
	}
	certificate.Leaf = leaf
	caPEM, err := os.ReadFile(values[3])
	if err != nil {
		return nil, errors.New("cannot read Tenant GitHub App proof Broker CA bundle")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("invalid Tenant GitHub App proof Broker CA bundle")
	}
	client, err := githubbroker.NewTenantAppProofMTLSClient(certificate, roots)
	if err != nil {
		return nil, errors.New("invalid Tenant GitHub App proof Broker identity")
	}
	return githubbroker.NewHTTPTenantAppProofVerifier(values[0], client)
}
