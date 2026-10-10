package githubregistry

import (
	"strings"
	"testing"
)

func TestNormalizeAppDraftRejectsSecretsAndUnsafeMetadata(t *testing.T) {
	valid := AppDraftRequest{GitHubAppID: 123, Slug: " Hako-App ", ClientID: "Iv1.public-client", CredentialBindingID: "binding_one"}
	normalized, err := NormalizeAppDraft(valid)
	if err != nil || normalized.Slug != "hako-app" || normalized.ClientID != "Iv1.public-client" {
		t.Fatal("valid App metadata normalization failed")
	}
	for _, invalid := range []AppDraftRequest{
		{}, {GitHubAppID: -1, Slug: "hako", ClientID: "client", CredentialBindingID: "binding"},
		{GitHubAppID: 123, Slug: "hako/path", ClientID: "client", CredentialBindingID: "binding"},
		{GitHubAppID: 123, Slug: "-hako", ClientID: "client", CredentialBindingID: "binding"},
		{GitHubAppID: 123, Slug: "hako", ClientID: "client\n", CredentialBindingID: ""},
		{GitHubAppID: 123, Slug: "hako", ClientID: "client", CredentialBindingID: "arn:aws:secretsmanager:region:account:secret:other"},
		{GitHubAppID: 123, Slug: "hako", ClientID: strings.Repeat("x", 129), CredentialBindingID: "binding"},
	} {
		if _, err := NormalizeAppDraft(invalid); err == nil {
			t.Fatal("invalid or arbitrary Secret metadata accepted")
		}
	}
}
