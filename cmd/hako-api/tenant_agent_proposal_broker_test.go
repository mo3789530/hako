package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"reflect"
	"testing"
	"time"
)

func TestTenantAgentProposalBrokerConfigurationIsOptionalButAllOrNothing(t *testing.T) {
	for _, name := range []string{"HAKO_TENANT_GITHUB_OAUTH_BROKER_URL", "HAKO_TENANT_GITHUB_OAUTH_CLIENT_CERT_FILE",
		"HAKO_TENANT_GITHUB_OAUTH_CLIENT_KEY_FILE", "HAKO_TENANT_GITHUB_OAUTH_BROKER_CA_FILE",
		"HAKO_TENANT_GITHUB_OAUTH_CLIENT_CERT_SECRET_ARN", "HAKO_TENANT_GITHUB_OAUTH_CLIENT_KEY_SECRET_ARN",
		"HAKO_TENANT_GITHUB_OAUTH_BROKER_CA_SECRET_ARN"} {
		t.Setenv(name, "")
	}
	client, err := tenantAgentProposalBrokerFromEnv()
	if err != nil || client != nil {
		t.Fatalf("unconfigured publication Broker client=%v error=%v", client, err)
	}
	t.Setenv("HAKO_TENANT_GITHUB_OAUTH_BROKER_URL", "https://broker.internal")
	if _, err := tenantAgentProposalBrokerFromEnv(); err == nil {
		t.Fatal("partial publication Broker configuration was accepted")
	}
}

func TestTenantAgentProposalBrokerLoadsOnlyConfiguredSecretARNs(t *testing.T) {
	t.Setenv("HAKO_TENANT_GITHUB_OAUTH_BROKER_URL", "https://broker.internal")
	t.Setenv("HAKO_TENANT_GITHUB_OAUTH_CLIENT_CERT_FILE", "")
	t.Setenv("HAKO_TENANT_GITHUB_OAUTH_CLIENT_KEY_FILE", "")
	t.Setenv("HAKO_TENANT_GITHUB_OAUTH_BROKER_CA_FILE", "")
	arns := []string{
		"arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:broker/client-cert",
		"arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:broker/client-key",
		"arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:broker/client-ca",
	}
	for i, name := range []string{"HAKO_TENANT_GITHUB_OAUTH_CLIENT_CERT_SECRET_ARN", "HAKO_TENANT_GITHUB_OAUTH_CLIENT_KEY_SECRET_ARN", "HAKO_TENANT_GITHUB_OAUTH_BROKER_CA_SECRET_ARN"} {
		t.Setenv(name, arns[i])
	}

	certPEM, keyPEM := syntheticBrokerClientCertificate(t)
	var read []string
	getSecret := func(_ context.Context, arn string) ([]byte, error) {
		read = append(read, arn)
		switch arn {
		case arns[0], arns[2]:
			return certPEM, nil
		case arns[1]:
			return keyPEM, nil
		default:
			t.Fatalf("unexpected Secret ARN read: %s", arn)
			return nil, nil
		}
	}
	client, err := tenantAgentProposalBrokerFromEnvWithSecretGetter(context.Background(), getSecret)
	if err != nil || client == nil {
		t.Fatalf("tenantAgentProposalBrokerFromEnvWithSecretGetter() = %v, %v", client, err)
	}
	if !reflect.DeepEqual(read, arns) {
		t.Fatalf("Secret reads = %#v, want %#v", read, arns)
	}
}

func TestTenantAgentProposalBrokerRejectsPartialSecretARNsWithoutReading(t *testing.T) {
	t.Setenv("HAKO_TENANT_GITHUB_OAUTH_BROKER_URL", "https://broker.internal")
	t.Setenv("HAKO_TENANT_GITHUB_OAUTH_CLIENT_CERT_SECRET_ARN", "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:cert")
	t.Setenv("HAKO_TENANT_GITHUB_OAUTH_CLIENT_KEY_SECRET_ARN", "")
	t.Setenv("HAKO_TENANT_GITHUB_OAUTH_BROKER_CA_SECRET_ARN", "")
	called := false
	_, err := tenantAgentProposalBrokerFromEnvWithSecretGetter(context.Background(), func(context.Context, string) ([]byte, error) {
		called = true
		return nil, nil
	})
	if err == nil || called {
		t.Fatalf("partial Secret config err=%v reader_called=%v", err, called)
	}
}

func syntheticBrokerClientCertificate(t *testing.T) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "hako-control-plane"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:        []*url.URL{{Scheme: "spiffe", Host: "hako", Path: "/control-plane"}},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}
