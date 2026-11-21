package githubapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

func TestTenantWebhookSecretReaderUsesPurposeRoleAndPinnedVersions(t *testing.T) {
	const roleARN = "arn:aws:iam::123456789012:role/hako/tenant-webhook-reader"
	const secretARN = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/webhook"
	const currentVersion = "11111111111111111111111111111111"
	const previousVersion = "22222222222222222222222222222222"
	access := TenantWebhookSecretAccess{TenantID: "tenant-a", IntegrationID: "integration-a", AppID: 901, CredentialGeneration: 4,
		ReaderRoleARN: roleARN, SecretARN: secretARN, VersionID: currentVersion, PreviousVersionID: previousVersion, PreviousValidUntil: time.Now().Add(time.Minute)}
	var assumeCalls, getCalls int
	assumer := assumeRoleFunc(func(_ context.Context, input *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
		assumeCalls++
		if aws.ToString(input.RoleArn) != roleARN || aws.ToInt32(input.DurationSeconds) != 900 || !strings.HasPrefix(aws.ToString(input.RoleSessionName), "hako-webhook-") {
			t.Fatal("STS did not use the exact Webhook-purpose role and bounded session")
		}
		var policy struct {
			Statement []struct {
				Action   []string `json:"Action"`
				Resource []string `json:"Resource"`
			} `json:"Statement"`
		}
		if err := json.Unmarshal([]byte(aws.ToString(input.Policy)), &policy); err != nil || len(policy.Statement) != 1 || len(policy.Statement[0].Action) != 1 || policy.Statement[0].Action[0] != "secretsmanager:GetSecretValue" || len(policy.Statement[0].Resource) != 1 || policy.Statement[0].Resource[0] != secretARN {
			t.Fatal("STS session policy was broader than exact Webhook Secret read")
		}
		return assumedRoleResult(time.Now().Add(10 * time.Minute)), nil
	})
	clients := func(region string, credentials aws.Credentials) SecretValueAPI {
		if region != "ap-northeast-1" || credentials.AccessKeyID != "ASIAEXAMPLE" {
			t.Fatal("assumed credentials were not isolated to the Secret region")
		}
		return secretAPIFunc(func(_ context.Context, input *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
			getCalls++
			if aws.ToString(input.SecretId) != secretARN || input.VersionId == nil || input.VersionStage != nil {
				t.Fatal("Secret request was not pinned by exact ARN and VersionId")
			}
			value := "current-webhook-secret"
			if aws.ToString(input.VersionId) == previousVersion {
				value = "previous-webhook-secret"
			} else if aws.ToString(input.VersionId) != currentVersion {
				t.Fatal("reader requested an unexpected secret version")
			}
			return &secretsmanager.GetSecretValueOutput{SecretString: &value, VersionId: input.VersionId}, nil
		})
	}
	reader, err := NewTenantWebhookSecretReader(access, assumer, clients)
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := reader.Read(context.Background())
	if err != nil || string(secrets.Current) != "current-webhook-secret" || string(secrets.Previous) != "previous-webhook-secret" || assumeCalls != 2 || getCalls != 2 {
		t.Fatalf("read current/previous secrets: calls=%d/%d result=%s err=%v", assumeCalls, getCalls, secrets, err)
	}
}

func TestTenantWebhookSecretReaderDoesNotReadExpiredPreviousVersion(t *testing.T) {
	access := TenantWebhookSecretAccess{TenantID: "tenant-a", IntegrationID: "integration-a", AppID: 901, CredentialGeneration: 4,
		ReaderRoleARN: "arn:aws:iam::123456789012:role/hako/tenant-webhook-reader", SecretARN: "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/webhook",
		VersionID: "11111111111111111111111111111111", PreviousVersionID: "22222222222222222222222222222222", PreviousValidUntil: time.Now().Add(-time.Second)}
	var requestedVersions []string
	reader, err := NewTenantWebhookSecretReader(access,
		assumeRoleFunc(func(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
			return assumedRoleResult(time.Now().Add(time.Minute)), nil
		}),
		func(string, aws.Credentials) SecretValueAPI {
			return secretAPIFunc(func(_ context.Context, input *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
				requestedVersions = append(requestedVersions, aws.ToString(input.VersionId))
				value := "current"
				return &secretsmanager.GetSecretValueOutput{SecretString: &value, VersionId: input.VersionId}, nil
			})
		})
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := reader.Read(context.Background())
	if err != nil || string(secrets.Previous) != "" || len(requestedVersions) != 1 || requestedVersions[0] != access.VersionID {
		t.Fatalf("expired prior secret was read: versions=%v err=%v", requestedVersions, err)
	}
}

func TestTenantWebhookSecretReaderRejectsIncompleteOrInvalidPins(t *testing.T) {
	valid := TenantWebhookSecretAccess{TenantID: "tenant-a", IntegrationID: "integration-a", AppID: 901, CredentialGeneration: 4,
		ReaderRoleARN: "arn:aws:iam::123456789012:role/hako/tenant-webhook-reader", SecretARN: "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/webhook", VersionID: "11111111111111111111111111111111"}
	for _, invalid := range []TenantWebhookSecretAccess{{}, func() TenantWebhookSecretAccess {
		v := valid
		v.ReaderRoleARN = "arn:aws:iam::123456789012:role/*"
		return v
	}(), func() TenantWebhookSecretAccess { v := valid; v.VersionID = "AWSCURRENT"; return v }(), func() TenantWebhookSecretAccess {
		v := valid
		v.PreviousVersionID = "22222222222222222222222222222222"
		return v
	}()} {
		if _, err := NewTenantWebhookSecretReader(invalid, assumeRoleFunc(func(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
			t.Fatal("invalid pin reached STS")
			return nil, nil
		}), func(string, aws.Credentials) SecretValueAPI {
			t.Fatal("invalid pin reached Secrets Manager")
			return nil
		}); err == nil {
			t.Fatal("invalid Webhook secret access pin was accepted")
		}
	}
}
