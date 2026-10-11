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
	input, err = parseGitHubAppCLI([]string{"deliveries", "--tenant", "tenant_one", "--integration", "gint_one", "--limit", "25", "--offset", "50"})
	if err != nil || input.Action != "deliveries" || input.Limit != 25 || input.Offset != 50 {
		t.Fatal("failed webhook delivery CLI parsing failed")
	}
	if _, err := parseGitHubAppCLI([]string{"deliveries", "--tenant", "tenant_one"}); err == nil {
		t.Fatal("failed webhook delivery CLI without integration was accepted")
	}
	input, err = parseGitHubAppCLI([]string{"migration-plan", "--tenant", "tenant_one", "--integration", "gint_one", "--revision", "4", "--generation", "8", "--installations", "12,13"})
	if err != nil || len(input.InstallationIDs) != 2 || input.InstallationIDs[1] != 13 {
		t.Fatal("explicit Tenant migration dry-run parsing failed")
	}
	if _, err := parseGitHubAppCLI([]string{"migration-backfill", "--tenant", "tenant_one", "--integration", "gint_one", "--revision", "4", "--generation", "8", "--installations", "12"}); err == nil {
		t.Fatal("migration backfill without --apply was accepted")
	}
	input, err = parseGitHubAppCLI([]string{"migration-backfill", "--tenant", "tenant_one", "--integration", "gint_one", "--revision", "4", "--generation", "8", "--installations", "12", "--apply"})
	if err != nil || !input.Confirm || input.Action != "migration-backfill" {
		t.Fatal("explicitly confirmed migration backfill parsing failed")
	}
	input, err = parseGitHubAppCLI([]string{"select-default", "--tenant", "tenant_one", "--integration", "gint_one", "--revision", "4", "--generation", "8"})
	if err != nil || input.ExpectedGeneration != 8 {
		t.Fatal("revision and generation-bound default selection parsing failed")
	}
}
