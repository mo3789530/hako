package main

import "testing"

func TestParseModelCredentialCLI(t *testing.T) {
	version := "0123456789abcdef0123456789abcdef"
	for _, args := range [][]string{
		{"list", "--tenant", "tenant_a"},
		{"add", "--tenant", "tenant_a", "--binding", "binding_a", "--version-id", version},
		{"add", "--tenant", "tenant_a", "--purpose", "native_agent", "--binding", "binding_agent", "--version-id", version},
		{"rotate", "--tenant", "tenant_a", "--version-id", version, "--generation", "2"},
		{"rotate", "--tenant", "tenant_a", "--purpose", "native_agent", "--version-id", version, "--generation", "2"},
		{"disable", "--tenant", "tenant_a", "--generation", "2"},
		{"disable", "--tenant", "tenant_a", "--purpose", "native_agent", "--generation", "2"},
	} {
		if _, err := parseModelCredentialCLI(args); err != nil {
			t.Errorf("parseModelCredentialCLI(%v) error = %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"add", "--tenant", "tenant_a", "--binding", "binding_a", "--version-id", "plain-secret-value"},
		{"rotate", "--tenant", "tenant_a", "--version-id", version},
		{"disable", "--tenant", "tenant_a"},
		{"add", "--tenant", "tenant_a", "--binding", "binding_a", "--version-id", version, "--secret", "sk-ant-plaintext"},
		{"add", "--tenant", "tenant_a", "--purpose", "workspace", "--binding", "binding_a", "--version-id", version},
	} {
		if _, err := parseModelCredentialCLI(args); err == nil {
			t.Errorf("parseModelCredentialCLI(%v) accepted invalid/secret-bearing arguments", args)
		}
	}
}
