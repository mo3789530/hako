package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"reflect"
	"testing"
	"time"
)

func TestServerTLSConfigFromPEMEnvironment(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	certificateDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "broker.test"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), DNSNames: []string{"broker.test"},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "broker.test"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), DNSNames: []string{"broker.test"},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	t.Setenv("HAKO_GITHUB_BROKER_TLS_CERT_PEM", string(certPEM))
	t.Setenv("HAKO_GITHUB_BROKER_TLS_KEY_PEM", string(keyPEM))
	t.Setenv("HAKO_GITHUB_BROKER_CLIENT_CA_PEM", string(certPEM))
	t.Setenv("HAKO_GITHUB_BROKER_TLS_CERT_FILE", "")
	t.Setenv("HAKO_GITHUB_BROKER_TLS_KEY_FILE", "")
	t.Setenv("HAKO_GITHUB_BROKER_CLIENT_CA_FILE", "")

	got, err := serverTLSConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got.MinVersion == 0 || len(got.Certificates) != 1 || got.ClientCAs == nil {
		t.Fatalf("incomplete broker TLS configuration: %#v", got)
	}
}

func TestServerTLSConfigRejectsPartialAndMixedMaterial(t *testing.T) {
	t.Setenv("HAKO_GITHUB_BROKER_TLS_CERT_PEM", "certificate")
	t.Setenv("HAKO_GITHUB_BROKER_TLS_KEY_PEM", "")
	t.Setenv("HAKO_GITHUB_BROKER_CLIENT_CA_PEM", "")
	t.Setenv("HAKO_GITHUB_BROKER_TLS_CERT_FILE", "")
	t.Setenv("HAKO_GITHUB_BROKER_TLS_KEY_FILE", "")
	t.Setenv("HAKO_GITHUB_BROKER_CLIENT_CA_FILE", "")
	if _, err := serverTLSConfigFromEnv(); err == nil {
		t.Fatal("partial PEM configuration was accepted")
	}

	t.Setenv("HAKO_GITHUB_BROKER_TLS_CERT_FILE", "/unused/cert.pem")
	if _, err := serverTLSConfigFromEnv(); err == nil {
		t.Fatal("mixed PEM and file configuration was accepted")
	}
}

func TestConfiguredCheckoutCallers(t *testing.T) {
	got, err := configuredCheckoutCallers(" rp-tokyo-01, rp-us-01 ")
	want := []string{"spiffe://hako/resource-plane/rp-tokyo-01", "spiffe://hako/resource-plane/rp-us-01"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("configuredCheckoutCallers = %#v, %v", got, err)
	}
	if got, err := configuredCheckoutCallers("  "); err != nil || len(got) != 0 {
		t.Fatalf("empty allowlist = %#v, %v", got, err)
	}
	for _, invalid := range []string{"rp-a,rp-a", "rp-a,", "rp-a,../control-plane", "spiffe://hako/control-plane"} {
		if _, err := configuredCheckoutCallers(invalid); err == nil {
			t.Fatalf("invalid caller list %q was accepted", invalid)
		}
	}
}

func TestConfiguredRunnerCleanupCallers(t *testing.T) {
	got, err := configuredRunnerCleanupCallers(" rp-tokyo-01, rp-us-01 ")
	want := []string{"spiffe://hako/resource-plane/rp-tokyo-01", "spiffe://hako/resource-plane/rp-us-01"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("configuredRunnerCleanupCallers = %#v, %v", got, err)
	}
	if got, err := configuredRunnerCleanupCallers(" "); err != nil || len(got) != 0 {
		t.Fatalf("empty allowlist = %#v, %v", got, err)
	}
	for _, invalid := range []string{"rp-a,rp-a", "rp-a,", "rp-a,../control-plane", "spiffe://hako/control-plane"} {
		if _, err := configuredRunnerCleanupCallers(invalid); err == nil {
			t.Fatalf("invalid cleanup caller list %q was accepted", invalid)
		}
	}
}

func TestConfiguredAgentTaskCallers(t *testing.T) {
	got, err := configuredResourcePlaneCallers("rp-tokyo-01", "HAKO_GITHUB_BROKER_AGENT_TASK_RESOURCE_PLANE_IDS")
	want := []string{"spiffe://hako/resource-plane/rp-tokyo-01"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("configured Agent task callers = %#v, %v", got, err)
	}
	if got, err := configuredResourcePlaneCallers(" ", "HAKO_GITHUB_BROKER_AGENT_TASK_RESOURCE_PLANE_IDS"); err != nil || len(got) != 0 {
		t.Fatalf("empty allowlist = %#v, %v", got, err)
	}
	for _, invalid := range []string{"rp-tokyo-01,rp-tokyo-01", "rp-a,", "../rp", "spiffe://hako/control-plane"} {
		if _, err := configuredResourcePlaneCallers(invalid, "HAKO_GITHUB_BROKER_AGENT_TASK_RESOURCE_PLANE_IDS"); err == nil {
			t.Fatalf("invalid Agent task caller list %q was accepted", invalid)
		}
	}
}
