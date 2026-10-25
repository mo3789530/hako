package main

import "testing"

func TestTenantAgentProposalBrokerConfigurationIsOptionalButAllOrNothing(t *testing.T) {
	for _, name := range []string{"HAKO_TENANT_GITHUB_OAUTH_BROKER_URL", "HAKO_TENANT_GITHUB_OAUTH_CLIENT_CERT_FILE",
		"HAKO_TENANT_GITHUB_OAUTH_CLIENT_KEY_FILE", "HAKO_TENANT_GITHUB_OAUTH_BROKER_CA_FILE"} {
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
