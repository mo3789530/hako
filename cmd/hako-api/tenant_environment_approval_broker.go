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

// tenantEnvironmentApprovalBrokerFromEnv reuses the Control Plane mTLS
// identity configured for the Tenant GitHub broker. Secret access stays in
// the broker process.
func tenantEnvironmentApprovalBrokerFromEnv() (api.GitHubEnvironmentApprovalVerifier, error) {
	values := []string{
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_BROKER_URL")),
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_CLIENT_CERT_FILE")),
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_CLIENT_KEY_FILE")),
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_BROKER_CA_FILE")),
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
		return nil, errors.New("incomplete Tenant GitHub Environment approval broker configuration")
	}
	certificate, err := tls.LoadX509KeyPair(values[1], values[2])
	if err != nil {
		return nil, errors.New("invalid Tenant GitHub Environment approval broker client certificate")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, errors.New("invalid Tenant GitHub Environment approval broker client certificate")
	}
	certificate.Leaf = leaf
	caPEM, err := os.ReadFile(values[3])
	if err != nil {
		return nil, errors.New("cannot read Tenant GitHub Environment approval broker CA bundle")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("invalid Tenant GitHub Environment approval broker CA bundle")
	}
	client, err := githubbroker.NewEnvironmentApprovalMTLSClient(certificate, roots)
	if err != nil {
		return nil, errors.New("invalid Tenant GitHub Environment approval broker identity")
	}
	verifier, err := githubbroker.NewHTTPEnvironmentApprovalVerifier(values[0], client)
	if err != nil {
		return nil, errors.New("invalid Tenant GitHub Environment approval broker URL")
	}
	return verifier, nil
}
