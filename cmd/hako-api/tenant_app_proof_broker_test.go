package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTenantAppProofBrokerConfigurationIsOptionalButAllOrNothing(t *testing.T) {
	names := []string{"HAKO_TENANT_GITHUB_APP_PROOF_BROKER_URL", "HAKO_TENANT_GITHUB_APP_PROOF_CLIENT_CERT_FILE",
		"HAKO_TENANT_GITHUB_APP_PROOF_CLIENT_KEY_FILE", "HAKO_TENANT_GITHUB_APP_PROOF_BROKER_CA_FILE"}
	for _, name := range names {
		t.Setenv(name, "")
	}
	verifier, err := tenantAppProofBrokerFromEnv()
	if err != nil || verifier != nil {
		t.Fatalf("unconfigured proof Broker verifier=%v error=%v", verifier, err)
	}
	t.Setenv(names[0], "https://broker.internal")
	if _, err := tenantAppProofBrokerFromEnv(); err == nil {
		t.Fatal("partial proof Broker configuration was accepted")
	}
}

func TestTenantAppProofBrokerBuildsOnlyControlPlaneMTLSClient(t *testing.T) {
	certPEM, keyPEM := syntheticBrokerClientCertificate(t)
	directory := t.TempDir()
	certPath, keyPath, caPath := filepath.Join(directory, "client.pem"), filepath.Join(directory, "client-key.pem"), filepath.Join(directory, "ca.pem")
	for path, data := range map[string][]byte{certPath: certPEM, keyPath: keyPEM, caPath: certPEM} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HAKO_TENANT_GITHUB_APP_PROOF_BROKER_URL", "https://broker.internal")
	t.Setenv("HAKO_TENANT_GITHUB_APP_PROOF_CLIENT_CERT_FILE", certPath)
	t.Setenv("HAKO_TENANT_GITHUB_APP_PROOF_CLIENT_KEY_FILE", keyPath)
	t.Setenv("HAKO_TENANT_GITHUB_APP_PROOF_BROKER_CA_FILE", caPath)
	verifier, err := tenantAppProofBrokerFromEnv()
	if err != nil || verifier == nil {
		t.Fatalf("proof Broker verifier=%v error=%v", verifier, err)
	}
}
