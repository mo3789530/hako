package githubbroker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/deploypolicy"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var (
	ErrEnvironmentApprovalUnavailable = errors.New("GitHub Environment approval unavailable")
	ErrEnvironmentApprovalDenied      = errors.New("GitHub Environment approval denied")
)

const environmentApprovalRoute = "/v1/environment/approval/verify"

var environmentApprovalServicePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

type EnvironmentApprovalPin struct {
	TenantID      domain.TenantID
	ServiceID     string
	Installation  int64
	RepositoryID  int64
	IntegrationID string
	App           githubapp.TenantPrivateKeyAccess
	Repository    githubapp.RepositoryAccess
}

type EnvironmentApprovalResolver func(context.Context, domain.TenantID, string, int64, int64, string) (EnvironmentApprovalPin, error)
type EnvironmentApprovalReader func(context.Context, EnvironmentApprovalPin, string) error

type EnvironmentApprovalService struct {
	resolve EnvironmentApprovalResolver
	read    EnvironmentApprovalReader
}

func NewEnvironmentApprovalService(resolve EnvironmentApprovalResolver, read EnvironmentApprovalReader) (*EnvironmentApprovalService, error) {
	if resolve == nil || read == nil {
		return nil, ErrEnvironmentApprovalUnavailable
	}
	return &EnvironmentApprovalService{resolve: resolve, read: read}, nil
}

func (s *EnvironmentApprovalService) Verify(ctx context.Context, caller string, tenant domain.TenantID, serviceID string,
	installationID, repositoryID int64, environment string) error {
	if s == nil || ctx == nil || caller != controlPlaneURI || tenant == "" || !environmentApprovalServicePattern.MatchString(serviceID) ||
		installationID <= 0 || repositoryID <= 0 || !environmentApprovalNameValid(environment) {
		return ErrEnvironmentApprovalDenied
	}
	first, err := s.resolve(ctx, tenant, serviceID, installationID, repositoryID, environment)
	if errors.Is(err, ErrEnvironmentApprovalUnavailable) {
		return ErrEnvironmentApprovalUnavailable
	}
	if err != nil || !validEnvironmentApprovalPin(first, tenant, serviceID, installationID, repositoryID) {
		return ErrEnvironmentApprovalDenied
	}
	if err := s.read(ctx, first, environment); err != nil {
		if errors.Is(err, githubapp.ErrEnvironmentApprovalMissing) {
			return ErrEnvironmentApprovalDenied
		}
		return ErrEnvironmentApprovalUnavailable
	}
	second, err := s.resolve(ctx, tenant, serviceID, installationID, repositoryID, environment)
	if errors.Is(err, ErrEnvironmentApprovalUnavailable) {
		return ErrEnvironmentApprovalUnavailable
	}
	if err != nil || !sameEnvironmentApprovalPin(first, second) {
		return ErrEnvironmentApprovalDenied
	}
	return nil
}

