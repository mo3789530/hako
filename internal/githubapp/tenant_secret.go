package githubapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const tenantSecretReadTimeout = 10 * time.Second

var errTenantPrivateKeyUnavailable = errors.New("tenant GitHub App private key unavailable")

// TenantPrivateKeyAccess is an internal, database-resolved credential pin.
// Never populate it directly from an API/CLI request or log its ARN fields.
type TenantPrivateKeyAccess struct {
	TenantID             string
	IntegrationID        string
	GitHubAppID          int64
	ConfigRevision       int64
	CredentialGeneration int64
	ReaderRoleARN        string
	SecretARN            string
	SecretVersionID      string
}

type STSAssumeRoleAPI interface {
	AssumeRole(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)
}

type AssumedSecretClientFactory func(region string, credentials aws.Credentials) SecretValueAPI

// TenantPrivateKeySecretAPI assumes the binding's exact, approved reader role
// and reads only its exact Secret ARN and immutable VersionId. It deliberately
// implements just the Secrets Manager operation needed by TokenClient.
type TenantPrivateKeySecretAPI struct {
	access  TenantPrivateKeyAccess
	sts     STSAssumeRoleAPI
	clients AssumedSecretClientFactory
	now     func() time.Time
}

// NewTenantPrivateKeySecretAPI is injectable so tests can use fake STS and
// Secrets Manager implementations without AWS credentials or network access.
func NewTenantPrivateKeySecretAPI(access TenantPrivateKeyAccess, assumer STSAssumeRoleAPI, clients AssumedSecretClientFactory) (*TenantPrivateKeySecretAPI, error) {
	if !access.valid() || assumer == nil || clients == nil {
		return nil, credentialError("invalid_configuration")
	}
	return &TenantPrivateKeySecretAPI{access: access, sts: assumer, clients: clients, now: time.Now}, nil
}

// NewTenantPrivateKeySecretAPIFromConfig creates the real SDK clients using
// the process role for STS and the returned short-lived role credentials only
// for Secrets Manager. No process-wide AWS credentials are changed.
func NewTenantPrivateKeySecretAPIFromConfig(access TenantPrivateKeyAccess, config aws.Config) (*TenantPrivateKeySecretAPI, error) {
	if strings.TrimSpace(config.Region) == "" {
		return nil, credentialError("invalid_configuration")
	}
	return NewTenantPrivateKeySecretAPI(access, sts.NewFromConfig(config), func(region string, assumed aws.Credentials) SecretValueAPI {
		secretConfig := config
		secretConfig.Region = region
		secretConfig.Credentials = credentials.NewStaticCredentialsProvider(assumed.AccessKeyID, assumed.SecretAccessKey, assumed.SessionToken)
		return secretsmanager.NewFromConfig(secretConfig)
	})
}

func (a TenantPrivateKeyAccess) valid() bool {
	return validInternalID(a.TenantID) && validInternalID(a.IntegrationID) && a.GitHubAppID > 0 && a.ConfigRevision > 0 && a.CredentialGeneration > 0 && validExactReaderRoleARN(a.ReaderRoleARN) && validExactSecretARN(a.SecretARN) && validSecretVersionID(a.SecretVersionID)
}

func validInternalID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func validExactReaderRoleARN(value string) bool {
	parsed, err := parseAWSARN(value)
	return err == nil && parsed.Service == "iam" && parsed.Region == "" && strings.HasPrefix(parsed.Resource, "role/") && len(parsed.Resource) > len("role/")
}

func validExactSecretARN(value string) bool {
	parsed, err := parseAWSARN(value)
	return err == nil && parsed.Service == "secretsmanager" && parsed.Region != "" && strings.HasPrefix(parsed.Resource, "secret:") && len(parsed.Resource) > len("secret:")
}

func parseAWSARN(value string) (awsARN, error) {
	parts := strings.SplitN(value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" || len(parts[4]) != 12 || strings.ContainsAny(value, "*?\r\n") {
		return awsARN{}, errors.New("invalid AWS ARN")
	}
	for _, r := range parts[4] {
		if r < '0' || r > '9' {
			return awsARN{}, errors.New("invalid AWS ARN")
		}
	}
	return awsARN{Service: parts[2], Region: parts[3], Resource: parts[5]}, nil
}

type awsARN struct{ Service, Region, Resource string }

func validSecretVersionID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func (c *TenantPrivateKeySecretAPI) GetSecretValue(ctx context.Context, input *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	if c == nil || input == nil || input.SecretId == nil || *input.SecretId != c.access.SecretARN || input.VersionId == nil || *input.VersionId != c.access.SecretVersionID || input.VersionStage != nil {
		return nil, errTenantPrivateKeyUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, tenantSecretReadTimeout)
	defer cancel()
	policy, err := json.Marshal(map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect":   "Allow",
			"Action":   []string{"secretsmanager:GetSecretValue"},
			"Resource": []string{c.access.SecretARN},
		}},
	})
	if err != nil {
		return nil, errTenantPrivateKeyUnavailable
	}
	sessionName := c.roleSessionName()
	assumed, err := c.sts.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(c.access.ReaderRoleARN),
		RoleSessionName: aws.String(sessionName),
		DurationSeconds: aws.Int32(900),
		Policy:          aws.String(string(policy)),
	})
	if err != nil || assumed == nil || assumed.Credentials == nil {
		return nil, errTenantPrivateKeyUnavailable
	}
	issued := assumed.Credentials
	if aws.ToString(issued.AccessKeyId) == "" || aws.ToString(issued.SecretAccessKey) == "" || aws.ToString(issued.SessionToken) == "" || issued.Expiration == nil || !issued.Expiration.After(c.now()) {
		return nil, errTenantPrivateKeyUnavailable
	}
	credential := aws.Credentials{AccessKeyID: aws.ToString(issued.AccessKeyId), SecretAccessKey: aws.ToString(issued.SecretAccessKey), SessionToken: aws.ToString(issued.SessionToken), Source: "AssumeRole", CanExpire: true, Expires: *issued.Expiration}
	secretClient := c.clients(c.secretRegion(), credential)
	if secretClient == nil {
		return nil, errTenantPrivateKeyUnavailable
	}
	response, err := secretClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(c.access.SecretARN), VersionId: aws.String(c.access.SecretVersionID)})
	if err != nil || response == nil || response.SecretString == nil || len(*response.SecretString) > 65536 || aws.ToString(response.VersionId) != c.access.SecretVersionID {
		return nil, errTenantPrivateKeyUnavailable
	}
	return &secretsmanager.GetSecretValueOutput{SecretString: response.SecretString, VersionId: response.VersionId}, nil
}

func (c *TenantPrivateKeySecretAPI) secretRegion() string {
	parts := strings.SplitN(c.access.SecretARN, ":", 6)
	if len(parts) != 6 {
		return ""
	}
	return parts[3]
}

func (c *TenantPrivateKeySecretAPI) roleSessionName() string {
	identity := fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%d", c.access.TenantID, c.access.IntegrationID, c.access.GitHubAppID, c.access.ConfigRevision, c.access.CredentialGeneration)
	digest := sha256.Sum256([]byte(identity))
	return "hako-app-proof-" + hex.EncodeToString(digest[:8])
}
