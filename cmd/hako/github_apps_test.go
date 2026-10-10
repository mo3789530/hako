package main

import "testing"

func TestParseGitHubAppCLIAllowsReferencesNotSecretInputs(t *testing.T) {
	input, err := parseGitHubAppCLI([]string{"add", "--tenant", "tenant_one", "--app-id", "123", "--slug", "app", "--client-id", "Iv1.client", "--credential-binding", "binding"})
	if err != nil || input.IdempotencyKey == "" || input.Draft.CredentialBindingID != "binding" {
		t.Fatal("App CLI parsing failed")
	}
	for _, args := range [][]string{{}, {"list"}, {"show", "--tenant", "tenant_one"}, {"disable", "--tenant", "tenant_one", "--integration", "gint_one"}, {"add", "--tenant", "tenant_one", "--private-key", "raw-key"}, {"unknown", "--tenant", "tenant_one"}} {
		if _, err := parseGitHubAppCLI(args); err == nil {
			t.Fatal("invalid action or Secret flag accepted")
		}
	}
	input, err = parseGitHubAppCLI([]string{"disable", "--tenant", "tenant_one", "--integration", "gint_one", "--revision", "1"})
	if err != nil || input.ExpectedRevision != 1 {
		t.Fatal("revision-bound disable parsing failed")
	}
	input, err = parseGitHubAppCLI([]string{"setup", "--tenant", "tenant_one", "--integration", "gint_one"})
	if err != nil || input.Action != "setup" || input.IntegrationID != "gint_one" {
		t.Fatal("Tenant GitHub App setup parsing failed")
	}
}
