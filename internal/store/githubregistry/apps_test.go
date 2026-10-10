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

func TestPinnedCredentialMetadataRequiresExactAWSReferences(t *testing.T) {
	for _, roleARN := range []string{
		"arn:aws:iam::123456789012:role/tenant-key-reader",
		"arn:aws:iam::123456789012:role/hako/tenant-key-reader",
	} {
		if !validExactIAMRoleARN(roleARN) {
			t.Fatalf("valid exact reader role ARN rejected: %s", roleARN)
		}
	}
	for _, roleARN := range []string{
		"",
		"arn:aws:iam::123456789012:role/*",
		"arn:aws:iam:ap-northeast-1:123456789012:role/reader",
		"arn:aws:iam::123456789012:policy/reader",
		"arn:aws:iam::12345678901:role/reader",
	} {
		if validExactIAMRoleARN(roleARN) {
			t.Fatalf("non-exact reader role ARN accepted: %q", roleARN)
		}
	}
	if !validSecretVersionID("0123456789abcdef0123456789abcdef") {
		t.Fatal("32-character Secrets Manager version ID rejected")
	}
	for _, versionID := range []string{"", "AWSCURRENT", "0123456789abcdef", "0123456789abcdef0123456789abcdeg", "0123456789abcdef0123456789abcdef\n"} {
		if validSecretVersionID(versionID) {
			t.Fatalf("unpinned or malformed version ID accepted: %q", versionID)
		}
	}
}
