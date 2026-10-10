package api

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
)

// TenantGitHubWebhookCredentialResolver uses only the Webhook-purpose reader
// metadata. It never resolves private-key or OAuth references.
type TenantGitHubWebhookCredentialResolver struct {
	Pool   transaction.Beginner
	Policy transaction.Policy
	Config aws.Config
}

func (r TenantGitHubWebhookCredentialResolver) ResolveWebhookCredential(ctx context.Context, integrationID string) (githubwebhook.WebhookCredential, error) {
	if r.Pool == nil {
		return githubwebhook.WebhookCredential{}, githubregistry.ErrAppBindingDenied
	}
	access, err := transaction.Within(ctx, r.Pool, r.Policy, func(ctx context.Context, tx pgx.Tx) (githubregistry.AppWebhookCredentialAccess, error) {
		return githubregistry.ResolveActiveWebhookCredentialAccess(ctx, tx, integrationID)
	})
	if err != nil {
		return githubwebhook.WebhookCredential{}, githubregistry.ErrAppBindingDenied
	}
	reader, err := githubapp.NewTenantWebhookSecretReaderFromConfig(githubapp.TenantWebhookSecretAccess{
		TenantID: string(access.TenantID), IntegrationID: access.IntegrationID, AppID: access.GitHubAppID,
		CredentialGeneration: access.CredentialGeneration, ReaderRoleARN: access.ReaderRoleARN,
		SecretARN: access.SecretARN, VersionID: access.SecretVersionID,
		PreviousVersionID: access.PreviousVersionID,
		PreviousValidUntil: func() time.Time {
			if access.PreviousValidUntil != nil {
				return *access.PreviousValidUntil
			}
			return time.Time{}
		}(),
	}, r.Config)
	if err != nil {
		return githubwebhook.WebhookCredential{}, err
	}
	secrets, err := reader.Read(ctx)
	if err != nil {
		return githubwebhook.WebhookCredential{}, err
	}
	credential := githubwebhook.WebhookCredential{IntegrationID: access.IntegrationID, Generation: access.CredentialGeneration, CurrentSecret: secrets.Current, PreviousSecret: secrets.Previous}
	if access.PreviousValidUntil != nil {
		credential.PreviousUntil = *access.PreviousValidUntil
	}
	return credential, nil
}

var _ githubwebhook.WebhookCredentialResolver = TenantGitHubWebhookCredentialResolver{}
