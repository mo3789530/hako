package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServerTLSConfigRequiresAllFilesAndVerifiedClientCertificates(t *testing.T) {
	t.Setenv("HAKO_GITHUB_BROKER_TLS_CERT_FILE", "")
	t.Setenv("HAKO_GITHUB_BROKER_TLS_KEY_FILE", "")
	t.Setenv("HAKO_GITHUB_BROKER_CLIENT_CA_FILE", "")
	if _, err := serverTLSConfigFromEnv(); err == nil {
		t.Fatal("missing TLS configuration was accepted")
	}

	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.pem")
	keyPath := filepath.Join(dir, "server-key.pem")
	caPath := filepath.Join(dir, "client-ca.pem")
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(11), Subject: pkix.Name{CommonName: "hako broker"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAKO_GITHUB_BROKER_TLS_CERT_FILE", certPath)
	t.Setenv("HAKO_GITHUB_BROKER_TLS_KEY_FILE", keyPath)
	t.Setenv("HAKO_GITHUB_BROKER_CLIENT_CA_FILE", caPath)
	cfg, err := serverTLSConfigFromEnv()
	if err != nil || cfg.MinVersion != tls.VersionTLS13 || cfg.ClientAuth != tls.RequireAndVerifyClientCert || len(cfg.Certificates) != 1 {
		t.Fatalf("invalid broker mTLS config: cfg=%+v err=%v", cfg, err)
	}
}
