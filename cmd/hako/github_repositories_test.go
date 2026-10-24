package main

import "testing"

func TestParseGitHubRepositoryAgentWritePolicyCLI(t *testing.T) {
	got, err := parseGitHubRepositoryAgentWritePolicyCLI([]string{"set", "--tenant", " tenant_a ", "--installation", "4", "--repository", "8", "--policy-file", "policy.json"})
	if err != nil || got.tenantID != "tenant_a" || got.installationID != 4 || got.repositoryID != 8 || got.policyFile != "policy.json" {
		t.Fatalf("parse Agent write policy CLI = %+v error=%v", got, err)
	}
	for _, args := range [][]string{
		{}, {"enable", "--tenant", "tenant_a", "--installation", "4", "--repository", "8", "--policy-file", "p.json"},
		{"set", "--tenant", "tenant_a", "--installation", "0", "--repository", "8", "--policy-file", "p.json"},
		{"set", "--tenant", "tenant_a", "--installation", "4", "--repository", "8"},
	} {
		if _, err := parseGitHubRepositoryAgentWritePolicyCLI(args); err == nil {
			t.Errorf("accepted invalid Agent write policy CLI args %q", args)
		}
	}
}
