package modelcredentialbroker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/mo3789530/hako/internal/store/modelcredentials"
)

type testAssumer func(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)

func (f testAssumer) AssumeRole(ctx context.Context, input *sts.AssumeRoleInput, options ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	return f(ctx, input, options...)
}

type testSecretClient func(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)

func (f testSecretClient) GetSecretValue(ctx context.Context, input *secretsmanager.GetSecretValueInput, options ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	return f(ctx, input, options...)
}

func TestAWSSecretReaderUsesExactRoleResourceAndVersion(t *testing.T) {
	version := "0123456789abcdef0123456789abcdef"
	const roleARN = "arn:aws:iam::123456789012:role/hako/tenant-model-credential-reader/tenant-a"
	const secretARN = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/model/a-AbCdEf"
	assumer := testAssumer(func(_ context.Context, input *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
		if aws.ToString(input.RoleArn) != roleARN || aws.ToInt32(input.DurationSeconds) != 900 || len(aws.ToString(input.RoleSessionName)) > 64 {
			t.Errorf("AssumeRole input = %+v", input)
		}
		var policy struct {
			Statement []struct {
				Action   []string `json:"Action"`
				Resource []string `json:"Resource"`
			} `json:"Statement"`
		}
		if json.Unmarshal([]byte(aws.ToString(input.Policy)), &policy) != nil || len(policy.Statement) != 1 ||
			len(policy.Statement[0].Action) != 1 || policy.Statement[0].Action[0] != "secretsmanager:GetSecretValue" ||
			len(policy.Statement[0].Resource) != 1 || policy.Statement[0].Resource[0] != secretARN {
			t.Errorf("AssumeRole inline policy was not exact-resource read-only: %+v", policy)
		}
		expires := time.Now().Add(5 * time.Minute)
		return &sts.AssumeRoleOutput{Credentials: &ststypes.Credentials{AccessKeyId: aws.String("AKIA-test"), SecretAccessKey: aws.String("secret"),
			SessionToken: aws.String("session"), Expiration: &expires}}, nil
	})
	client := testSecretClient(func(_ context.Context, input *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
		if aws.ToString(input.SecretId) != secretARN || aws.ToString(input.VersionId) != version || input.VersionStage != nil {
			t.Errorf("GetSecretValue input = %+v", input)
		}
		return &secretsmanager.GetSecretValueOutput{SecretString: aws.String("sk-ant-test-token"), VersionId: aws.String(version)}, nil
	})
	reader, err := newAWSSecretReader("us-east-1", assumer, func(region string, _ aws.Credentials) secretsAPI {
		if region != "ap-northeast-1" {
			t.Errorf("secret client region = %q", region)
		}
		return client
	})
	if err != nil {
		t.Fatal(err)
	}
	reader.now = time.Now
	secret, err := reader.ReadSecret(context.Background(), modelcredentials.SecretAccess{CredentialID: "mcred_testcred1", TenantID: "tenant_a",
		Provider: modelcredentials.ProviderAnthropic, Purpose: modelcredentials.PurposeClaudeAction, ReaderRoleARN: roleARN, SecretARN: secretARN, SecretVersionID: version, CredentialGeneration: 1})
	if err != nil || string(secret) != "sk-ant-test-token" {
		t.Fatalf("ReadSecret() = %q, %v", secret, err)
	}
}

func TestAWSSecretReaderRejectsUnpinnedOrOversizedValues(t *testing.T) {
	version := "0123456789abcdef0123456789abcdef"
	base := modelcredentials.SecretAccess{CredentialID: "mcred_testcred1", TenantID: "tenant_a", Provider: modelcredentials.ProviderAnthropic, Purpose: modelcredentials.PurposeClaudeAction,
		ReaderRoleARN: "arn:aws:iam::123456789012:role/hako/tenant-model-credential-reader/tenant-a", SecretARN: "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/model/a-AbCdEf",
		SecretVersionID: version, CredentialGeneration: 1}
	for _, response := range []*secretsmanager.GetSecretValueOutput{
		{SecretString: aws.String("sk-ant-token"), VersionId: aws.String("ffffffffffffffffffffffffffffffff")},
		{SecretString: aws.String(string(make([]byte, 8193))), VersionId: aws.String(version)},
	} {
		testResponse := response
		reader, err := newAWSSecretReader("us-east-1", testAssumer(func(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
			expires := time.Now().Add(time.Minute)
			return &sts.AssumeRoleOutput{Credentials: &ststypes.Credentials{AccessKeyId: aws.String("a"), SecretAccessKey: aws.String("b"), SessionToken: aws.String("c"), Expiration: &expires}}, nil
		}), func(string, aws.Credentials) secretsAPI {
			return testSecretClient(func(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
				return testResponse, nil
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		if secret, err := reader.ReadSecret(context.Background(), base); err == nil || len(secret) > 0 {
			t.Fatal("unversioned/oversized value was accepted")
		}
	}
}

func TestAWSSecretReaderRejectsRoleOutsideModelCredentialPurposePathBeforeAssumeRole(t *testing.T) {
	for _, roleARN := range []string{
		"arn:aws:iam::123456789012:role/hako/model-reader/tenant-a",
		"arn:aws:iam::123456789012:role/hako/tenant-private-key-reader/tenant-a",
		"arn:aws:iam::123456789012:role/hako/tenant-model-credential-reader/*",
		"arn:aws:iam::123456789012:role/hako/tenant-model-credential-reader/tenant/a",
	} {
		t.Run(roleARN, func(t *testing.T) {
			assumeRoleCalled := false
			reader, err := newAWSSecretReader("us-east-1", testAssumer(func(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
				assumeRoleCalled = true
				return nil, nil
			}), func(string, aws.Credentials) secretsAPI { return nil })
			if err != nil {
				t.Fatal(err)
			}
			_, err = reader.ReadSecret(context.Background(), modelcredentials.SecretAccess{
				CredentialID: "mcred_testcred1", TenantID: "tenant_a", Provider: modelcredentials.ProviderAnthropic,
				Purpose: modelcredentials.PurposeClaudeAction, ReaderRoleARN: roleARN,
				SecretARN:       "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/model/a-AbCdEf",
				SecretVersionID: "0123456789abcdef0123456789abcdef", CredentialGeneration: 1,
			})
			if err == nil || assumeRoleCalled {
				t.Fatalf("ReadSecret accepted disallowed Role ARN (err=%v, AssumeRole called=%v)", err, assumeRoleCalled)
			}
		})
	}
}
