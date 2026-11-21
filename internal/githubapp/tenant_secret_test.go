package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/aws-sdk-go-v2/service/sts/types"
)

const (
	tenantProofSecretARN = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/tenant/key-AbCdEf"
	tenantProofRoleARN   = "arn:aws:iam::123456789012:role/hako/tenant-key-reader"
	tenantProofVersion   = "0123456789abcdef0123456789abcdef"
)

type assumeRoleFunc func(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)

func (f assumeRoleFunc) AssumeRole(ctx context.Context, input *sts.AssumeRoleInput, options ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	return f(ctx, input, options...)
}

func tenantProofAccess() TenantPrivateKeyAccess {
	return TenantPrivateKeyAccess{
		TenantID:             "tenant-a",
		IntegrationID:        "integration-a",
		GitHubAppID:          123,
		ConfigRevision:       4,
		CredentialGeneration: 7,
		ReaderRoleARN:        tenantProofRoleARN,
		SecretARN:            tenantProofSecretARN,
		SecretVersionID:      tenantProofVersion,
	}
}

func assumedRoleResult(expiration time.Time) *sts.AssumeRoleOutput {
	return &sts.AssumeRoleOutput{Credentials: &types.Credentials{
		AccessKeyId:     aws.String("ASIAEXAMPLE"),
		SecretAccessKey: aws.String("assumed-secret"),
		SessionToken:    aws.String("assumed-session"),
		Expiration:      &expiration,
	}}
}

func TestTenantPrivateKeySecretAPIUsesExactRolePolicyAndVersion(t *testing.T) {
	var assumeCalls, secretCalls int
	assumer := assumeRoleFunc(func(_ context.Context, input *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
		assumeCalls++
		if aws.ToString(input.RoleArn) != tenantProofRoleARN || aws.ToInt32(input.DurationSeconds) != 900 || !strings.HasPrefix(aws.ToString(input.RoleSessionName), "hako-app-proof-") {
			t.Fatal("STS request did not use the approved role and bounded session")
		}
		var policy struct {
			Version   string `json:"Version"`
			Statement []struct {
				Effect   string   `json:"Effect"`
				Action   []string `json:"Action"`
				Resource []string `json:"Resource"`
			} `json:"Statement"`
		}
		if err := json.Unmarshal([]byte(aws.ToString(input.Policy)), &policy); err != nil || policy.Version != "2012-10-17" || len(policy.Statement) != 1 || policy.Statement[0].Effect != "Allow" || len(policy.Statement[0].Action) != 1 || policy.Statement[0].Action[0] != "secretsmanager:GetSecretValue" || len(policy.Statement[0].Resource) != 1 || policy.Statement[0].Resource[0] != tenantProofSecretARN {
			t.Fatal("STS session policy was not restricted to the exact Secret ARN")
		}
		return assumedRoleResult(time.Now().Add(10 * time.Minute)), nil
	})
	api, err := NewTenantPrivateKeySecretAPI(tenantProofAccess(), assumer, func(region string, credentials aws.Credentials) SecretValueAPI {
		if region != "ap-northeast-1" || credentials.AccessKeyID != "ASIAEXAMPLE" || credentials.SessionToken != "assumed-session" || !credentials.CanExpire {
			t.Fatal("Secrets Manager client did not receive short-lived assumed credentials in the Secret region")
		}
		return secretAPIFunc(func(_ context.Context, input *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
			secretCalls++
			if aws.ToString(input.SecretId) != tenantProofSecretARN || aws.ToString(input.VersionId) != tenantProofVersion || input.VersionStage != nil {
				t.Fatal("Secrets Manager request did not pin exact Secret ARN and VersionId")
			}
			key := "generated-private-key"
			return &secretsmanager.GetSecretValueOutput{SecretString: &key, VersionId: aws.String(tenantProofVersion)}, nil
		})
	})
	if err != nil {
		t.Fatal("valid Tenant private-key provider rejected")
	}
	got, err := api.GetSecretValue(context.Background(), &secretsmanager.GetSecretValueInput{SecretId: aws.String(tenantProofSecretARN), VersionId: aws.String(tenantProofVersion)})
	if err != nil || aws.ToString(got.VersionId) != tenantProofVersion || aws.ToString(got.SecretString) != "generated-private-key" || assumeCalls != 1 || secretCalls != 1 {
		t.Fatal("pinned Tenant private-key read failed")
	}
}