func NewPostgresEnvironmentApprovalResolver(pool transaction.Beginner, policy transaction.Policy) EnvironmentApprovalResolver {
	return func(ctx context.Context, tenant domain.TenantID, serviceID string, installationID, repositoryID int64, environment string) (EnvironmentApprovalPin, error) {
		if pool == nil || ctx == nil || tenant == "" || !environmentApprovalServicePattern.MatchString(serviceID) || installationID <= 0 || repositoryID <= 0 || !environmentApprovalNameValid(environment) {
			return EnvironmentApprovalPin{}, ErrEnvironmentApprovalDenied
		}
		pin, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (EnvironmentApprovalPin, error) {
			var result EnvironmentApprovalPin
			var data []byte
			err := tx.QueryRow(ctx, `SELECT r.owner_login,r.repository_name,i.id,c.installation_id,c.github_repository_id,
				i.github_app_id,i.config_revision,i.credential_generation,COALESCE(c.oidc_deploy_policy,'{}'::jsonb)
				FROM tenant_software_catalog_services c
				JOIN tenant_github_repositories r ON r.tenant_id=c.tenant_id AND r.installation_id=c.installation_id AND r.github_repository_id=c.github_repository_id
				JOIN tenant_github_app_installation_bindings ib ON ib.tenant_id=c.tenant_id AND ib.installation_id=c.installation_id
				JOIN tenant_github_integrations i ON i.tenant_id=ib.tenant_id AND i.id=ib.integration_id AND i.status='active'
				JOIN github_app_registrations app ON app.id=i.app_registration_id AND app.github_app_id=i.github_app_id AND app.mode='tenant_owned' AND app.owner_tenant_id=i.tenant_id
				JOIN github_credential_bindings credential ON credential.tenant_id=i.tenant_id AND credential.id=i.credential_binding_id AND credential.github_app_id=i.github_app_id AND credential.status='approved'
				JOIN tenant_github_installations installation ON installation.tenant_id=ib.tenant_id AND installation.installation_id=ib.installation_id
				WHERE c.tenant_id=$1 AND c.service_id=$2 AND c.installation_id=$3 AND c.github_repository_id=$4
				AND ib.installation_id=$3 AND installation.status='active' AND installation.suspended_at IS NULL`,
				tenant, serviceID, installationID, repositoryID).Scan(&result.Repository.Owner, &result.Repository.Name, &result.IntegrationID,
				&result.Installation, &result.RepositoryID, &result.App.GitHubAppID, &result.App.ConfigRevision, &result.App.CredentialGeneration, &data)
			if err != nil {
				return EnvironmentApprovalPin{}, err
			}
			var configured deploypolicy.Policy
			if json.Unmarshal(data, &configured) != nil || configured.Validate() != nil || !configured.Enabled || !configured.RequireHumanApproval || !containsEnvironment(configured.Environments, environment) {
				return EnvironmentApprovalPin{}, ErrEnvironmentApprovalDenied
			}
			credential, err := githubregistry.ResolveActiveAppCredential(ctx, tx, tenant, result.IntegrationID, result.App.ConfigRevision, result.App.CredentialGeneration)
			if err != nil || credential.GitHubAppID != result.App.GitHubAppID {
				return EnvironmentApprovalPin{}, ErrEnvironmentApprovalDenied
			}
			result.TenantID = tenant
			result.ServiceID = serviceID
			result.App.TenantID = string(tenant)
			result.App.IntegrationID = result.IntegrationID
			result.App.ReaderRoleARN = credential.PrivateKeyReaderRoleARN
			result.App.SecretARN = credential.PrivateKeySecretARN
			result.App.SecretVersionID = credential.PrivateKeyVersionID
			return result, nil
		})
		if err != nil {
			if errors.Is(err, ErrEnvironmentApprovalDenied) || errors.Is(err, pgx.ErrNoRows) {
				return EnvironmentApprovalPin{}, ErrEnvironmentApprovalDenied
			}
			return EnvironmentApprovalPin{}, ErrEnvironmentApprovalUnavailable
		}
		return pin, nil
	}
}

func NewAWSEnvironmentApprovalReader(config aws.Config, pool transaction.Beginner, policy transaction.Policy, httpClient *http.Client) (EnvironmentApprovalReader, error) {
	if strings.TrimSpace(config.Region) == "" || pool == nil {
		return nil, ErrEnvironmentApprovalUnavailable
	}
	authorizer := githubregistry.CredentialAuthorizer{Pool: pool, Policy: policy}
	return func(ctx context.Context, pin EnvironmentApprovalPin, environment string) error {
		client, err := githubapp.NewTenantAppTokenClientFromConfig(pin.App, config, authorizer, httpClient)
		if err != nil {
			return ErrEnvironmentApprovalUnavailable
		}
		token, repository, err := client.EnvironmentApprovalToken(ctx, pin.TenantID, pin.Installation, pin.RepositoryID)
		if err != nil || repository != pin.Repository {
			return ErrEnvironmentApprovalUnavailable
		}
		if err := client.VerifyEnvironmentApproval(ctx, token, repository, environment); err != nil {
			return err
		}
		return nil
	}, nil
}

func validEnvironmentApprovalPin(pin EnvironmentApprovalPin, tenant domain.TenantID, serviceID string, installationID, repositoryID int64) bool {
	return pin.TenantID == tenant && pin.ServiceID == serviceID && pin.Installation == installationID && pin.RepositoryID == repositoryID &&
		pin.IntegrationID != "" && pin.App.TenantID == string(tenant) && pin.App.IntegrationID == pin.IntegrationID && pin.App.GitHubAppID > 0 &&
		pin.App.ConfigRevision > 0 && pin.App.CredentialGeneration > 0 && pin.App.ReaderRoleARN != "" && pin.App.SecretARN != "" && pin.App.SecretVersionID != "" &&
		validBrokerLogin(pin.Repository.Owner) && validCredentialRepository(pin.Repository.Name)
}

func sameEnvironmentApprovalPin(a, b EnvironmentApprovalPin) bool {
	return a.TenantID == b.TenantID && a.ServiceID == b.ServiceID && a.Installation == b.Installation && a.RepositoryID == b.RepositoryID &&
		a.IntegrationID == b.IntegrationID && a.App == b.App && a.Repository == b.Repository
}

func containsEnvironment(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func environmentApprovalNameValid(value string) bool {
	if value == "" || len(value) > 64 || strings.TrimSpace(value) != value || strings.Contains(value, "..") {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '.' || char == '_' || char == '-') {
			return false
		}
	}
	return true
}
