package main

import "testing"

func TestTenantOAuthBrokerConfigurationIsAllOrNothing(t *testing.T) {
	for _, name := range []string{"HAKO_TENANT_GITHUB_OAUTH_BROKER_URL", "HAKO_TENANT_GITHUB_OAUTH_CLIENT_CERT_FILE", "HAKO_TENANT_GITHUB_OAUTH_CLIENT_KEY_FILE", "HAKO_TENANT_GITHUB_OAUTH_BROKER_CA_FILE"} {
		t.Setenv(name, "")
	}
	resolver, err := tenantOAuthBrokerFromEnv()
	if err != nil || resolver != nil {
		t.Fatal("unset broker config should remain disabled")
	}
	t.Setenv("HAKO_TENANT_GITHUB_OAUTH_BROKER_URL", "https://broker.internal")
	if _, err := tenantOAuthBrokerFromEnv(); err == nil {
		t.Fatal("partial broker configuration accepted")
	}
}