func TestTenantPrivateKeySecretAPIFailsClosedAndRedactsFailures(t *testing.T) {
	assumeCalls := 0
	assumer := assumeRoleFunc(func(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
		assumeCalls++
		return nil, errors.New("sensitive AWS response mentioning " + tenantProofSecretARN)
	})
	api, err := NewTenantPrivateKeySecretAPI(tenantProofAccess(), assumer, func(string, aws.Credentials) SecretValueAPI {
		t.Fatal("secret client created after failed AssumeRole")
		return nil
	})
	if err != nil {
		t.Fatal("valid Tenant private-key provider rejected")
	}
	for _, input := range []*secretsmanager.GetSecretValueInput{
		{SecretId: aws.String("arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:other"), VersionId: aws.String(tenantProofVersion)},
		{SecretId: aws.String(tenantProofSecretARN), VersionStage: aws.String("AWSCURRENT")},
		{SecretId: aws.String(tenantProofSecretARN), VersionId: aws.String("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
	} {
		_, err := api.GetSecretValue(context.Background(), input)
		if err == nil || strings.Contains(err.Error(), tenantProofSecretARN) || assumeCalls != 0 {
			t.Fatal("caller-controlled Secret reference reached STS or leaked in error")
		}
	}
	_, err = api.GetSecretValue(context.Background(), &secretsmanager.GetSecretValueInput{SecretId: aws.String(tenantProofSecretARN), VersionId: aws.String(tenantProofVersion)})
	if err == nil || strings.Contains(err.Error(), tenantProofSecretARN) || strings.Contains(err.Error(), "sensitive AWS response") || assumeCalls != 1 {
		t.Fatal("AssumeRole failure leaked details or was not safely classified")
	}
}

func TestTenantPrivateKeySecretAPIRejectsWrongVersionAndExpiredSTS(t *testing.T) {
	for _, test := range []struct {
		name        string
		credentials *types.Credentials
		response    *secretsmanager.GetSecretValueOutput
	}{
		{name: "missing credentials"},
		{name: "expired credentials", credentials: assumedRoleResult(time.Now().Add(-time.Minute)).Credentials},
		{name: "version mismatch", credentials: assumedRoleResult(time.Now().Add(time.Minute)).Credentials, response: &secretsmanager.GetSecretValueOutput{SecretString: aws.String("key"), VersionId: aws.String("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, err := NewTenantPrivateKeySecretAPI(tenantProofAccess(), assumeRoleFunc(func(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
				if test.credentials == nil {
					return &sts.AssumeRoleOutput{}, nil
				}
				return &sts.AssumeRoleOutput{Credentials: test.credentials}, nil
			}), func(string, aws.Credentials) SecretValueAPI {
				return secretAPIFunc(func(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
					return test.response, nil
				})
			})
			if err != nil {
				t.Fatal("valid Tenant private-key provider rejected")
			}
			_, err = api.GetSecretValue(context.Background(), &secretsmanager.GetSecretValueInput{SecretId: aws.String(tenantProofSecretARN), VersionId: aws.String(tenantProofVersion)})
			if err == nil || err.Error() != errTenantPrivateKeyUnavailable.Error() {
				t.Fatal("invalid temporary credentials or version mismatch did not fail closed")
			}
		})
	}
}

func TestTenantPrivateKeyAccessValidationRejectsWildcardAndMutablePins(t *testing.T) {
	for _, mutate := range []func(*TenantPrivateKeyAccess){
		func(access *TenantPrivateKeyAccess) { access.ReaderRoleARN = "arn:aws:iam::123456789012:role/*" },
		func(access *TenantPrivateKeyAccess) {
			access.SecretARN = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:*"
		},
		func(access *TenantPrivateKeyAccess) { access.SecretVersionID = "AWSCURRENT" },
		func(access *TenantPrivateKeyAccess) { access.CredentialGeneration = 0 },
	} {
		access := tenantProofAccess()
		mutate(&access)
		if _, err := NewTenantPrivateKeySecretAPI(access, assumeRoleFunc(func(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
			return nil, nil
		}), func(string, aws.Credentials) SecretValueAPI { return nil }); err == nil {
			t.Fatal("untrusted or incomplete credential pin accepted")
		}
	}
}
