package main

import "testing"

func TestParseSoftwareCatalogCLI(t *testing.T) {
	got, err := parseSoftwareCatalogCLI([]string{"set", "--tenant", " tenant_a ", "--service", "payments-api", "--file", "service.json"})
	if err != nil || got.tenantID != "tenant_a" || got.service.ServiceID != "payments-api" || got.file != "service.json" {
		t.Fatalf("parse catalog set = %+v, error=%v", got, err)
	}
	if got, err := parseSoftwareCatalogCLI([]string{"list", "--tenant", "tenant_a"}); err != nil || got.action != "list" {
		t.Fatalf("parse catalog list = %+v, error=%v", got, err)
	}
	for _, args := range [][]string{
		{}, {"delete", "--tenant", "tenant_a", "--service", "api"}, {"list"},
		{"list", "--tenant", "tenant_a", "--service", "api"}, {"set", "--tenant", "tenant_a", "--service", "api"},
		{"set", "--tenant", "tenant_a", "--service", "api", "--file", "s.json", "unexpected"},
	} {
		if _, err := parseSoftwareCatalogCLI(args); err == nil {
			t.Errorf("accepted invalid catalog CLI args %q", args)
		}
	}
}

func TestParseSoftwareCatalogDeployPolicyCLI(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"show", "--tenant", "tenant_a", "--service", "api"}, "show"},
		{[]string{"set", "--tenant", "tenant_a", "--service", "api", "--file", "policy.json"}, "set"},
		{[]string{"verify", "--tenant", "tenant_a", "--service", "api", "--token-file", "oidc.jwt"}, "verify"},
	} {
		got, err := parseSoftwareCatalogDeployPolicyCLI(tc.args)
		if err != nil || got.action != tc.want {
			t.Errorf("parse deploy policy args %q = %+v, %v", tc.args, got, err)
		}
	}
	for _, args := range [][]string{
		{}, {"disable", "--tenant", "tenant_a", "--service", "api"},
		{"show", "--tenant", "tenant_a", "--service", "api", "--token-file", "token"},
		{"set", "--tenant", "tenant_a", "--service", "api"},
		{"verify", "--tenant", "tenant_a", "--service", "api"},
		{"verify", "--tenant", "tenant_a", "--service", "api", "--file", "policy.json", "--token-file", "token"},
	} {
		if _, err := parseSoftwareCatalogDeployPolicyCLI(args); err == nil {
			t.Errorf("accepted invalid deploy policy args %q", args)
		}
	}
}
