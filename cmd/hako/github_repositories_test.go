package main

import "testing"

func TestParseGitHubRepositoryFactoryCLI(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "enable", args: []string{"enable", "--tenant", "tenant_one", "--installation", "123", "--repository", "456"}, want: true},
		{name: "disable", args: []string{"disable", "--tenant", "tenant_one", "--installation", "123", "--repository", "456"}, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input, err := parseGitHubRepositoryFactoryCLI(test.args)
			if err != nil || input.enabled != test.want || input.tenantID != "tenant_one" || input.installationID != 123 || input.repositoryID != 456 {
				t.Fatalf("parseGitHubRepositoryFactoryCLI() = %+v, error = %v", input, err)
			}
		})
	}
}

func TestParseGitHubRepositoryFactoryCLIRejectsUnsafeOrIncompleteInput(t *testing.T) {
	for _, args := range [][]string{
		{}, {"start", "--tenant", "tenant_one", "--installation", "123", "--repository", "456"},
		{"enable", "--installation", "123", "--repository", "456"},
		{"enable", "--tenant", "tenant_one", "--installation", "0", "--repository", "456"},
		{"disable", "--tenant", "tenant_one", "--installation", "123", "--repository", "not-a-number"},
		{"enable", "--tenant", "tenant_one", "--installation", "123", "--repository", "456", "extra"},
	} {
		if _, err := parseGitHubRepositoryFactoryCLI(args); err == nil {
			t.Fatalf("parseGitHubRepositoryFactoryCLI(%q) unexpectedly succeeded", args)
		}
	}
}

func TestParseGitHubRepositoryActionsRunnerCLI(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "enable", args: []string{"enable", "--tenant", "tenant_one", "--installation", "123", "--repository", "456"}, want: true},
		{name: "disable", args: []string{"disable", "--tenant", "tenant_one", "--installation", "123", "--repository", "456"}, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input, err := parseGitHubRepositoryActionsRunnerCLI(test.args)
			if err != nil || input.enabled != test.want || input.tenantID != "tenant_one" || input.installationID != 123 || input.repositoryID != 456 {
				t.Fatalf("parseGitHubRepositoryActionsRunnerCLI() = %+v, error = %v", input, err)
			}
		})
	}
}

func TestParseGitHubRepositoryActionsRunnerCLIRejectsInvalidInput(t *testing.T) {
	for _, args := range [][]string{
		{}, {"run", "--tenant", "tenant_one", "--installation", "123", "--repository", "456"},
		{"enable", "--installation", "123", "--repository", "456"},
		{"enable", "--tenant", "tenant_one", "--installation", "0", "--repository", "456"},
		{"disable", "--tenant", "tenant_one", "--installation", "123", "--repository", "bad"},
		{"enable", "--tenant", "tenant_one", "--installation", "123", "--repository", "456", "extra"},
	} {
		if _, err := parseGitHubRepositoryActionsRunnerCLI(args); err == nil {
			t.Fatalf("parseGitHubRepositoryActionsRunnerCLI(%q) unexpectedly succeeded", args)
		}
	}
}
