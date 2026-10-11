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

func TestTenantWebhookRateLimitFromEnv(t *testing.T) {
	t.Setenv("HAKO_TENANT_GITHUB_WEBHOOK_RATE_LIMIT", "")
	if got, err := tenantWebhookRateLimitFromEnv(); err != nil || got != 120 {
		t.Fatalf("default rate=%d err=%v", got, err)
	}
	for _, value := range []string{"0", "-1", "100001", "not-a-number"} {
		t.Setenv("HAKO_TENANT_GITHUB_WEBHOOK_RATE_LIMIT", value)
		if _, err := tenantWebhookRateLimitFromEnv(); err == nil {
			t.Errorf("accepted invalid rate %q", value)
		}
	}
	t.Setenv("HAKO_TENANT_GITHUB_WEBHOOK_RATE_LIMIT", "250")
	if got, err := tenantWebhookRateLimitFromEnv(); err != nil || got != 250 {
		t.Fatalf("configured rate=%d err=%v", got, err)
	}
}
