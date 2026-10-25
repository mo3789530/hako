package main

import (
	"io"
	"strings"
	"testing"
)

func TestConfirmWorkspaceDeletionRequiresExactWorkspaceID(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		confirmed bool
		wantError bool
	}{
		{name: "exact workspace ID", input: "ws_123\n", confirmed: true},
		{name: "different workspace ID", input: "ws_124\n"},
		{name: "generic yes is insufficient", input: "yes\n"},
		{name: "EOF cancels", input: ""},
		{name: "oversized input is rejected", input: strings.Repeat("x", 257) + "\n", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output strings.Builder
			confirmed, err := confirmWorkspaceDeletion(strings.NewReader(test.input), &output, "tenant_a", "ws_123")
			if (err != nil) != test.wantError || confirmed != test.confirmed {
				t.Fatalf("confirmWorkspaceDeletion() = %t, %v; want %t, error=%t", confirmed, err, test.confirmed, test.wantError)
			}
			if !strings.Contains(output.String(), "Tenant tenant_a") || !strings.Contains(output.String(), `Type "ws_123"`) {
				t.Fatalf("confirmation prompt omits the exact deletion target: %q", output.String())
			}
		})
	}
}

func TestConfirmWorkspaceDeletionRejectsMissingIOOrIdentity(t *testing.T) {
	tests := []struct {
		name      string
		input     io.Reader
		output    io.Writer
		tenantID  string
		workspace string
	}{
		{name: "missing input", output: &strings.Builder{}, tenantID: "tenant_a", workspace: "ws_123"},
		{name: "missing output", input: strings.NewReader("ws_123\n"), tenantID: "tenant_a", workspace: "ws_123"},
		{name: "missing tenant", input: strings.NewReader("ws_123\n"), output: &strings.Builder{}, workspace: "ws_123"},
		{name: "missing workspace", input: strings.NewReader("ws_123\n"), output: &strings.Builder{}, tenantID: "tenant_a"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := confirmWorkspaceDeletion(test.input, test.output, test.tenantID, test.workspace); err == nil {
				t.Fatal("invalid confirmation input accepted")
			}
		})
	}
}

func TestAuthorizeWorkspaceDeletionYesSkipsPrompt(t *testing.T) {
	confirmed, err := authorizeWorkspaceDeletion(true, nil, nil, "tenant_a", "ws_123")
	if err != nil || !confirmed {
		t.Fatalf("--yes authorization = %t, %v; want confirmed without prompt", confirmed, err)
	}
}

func TestAuthorizeWorkspaceServiceRemovalRequiresExactInstanceName(t *testing.T) {
	for _, test := range []struct {
		name      string
		input     string
		confirmed bool
	}{
		{name: "exact instance name", input: "postgres\n", confirmed: true},
		{name: "generic yes is insufficient", input: "yes\n"},
		{name: "wrong instance name", input: "redis\n"},
		{name: "EOF cancels", input: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output strings.Builder
			confirmed, err := authorizeWorkspaceServiceRemoval(false, strings.NewReader(test.input), &output,
				"tenant_a", "ws_123", "postgres")
			if err != nil || confirmed != test.confirmed {
				t.Fatalf("removal authorization = %t, %v; want %t", confirmed, err, test.confirmed)
			}
			if !strings.Contains(output.String(), "deletes its data") || !strings.Contains(output.String(), `Type "postgres"`) {
				t.Fatalf("service removal prompt does not state the data impact and target: %q", output.String())
			}
		})
	}
}

func TestAuthorizeWorkspaceServiceRemovalYesSkipsPrompt(t *testing.T) {
	confirmed, err := authorizeWorkspaceServiceRemoval(true, nil, nil, "tenant_a", "ws_123", "postgres")
	if err != nil || !confirmed {
		t.Fatalf("--yes authorization = %t, %v; want confirmed without prompt", confirmed, err)
	}
}
