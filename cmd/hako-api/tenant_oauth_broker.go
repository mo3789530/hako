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

// tenantOAuthBrokerFromEnv configures only the mTLS API client. Secret access
// remains inside the broker workload and is never enabled in the public API.
func tenantOAuthBrokerFromEnv() (api.TenantGitHubSetupVerifierResolver, error) {
	values := []string{
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_BROKER_URL")),
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_CLIENT_CERT_FILE")),
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_CLIENT_KEY_FILE")),
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_BROKER_CA_FILE")),
	}
	set := 0
	for _, value := range values {
		if value != "" {
			set++
		}
	}
	if set == 0 {
		return nil, nil
	}
	if set != len(values) {
		return nil, errors.New("incomplete Tenant GitHub OAuth broker configuration")
	}
	certificate, err := tls.LoadX509KeyPair(values[1], values[2])
	if err != nil {
		return nil, errors.New("invalid Tenant GitHub OAuth broker client certificate")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, errors.New("invalid Tenant GitHub OAuth broker client certificate")
	}
	certificate.Leaf = leaf
	caPEM, err := os.ReadFile(values[3])
	if err != nil {
		return nil, errors.New("cannot read Tenant GitHub OAuth broker CA bundle")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("invalid Tenant GitHub OAuth broker CA bundle")
	}
	client, err := githubbroker.NewMTLSClient(certificate, roots)
	if err != nil {
		return nil, errors.New("invalid Tenant GitHub OAuth broker identity")
	}
	resolver, err := githubbroker.NewHTTPResolver(values[0], client)
	if err != nil {
		return nil, errors.New("invalid Tenant GitHub OAuth broker URL")
	}
	return resolver, nil
}
