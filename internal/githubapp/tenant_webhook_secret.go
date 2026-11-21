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

var errTenantWebhookSecretUnavailable = errors.New("tenant GitHub App webhook secret unavailable")

type TenantWebhookSecretAccess struct {
	TenantID             string
	IntegrationID        string
	AppID                int64
	CredentialGeneration int64
	ReaderRoleARN        string
	SecretARN            string
	VersionID            string
	PreviousVersionID    string
	PreviousValidUntil   time.Time
}

func (TenantWebhookSecretAccess) String() string     { return "Tenant Webhook Secret access [REDACTED]" }
func (a TenantWebhookSecretAccess) GoString() string { return a.String() }
func (TenantWebhookSecretAccess) MarshalJSON() ([]byte, error) {
	return json.Marshal("[REDACTED Tenant Webhook Secret access]")
}

type WebhookSecrets struct {
	Current  []byte
	Previous []byte
}

func (WebhookSecrets) String() string     { return "Tenant GitHub App webhook secrets [REDACTED]" }
func (s WebhookSecrets) GoString() string { return s.String() }
func (WebhookSecrets) MarshalJSON() ([]byte, error) {
	return json.Marshal("[REDACTED Tenant GitHub App webhook secrets]")
}

type TenantWebhookSecretReader struct {
	access  TenantWebhookSecretAccess
	sts     STSAssumeRoleAPI
	clients AssumedSecretClientFactory
	now     func() time.Time
}

func NewTenantWebhookSecretReader(access TenantWebhookSecretAccess, assumer STSAssumeRoleAPI, clients AssumedSecretClientFactory) (*TenantWebhookSecretReader, error) {
	if !access.valid() || assumer == nil || clients == nil {
		return nil, credentialError("invalid_configuration")
	}
	return &TenantWebhookSecretReader{access: access, sts: assumer, clients: clients, now: time.Now}, nil
}

func NewTenantWebhookSecretReaderFromConfig(access TenantWebhookSecretAccess, config aws.Config) (*TenantWebhookSecretReader, error) {
	if strings.TrimSpace(config.Region) == "" {
		return nil, credentialError("invalid_configuration")
	}
	return NewTenantWebhookSecretReader(access, sts.NewFromConfig(config), func(region string, assumed aws.Credentials) SecretValueAPI {
		secretConfig := config
		secretConfig.Region = region
		secretConfig.Credentials = credentials.NewStaticCredentialsProvider(assumed.AccessKeyID, assumed.SecretAccessKey, assumed.SessionToken)
		return secretsmanager.NewFromConfig(secretConfig)
	})
}

func (a TenantWebhookSecretAccess) valid() bool {
	if !validInternalID(a.TenantID) || !validInternalID(a.IntegrationID) || a.AppID <= 0 || a.CredentialGeneration <= 0 ||
		!validExactReaderRoleARN(a.ReaderRoleARN) || !validExactSecretARN(a.SecretARN) || !validSecretVersionID(a.VersionID) {
		return false
	}
	return (a.PreviousVersionID == "" && a.PreviousValidUntil.IsZero()) || (validSecretVersionID(a.PreviousVersionID) && !a.PreviousValidUntil.IsZero())
}

func (r *TenantWebhookSecretReader) Read(ctx context.Context) (WebhookSecrets, error) {
	if r == nil || !r.access.valid() || ctx == nil {
		return WebhookSecrets{}, errTenantWebhookSecretUnavailable
	}
	readCtx, cancel := context.WithTimeout(ctx, tenantSecretReadTimeout)
	defer cancel()
	current, err := r.readVersion(readCtx, r.access.VersionID)
	if err != nil {
		return WebhookSecrets{}, errTenantWebhookSecretUnavailable
	}
	result := WebhookSecrets{Current: current}
	if r.access.PreviousVersionID != "" && r.now().Before(r.access.PreviousValidUntil) {
		previous, err := r.readVersion(readCtx, r.access.PreviousVersionID)
		if err != nil {
			return WebhookSecrets{}, errTenantWebhookSecretUnavailable
		}
		result.Previous = previous
	}
	return result, nil
}

func (r *TenantWebhookSecretReader) readVersion(ctx context.Context, versionID string) ([]byte, error) {
	policy, err := json.Marshal(map[string]any{"Version": "2012-10-17", "Statement": []map[string]any{{"Effect": "Allow", "Action": []string{"secretsmanager:GetSecretValue"}, "Resource": []string{r.access.SecretARN}}}})
	if err != nil {
		return nil, errTenantWebhookSecretUnavailable
	}
	sessionName := r.sessionName()
	assumed, err := r.sts.AssumeRole(ctx, &sts.AssumeRoleInput{RoleArn: aws.String(r.access.ReaderRoleARN), RoleSessionName: aws.String(sessionName), DurationSeconds: aws.Int32(900), Policy: aws.String(string(policy))})
	if err != nil || assumed == nil || assumed.Credentials == nil {
		return nil, errTenantWebhookSecretUnavailable
	}
	issued := assumed.Credentials
	if aws.ToString(issued.AccessKeyId) == "" || aws.ToString(issued.SecretAccessKey) == "" || aws.ToString(issued.SessionToken) == "" || issued.Expiration == nil || !issued.Expiration.After(r.now()) {
		return nil, errTenantWebhookSecretUnavailable
	}
	credentials := aws.Credentials{AccessKeyID: aws.ToString(issued.AccessKeyId), SecretAccessKey: aws.ToString(issued.SecretAccessKey), SessionToken: aws.ToString(issued.SessionToken), Source: "AssumeRole", CanExpire: true, Expires: *issued.Expiration}
	client := r.clients(secretRegion(r.access.SecretARN), credentials)
	if client == nil {
		return nil, errTenantWebhookSecretUnavailable
	}
	response, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(r.access.SecretARN), VersionId: aws.String(versionID)})
	if err != nil || response == nil || response.SecretString == nil || len(*response.SecretString) == 0 || len(*response.SecretString) > 65536 || aws.ToString(response.VersionId) != versionID {
		return nil, errTenantWebhookSecretUnavailable
	}
	return []byte(*response.SecretString), nil
}

func (r *TenantWebhookSecretReader) sessionName() string {
	identity := fmt.Sprintf("%s\x00%s\x00%d\x00%d", r.access.TenantID, r.access.IntegrationID, r.access.AppID, r.access.CredentialGeneration)
	digest := sha256.Sum256([]byte(identity))
	return "hako-webhook-" + hex.EncodeToString(digest[:8])
}

func secretRegion(secretARN string) string {
	parts := strings.SplitN(secretARN, ":", 6)
	if len(parts) != 6 {
		return ""
	}
	return parts[3]
}
