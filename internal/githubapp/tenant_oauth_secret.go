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

var errTenantOAuthUnavailable = errors.New("tenant GitHub OAuth credential unavailable")

// TenantOAuthAccess must come from the trusted OAuth-purpose DB resolver.
// Never accept Role/Secret fields from an HTTP request.
type TenantOAuthAccess struct {
	TenantID, IntegrationID                     string
	AppID, ConfigRevision, CredentialGeneration int64
	ReaderRoleARN, SecretARN, VersionID         string
}

func (TenantOAuthAccess) String() string     { return "Tenant OAuth access [REDACTED]" }
func (a TenantOAuthAccess) GoString() string { return a.String() }
func (TenantOAuthAccess) MarshalJSON() ([]byte, error) {
	return json.Marshal("[REDACTED Tenant OAuth access]")
}

type TenantOAuthSecretReader struct {
	access  TenantOAuthAccess
	sts     STSAssumeRoleAPI
	clients AssumedSecretClientFactory
	now     func() time.Time
}

func NewTenantOAuthSecretReader(a TenantOAuthAccess, assumer STSAssumeRoleAPI, clients AssumedSecretClientFactory) (*TenantOAuthSecretReader, error) {
	if !validInternalID(a.TenantID) || !validInternalID(a.IntegrationID) || a.AppID <= 0 || a.ConfigRevision <= 0 || a.CredentialGeneration <= 0 || !validExactReaderRoleARN(a.ReaderRoleARN) || !validExactSecretARN(a.SecretARN) || !validSecretVersionID(a.VersionID) || assumer == nil || clients == nil {
		return nil, errTenantOAuthUnavailable
	}
	return &TenantOAuthSecretReader{a, assumer, clients, time.Now}, nil
}

func NewTenantOAuthSecretReaderFromConfig(a TenantOAuthAccess, config aws.Config) (*TenantOAuthSecretReader, error) {
	if strings.TrimSpace(config.Region) == "" {
		return nil, errTenantOAuthUnavailable
	}
	return NewTenantOAuthSecretReader(a, sts.NewFromConfig(config), func(region string, assumed aws.Credentials) SecretValueAPI {
		cfg := config
		cfg.Region = region
		cfg.Credentials = credentials.NewStaticCredentialsProvider(assumed.AccessKeyID, assumed.SecretAccessKey, assumed.SessionToken)
		return secretsmanager.NewFromConfig(cfg)
	})
}

// Read is broker-internal. It returns no AWS credentials and caches no secret.
func (r *TenantOAuthSecretReader) Read(ctx context.Context) (string, error) {
	if r == nil || ctx == nil {
		return "", errTenantOAuthUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, tenantSecretReadTimeout)
	defer cancel()
	a := r.access
	policy, _ := json.Marshal(map[string]any{"Version": "2012-10-17", "Statement": []map[string]any{{"Effect": "Allow", "Action": []string{"secretsmanager:GetSecretValue"}, "Resource": []string{a.SecretARN}}}})
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%d", a.TenantID, a.IntegrationID, a.AppID, a.ConfigRevision, a.CredentialGeneration)))
	assumed, err := r.sts.AssumeRole(ctx, &sts.AssumeRoleInput{RoleArn: aws.String(a.ReaderRoleARN), RoleSessionName: aws.String("hako-oauth-" + hex.EncodeToString(digest[:8])), DurationSeconds: aws.Int32(900), Policy: aws.String(string(policy))})
	if err != nil || assumed == nil || assumed.Credentials == nil {
		return "", errTenantOAuthUnavailable
	}
	c := assumed.Credentials
	if aws.ToString(c.AccessKeyId) == "" || aws.ToString(c.SecretAccessKey) == "" || aws.ToString(c.SessionToken) == "" || c.Expiration == nil || !c.Expiration.After(r.now()) {
		return "", errTenantOAuthUnavailable
	}
	client := r.clients(secretRegion(a.SecretARN), aws.Credentials{AccessKeyID: aws.ToString(c.AccessKeyId), SecretAccessKey: aws.ToString(c.SecretAccessKey), SessionToken: aws.ToString(c.SessionToken), CanExpire: true, Expires: *c.Expiration, Source: "AssumeRole"})
	if client == nil {
		return "", errTenantOAuthUnavailable
	}
	result, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(a.SecretARN), VersionId: aws.String(a.VersionID)})
	if err != nil || result == nil || result.SecretString == nil || strings.TrimSpace(*result.SecretString) == "" || len(*result.SecretString) > 65536 || aws.ToString(result.VersionId) != a.VersionID {
		return "", errTenantOAuthUnavailable
	}
	return *result.SecretString, nil
}
