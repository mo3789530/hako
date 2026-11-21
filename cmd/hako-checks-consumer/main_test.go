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
	"os"
	"testing"
	"time"
)

func TestOpenDatabaseRejectsMixedModes(t *testing.T) {
	t.Setenv("HAKO_DATABASE_URL", "postgres://local/db")
	t.Setenv("HAKO_DSQL_HOST", "cluster.dsql.us-east-1.on.aws")
	if _, err := openDatabase(context.Background()); err == nil {
		t.Fatal("both database modes should be rejected")
	}
}

func TestQueueVisibilityTimeoutFromEnv(t *testing.T) {
	t.Setenv("HAKO_GITHUB_CHECKS_VISIBILITY_TIMEOUT_SECONDS", "")
	if timeout, err := queueVisibilityTimeoutFromEnv(); err != nil || timeout != 15*time.Minute {
		t.Fatalf("default queue visibility = %s err=%v", timeout, err)
	}
	t.Setenv("HAKO_GITHUB_CHECKS_VISIBILITY_TIMEOUT_SECONDS", "900")
	if timeout, err := queueVisibilityTimeoutFromEnv(); err != nil || timeout != 15*time.Minute {
		t.Fatalf("configured queue visibility = %s err=%v", timeout, err)
	}
	for _, invalid := range []string{"0", "43201", "not-a-number"} {
		t.Setenv("HAKO_GITHUB_CHECKS_VISIBILITY_TIMEOUT_SECONDS", invalid)
		if _, err := queueVisibilityTimeoutFromEnv(); err == nil {
			t.Fatalf("invalid queue visibility %q was accepted", invalid)
		}
	}
}

func TestClientTLSRequiresCompleteConfiguration(t *testing.T) {
	t.Setenv("HAKO_GITHUB_CHECKS_BROKER_CLIENT_CERT_FILE", "")
	t.Setenv("HAKO_GITHUB_CHECKS_BROKER_CLIENT_KEY_FILE", "")
	t.Setenv("HAKO_GITHUB_CHECKS_BROKER_CA_FILE", "")
	if _, _, err := clientTLSFromEnv(); err == nil {
		t.Fatal("missing broker certificate configuration should fail")
	}
}

func TestClientTLSLoadsInjectedSecretsAndClearsEnvironment(t *testing.T) {
	certPEM, keyPEM := testClientCertificatePEM(t)
	for name, value := range map[string]string{
		"HAKO_GITHUB_CHECKS_BROKER_CLIENT_CERT_PEM": certPEM,
		"HAKO_GITHUB_CHECKS_BROKER_CLIENT_KEY_PEM":  keyPEM,
		"HAKO_GITHUB_CHECKS_BROKER_CA_PEM":          certPEM,
	} {
		t.Setenv(name, value)
	}
	certificate, roots, err := clientTLSFromEnv()
	if err != nil || len(certificate.Certificate) != 1 || certificate.Leaf == nil || roots == nil {
		t.Fatalf("injected broker mTLS configuration rejected: certificate=%+v roots=%v error=%v", certificate, roots, err)
	}
	for _, name := range []string{
		"HAKO_GITHUB_CHECKS_BROKER_CLIENT_CERT_PEM",
		"HAKO_GITHUB_CHECKS_BROKER_CLIENT_KEY_PEM",
		"HAKO_GITHUB_CHECKS_BROKER_CA_PEM",
	} {
		if os.Getenv(name) != "" {
			t.Errorf("injected Secret %s remains in the process environment", name)
		}
	}
}

func TestClientTLSRejectsPartialOrMixedInjectedConfiguration(t *testing.T) {
	t.Setenv("HAKO_GITHUB_CHECKS_BROKER_CLIENT_CERT_FILE", "/unused/cert.pem")
	t.Setenv("HAKO_GITHUB_CHECKS_BROKER_CLIENT_KEY_FILE", "")
	t.Setenv("HAKO_GITHUB_CHECKS_BROKER_CA_FILE", "")
	t.Setenv("HAKO_GITHUB_CHECKS_BROKER_CLIENT_CERT_PEM", "certificate")
	t.Setenv("HAKO_GITHUB_CHECKS_BROKER_CLIENT_KEY_PEM", "key")
	t.Setenv("HAKO_GITHUB_CHECKS_BROKER_CA_PEM", "ca")
	if _, _, err := clientTLSFromEnv(); err == nil {
		t.Fatal("mixed file and injected broker mTLS configuration should be rejected")
	}
	t.Setenv("HAKO_GITHUB_CHECKS_BROKER_CLIENT_CERT_FILE", "")
	t.Setenv("HAKO_GITHUB_CHECKS_BROKER_CLIENT_CERT_PEM", "")
	if _, _, err := clientTLSFromEnv(); err == nil {
		t.Fatal("partial injected broker mTLS configuration should be rejected")
	}
}

func testClientCertificatePEM(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "hako-checks-test"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}
