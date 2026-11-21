package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

func TestTenantOAuthReaderPinsPurposeRoleAndVersion(t *testing.T) {
	access := TenantOAuthAccess{TenantID: "tenant_a", IntegrationID: "integration_a", AppID: 123, ConfigRevision: 2, CredentialGeneration: 4, ReaderRoleARN: "arn:aws:iam::123456789012:role/hako/oauth-reader", SecretARN: "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/oauth-AbCdEf", VersionID: tenantProofVersion}
	for _, mode := range []string{"success", "sts_failure", "expired", "wrong_version", "empty", "secret_failure", "nil_client"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			reader, err := NewTenantOAuthSecretReader(access, assumeRoleFunc(func(_ context.Context, in *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
				if aws.ToString(in.RoleArn) != access.ReaderRoleARN || !strings.HasPrefix(aws.ToString(in.RoleSessionName), "hako-oauth-") || aws.ToInt32(in.DurationSeconds) != 900 {
					t.Fatal("incorrect OAuth STS identity")
				}
				var policy struct {
					Statement []struct {
						Resource []string
						Action   []string
					}
				}
				if json.Unmarshal([]byte(aws.ToString(in.Policy)), &policy) != nil || len(policy.Statement) != 1 || len(policy.Statement[0].Resource) != 1 || policy.Statement[0].Resource[0] != access.SecretARN || len(policy.Statement[0].Action) != 1 || policy.Statement[0].Action[0] != "secretsmanager:GetSecretValue" {
					t.Fatal("unbounded session policy")
				}
				if mode == "sts_failure" {
					return nil, errors.New("sensitive-secret")
				}
				expiry := time.Now().Add(time.Minute)
				if mode == "expired" {
					expiry = time.Now().Add(-time.Minute)
				}
				return assumedRoleResult(expiry), nil
			}), func(region string, c aws.Credentials) SecretValueAPI {
				if region != "ap-northeast-1" || !c.CanExpire || c.SessionToken == "" {
					t.Fatal("invalid assumed client")
				}
				if mode == "nil_client" {
					return nil
				}
				return secretAPIFunc(func(_ context.Context, in *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
					calls++
					if aws.ToString(in.SecretId) != access.SecretARN || aws.ToString(in.VersionId) != access.VersionID || in.VersionStage != nil {
						t.Fatal("unpinned secret request")
					}
					if mode == "secret_failure" {
						return nil, errors.New("sensitive-secret")
					}
					version, value := access.VersionID, "fake-oauth-secret"
					if mode == "wrong_version" {
						version = strings.Repeat("a", 32)
					}
					if mode == "empty" {
						value = " "
					}
					return &secretsmanager.GetSecretValueOutput{SecretString: &value, VersionId: &version}, nil
				})
			})
			if err != nil {
				t.Fatal(err)
			}
			value, err := reader.Read(context.Background())
			if mode == "success" {
				if err != nil || value != "fake-oauth-secret" || calls != 1 {
					t.Fatal("read failed")
				}
			} else if value != "" || !errors.Is(err, errTenantOAuthUnavailable) || strings.Contains(err.Error(), "sensitive-secret") {
				t.Fatal("failure leaked credential")
			}
			if (mode == "sts_failure" || mode == "expired" || mode == "nil_client") && calls != 0 {
				t.Fatal("secret requested after failure")
			}
		})
	}
	encoded, _ := json.Marshal(access)
	if strings.Contains(string(encoded), access.SecretARN) || strings.Contains(fmt.Sprintf("%#v", access), access.ReaderRoleARN) {
		t.Fatal("access metadata leaked")
	}
	access.VersionID = "AWSCURRENT"
	if _, err := NewTenantOAuthSecretReader(access, assumeRoleFunc(nil), func(string, aws.Credentials) SecretValueAPI { return nil }); err == nil {
		t.Fatal("mutable version accepted")
	}
	var reader *TenantOAuthSecretReader
	if _, err := reader.Read(context.Background()); !errors.Is(err, errTenantOAuthUnavailable) {
		t.Fatal("nil reader accepted")
	}
}
