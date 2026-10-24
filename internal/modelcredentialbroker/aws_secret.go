package modelcredentialbroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/mo3789530/hako/internal/store/modelcredentials"
)

const secretReadTimeout = 10 * time.Second

type stsAPI interface {
	AssumeRole(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)
}

type secretsAPI interface {
	GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

type assumedClientFactory func(string, aws.Credentials) secretsAPI

type AWSSecretReader struct {
	region  string
	sts     stsAPI
	clients assumedClientFactory
	now     func() time.Time
}

func NewAWSSecretReader(config aws.Config) (*AWSSecretReader, error) {
	if strings.TrimSpace(config.Region) == "" {
		return nil, ErrUnavailable
	}
	return newAWSSecretReader(config.Region, sts.NewFromConfig(config), func(region string, assumed aws.Credentials) secretsAPI {
		secretConfig := config
		secretConfig.Region = region
		secretConfig.Credentials = credentials.NewStaticCredentialsProvider(assumed.AccessKeyID, assumed.SecretAccessKey, assumed.SessionToken)
		return secretsmanager.NewFromConfig(secretConfig)
	})
}

func newAWSSecretReader(region string, assumer stsAPI, clients assumedClientFactory) (*AWSSecretReader, error) {
	if strings.TrimSpace(region) == "" || assumer == nil || clients == nil {
		return nil, ErrUnavailable
	}
	return &AWSSecretReader{region: region, sts: assumer, clients: clients, now: time.Now}, nil
}

func (r *AWSSecretReader) ReadSecret(ctx context.Context, access modelcredentials.SecretAccess) ([]byte, error) {
	if r == nil || ctx == nil || access.TenantID == "" || access.CredentialID == "" || access.Provider != modelcredentials.ProviderAnthropic ||
		!modelcredentials.ValidPurpose(access.Purpose) ||
		access.CredentialGeneration <= 0 || !validRoleARN(access.ReaderRoleARN) || !validSecretARN(access.SecretARN) ||
		!modelcredentials.ValidSecretVersionID(access.SecretVersionID) {
		return nil, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, secretReadTimeout)
	defer cancel()
	policy, err := json.Marshal(map[string]any{"Version": "2012-10-17", "Statement": []map[string]any{{
		"Effect": "Allow", "Action": []string{"secretsmanager:GetSecretValue"}, "Resource": []string{access.SecretARN},
	}}})
	if err != nil {
		return nil, ErrUnavailable
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d", access.TenantID, access.CredentialID, access.Provider, access.Purpose, access.CredentialGeneration)))
	assumed, err := r.sts.AssumeRole(ctx, &sts.AssumeRoleInput{RoleArn: aws.String(access.ReaderRoleARN),
		RoleSessionName: aws.String("hako-model-" + hex.EncodeToString(digest[:8])), DurationSeconds: aws.Int32(900), Policy: aws.String(string(policy))})
	if err != nil || assumed == nil || assumed.Credentials == nil {
		return nil, ErrUnavailable
	}
	issued := assumed.Credentials
	if aws.ToString(issued.AccessKeyId) == "" || aws.ToString(issued.SecretAccessKey) == "" || aws.ToString(issued.SessionToken) == "" ||
		issued.Expiration == nil || !issued.Expiration.After(r.now()) {
		return nil, ErrUnavailable
	}
	credentials := aws.Credentials{AccessKeyID: aws.ToString(issued.AccessKeyId), SecretAccessKey: aws.ToString(issued.SecretAccessKey),
		SessionToken: aws.ToString(issued.SessionToken), Source: "AssumeRole", CanExpire: true, Expires: *issued.Expiration}
	client := r.clients(secretRegion(access.SecretARN), credentials)
	if client == nil {
		return nil, ErrUnavailable
	}
	result, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(access.SecretARN), VersionId: aws.String(access.SecretVersionID)})
	if err != nil || result == nil || result.SecretString == nil || aws.ToString(result.VersionId) != access.SecretVersionID ||
		len(*result.SecretString) == 0 || len(*result.SecretString) > 8192 {
		return nil, ErrUnavailable
	}
	return []byte(*result.SecretString), nil
}

func validRoleARN(value string) bool {
	parts := strings.SplitN(value, ":", 6)
	return len(parts) == 6 && parts[0] == "arn" && validPartition(parts[1]) && parts[2] == "iam" && parts[3] == "" && validAccount(parts[4]) &&
		strings.HasPrefix(parts[5], "role/") && len(parts[5]) > len("role/") && !strings.ContainsAny(value, "*?\r\n")
}

func validSecretARN(value string) bool {
	parts := strings.SplitN(value, ":", 6)
	return len(parts) == 6 && parts[0] == "arn" && validPartition(parts[1]) && parts[2] == "secretsmanager" && parts[3] != "" && validAccount(parts[4]) &&
		strings.HasPrefix(parts[5], "secret:") && len(parts[5]) > len("secret:") && !strings.ContainsAny(value, "*?\r\n")
}

func validPartition(value string) bool {
	return value == "aws" || value == "aws-us-gov" || value == "aws-cn"
}

func validAccount(value string) bool {
	if len(value) != 12 {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func secretRegion(secretARN string) string {
	parts := strings.SplitN(secretARN, ":", 6)
	if len(parts) != 6 {
		return ""
	}
	return parts[3]
}
