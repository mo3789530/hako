package githubbroker

import (
	"context"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
)

var ErrChecksDenied = errors.New("GitHub Checks credential unavailable")
var githubLoginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)

type ChecksRunIdentity struct {
	Run                  domain.WorkloadRun
	Shared               bool
	Access               githubapp.TenantPrivateKeyAccess
	Repository           githubapp.RepositoryAccess
	DesiredCheckRevision int64
}

type ChecksRunResolver func(context.Context, domain.TenantID, domain.WorkloadRunID) (ChecksRunIdentity, error)
type ChecksTokenMinter func(context.Context, ChecksRunIdentity) (githubapp.CheckToken, error)
type SharedChecksTokenSource func(context.Context, domain.WorkloadRun) (githubapp.CheckToken, githubapp.RepositoryAccess, error)
type TenantChecksTokenSource func(context.Context, ChecksRunIdentity) (githubapp.CheckToken, githubapp.RepositoryAccess, error)

type ChecksGrant struct {
	token      string
	expiresAt  time.Time
	repository githubapp.RepositoryAccess
}

func (ChecksGrant) String() string     { return "GitHub Checks grant [REDACTED]" }
func (g ChecksGrant) GoString() string { return g.String() }
func (ChecksGrant) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED GitHub Checks grant]"`), nil
}
func (g ChecksGrant) Token() string                          { return g.token }
func (g ChecksGrant) ExpiresAt() time.Time                   { return g.expiresAt }
func (g ChecksGrant) Repository() githubapp.RepositoryAccess { return g.repository }

type ChecksService struct {
	authorize CheckoutCallerAuthorizer
	resolve   ChecksRunResolver
	mint      ChecksTokenMinter
	now       func() time.Time
}

func NewChecksService(authorize CheckoutCallerAuthorizer, resolve ChecksRunResolver, mint ChecksTokenMinter) (*ChecksService, error) {
	if authorize == nil || resolve == nil || mint == nil {
		return nil, ErrUnavailable
	}
	return &ChecksService{authorize: authorize, resolve: resolve, mint: mint, now: time.Now}, nil
}

// NewPostgresChecksRunResolver resolves both the immutable Run credential pin
// and its durable Check state. Terminal Runs remain eligible so result delivery
// can finish after the Job has ended; disabled/rotated Apps fail closed.
func NewPostgresChecksRunResolver(pool transaction.Beginner, policy transaction.Policy) ChecksRunResolver {
	return func(ctx context.Context, tenant domain.TenantID, runID domain.WorkloadRunID) (ChecksRunIdentity, error) {
		if pool == nil || ctx == nil || tenant == "" || !validCheckoutRunID(runID) {
			return ChecksRunIdentity{}, ErrChecksDenied
		}
		value, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (ChecksRunIdentity, error) {
			run, err := workloads.Get(ctx, tx, tenant, runID)
			if err != nil || run.TenantID != tenant || run.ID != runID || run.Kind != domain.WorkloadJob ||
				run.GitHubInstallation < 1 || run.GitHubRepository < 1 || !checkoutCommitPattern.MatchString(run.CommitSHA) {
				return ChecksRunIdentity{}, ErrChecksDenied
			}
			check, err := workloads.GetGitHubCheck(ctx, tx, tenant, runID)
			if err != nil || check.TenantID != tenant || check.WorkloadRunID != runID ||
				check.GitHubInstallationID != run.GitHubInstallation || check.GitHubRepositoryID != run.GitHubRepository ||
				check.CommitSHA != run.CommitSHA || check.ExternalID != string(run.ID) || check.Revision < 1 {
				return ChecksRunIdentity{}, ErrChecksDenied
			}
			if run.GitHubIntegrationID == "" {
				if run.GitHubConfigRevision != 0 || run.GitHubCredentialGeneration != 0 {
					return ChecksRunIdentity{}, ErrChecksDenied
				}
				var owner, repositoryName string
				err = tx.QueryRow(ctx, `SELECT r.owner_login, r.repository_name FROM github_app_installation_bindings b
					JOIN tenant_github_installations i ON i.tenant_id=b.tenant_id AND i.installation_id=b.installation_id
					JOIN tenant_github_repositories r ON r.tenant_id=b.tenant_id AND r.installation_id=b.installation_id
					WHERE b.tenant_id=$1 AND b.installation_id=$2 AND r.github_repository_id=$3
					AND i.status='active' AND i.suspended_at IS NULL`, tenant, run.GitHubInstallation, run.GitHubRepository).
					Scan(&owner, &repositoryName)
				if err != nil || !validLogin(owner) || !validCredentialRepository(repositoryName) {
					return ChecksRunIdentity{}, ErrChecksDenied
				}
				return ChecksRunIdentity{Run: run, Shared: true, Repository: githubapp.RepositoryAccess{Owner: owner, Name: repositoryName}, DesiredCheckRevision: check.Revision}, nil
			}
			if run.GitHubConfigRevision < 1 || run.GitHubCredentialGeneration < 1 {
				return ChecksRunIdentity{}, ErrChecksDenied
			}
			var owner, repositoryName string
			err = tx.QueryRow(ctx, `SELECT r.owner_login, r.repository_name FROM tenant_github_app_installation_bindings b
				JOIN tenant_github_installations i ON i.tenant_id=b.tenant_id AND i.installation_id=b.installation_id
				JOIN tenant_github_repositories r ON r.tenant_id=b.tenant_id AND r.installation_id=b.installation_id
				WHERE b.tenant_id=$1 AND b.integration_id=$2 AND b.installation_id=$3 AND r.github_repository_id=$4
				AND i.status='active' AND i.suspended_at IS NULL`, tenant, run.GitHubIntegrationID, run.GitHubInstallation, run.GitHubRepository).
				Scan(&owner, &repositoryName)
			if err != nil || !validLogin(owner) || !validCredentialRepository(repositoryName) {
				return ChecksRunIdentity{}, ErrChecksDenied
			}
			credential, err := githubregistry.ResolveActiveAppCredential(ctx, tx, tenant, run.GitHubIntegrationID, run.GitHubConfigRevision, run.GitHubCredentialGeneration)
			if err != nil || credential.TenantID != tenant || credential.IntegrationID != run.GitHubIntegrationID ||
				credential.GitHubAppID <= 0 || credential.ConfigRevision != run.GitHubConfigRevision || credential.CredentialGeneration != run.GitHubCredentialGeneration {
				return ChecksRunIdentity{}, ErrChecksDenied
			}
			access := githubapp.TenantPrivateKeyAccess{TenantID: string(credential.TenantID), IntegrationID: credential.IntegrationID,
				GitHubAppID: credential.GitHubAppID, ConfigRevision: credential.ConfigRevision, CredentialGeneration: credential.CredentialGeneration,
				ReaderRoleARN: credential.PrivateKeyReaderRoleARN, SecretARN: credential.PrivateKeySecretARN, SecretVersionID: credential.PrivateKeyVersionID}
			return ChecksRunIdentity{Run: run, Access: access, Repository: githubapp.RepositoryAccess{Owner: owner, Name: repositoryName}, DesiredCheckRevision: check.Revision}, nil
		})
		if err != nil {
			return ChecksRunIdentity{}, ErrChecksDenied
		}
		return value, nil
	}
}

func NewAWSChecksTokenMinter(config aws.Config, pool transaction.Beginner, policy transaction.Policy, httpClient *http.Client) (ChecksTokenMinter, error) {
	if strings.TrimSpace(config.Region) == "" || pool == nil {
		return nil, ErrUnavailable
	}
	authorizer := githubregistry.CredentialAuthorizer{Pool: pool, Policy: policy}
	var sharedClient *githubapp.TokenClient
	if strings.TrimSpace(os.Getenv("HAKO_GITHUB_APP_ID")) != "" || strings.TrimSpace(os.Getenv("HAKO_GITHUB_APP_PRIVATE_KEY_SECRET_ARN")) != "" {
		sharedConfig, err := githubapp.TokenConfigFromEnv(os.Getenv)
		if err != nil {
			return nil, ErrUnavailable
		}
		sharedClient, err = githubapp.NewTokenClient(sharedConfig, secretsmanager.NewFromConfig(config), authorizer, httpClient)
		if err != nil {
			return nil, ErrUnavailable
		}
	}
	var sharedSource SharedChecksTokenSource
	if sharedClient != nil {
		sharedSource = func(ctx context.Context, run domain.WorkloadRun) (githubapp.CheckToken, githubapp.RepositoryAccess, error) {
			return sharedClient.ChecksRunToken(ctx, run)
		}
	}
	tenantSource := func(ctx context.Context, identity ChecksRunIdentity) (githubapp.CheckToken, githubapp.RepositoryAccess, error) {
		if identity.Run.GitHubIntegrationID == "" {
			return nil, githubapp.RepositoryAccess{}, ErrChecksDenied
		}
		client, err := githubapp.NewTenantAppTokenClientFromConfig(identity.Access, config, authorizer, httpClient)
		if err != nil {
			return nil, githubapp.RepositoryAccess{}, ErrUnavailable
		}
		return client.ChecksRunToken(ctx, identity.Run)
	}
	return NewChecksTokenMinter(tenantSource, sharedSource), nil
}

// NewChecksTokenMinter separates the existing shared App mode from Tenant-owned
// App credentials. A failure in either mode never falls back to the other.
func NewChecksTokenMinter(tenantOwned TenantChecksTokenSource, shared SharedChecksTokenSource) ChecksTokenMinter {
	return func(ctx context.Context, identity ChecksRunIdentity) (githubapp.CheckToken, error) {
		var token githubapp.CheckToken
		var repository githubapp.RepositoryAccess
		var err error
		if identity.Shared {
			if shared == nil || identity.Run.GitHubIntegrationID != "" || identity.Run.GitHubConfigRevision != 0 || identity.Run.GitHubCredentialGeneration != 0 {
				return nil, ErrChecksDenied
			}
			token, repository, err = shared(ctx, identity.Run)
		} else {
			if tenantOwned == nil || identity.Run.GitHubIntegrationID == "" {
				return nil, ErrChecksDenied
			}
			token, repository, err = tenantOwned(ctx, identity)
		}
		if err != nil || token == nil || repository != identity.Repository {
			return nil, ErrChecksDenied
		}
		return token, nil
	}
}

func (s *ChecksService) Issue(ctx context.Context, caller string, tenant domain.TenantID, runID domain.WorkloadRunID) (ChecksGrant, error) {
	if s == nil || ctx == nil || caller != controlPlaneURI || tenant == "" || !validCheckoutRunID(runID) {
		return ChecksGrant{}, ErrChecksDenied
	}
	if err := s.authorize(ctx, caller, tenant, runID); err != nil {
		return ChecksGrant{}, ErrChecksDenied
	}
	identity, err := s.resolve(ctx, tenant, runID)
	if err != nil || !eligibleChecksIdentity(identity, tenant, runID) {
		return ChecksGrant{}, ErrChecksDenied
	}
	token, err := s.mint(ctx, identity)
	if err != nil || token == nil || !validBrokerToken(token.Value(), token.ExpiresAt(), s.now()) || token.ExpiresAt().After(s.now().Add(65*time.Minute)) {
		return ChecksGrant{}, ErrChecksDenied
	}
	if err := s.authorize(ctx, caller, tenant, runID); err != nil {
		return ChecksGrant{}, ErrChecksDenied
	}
	current, err := s.resolve(ctx, tenant, runID)
	if err != nil || !sameChecksIdentity(identity, current) {
		return ChecksGrant{}, ErrChecksDenied
	}
	return ChecksGrant{token: token.Value(), expiresAt: token.ExpiresAt(), repository: identity.Repository}, nil
}

func eligibleChecksIdentity(identity ChecksRunIdentity, tenant domain.TenantID, runID domain.WorkloadRunID) bool {
	run := identity.Run
	if run.TenantID != tenant || run.ID != runID || run.Kind != domain.WorkloadJob || run.GitHubInstallation < 1 || run.GitHubRepository < 1 ||
		!checkoutCommitPattern.MatchString(run.CommitSHA) || identity.DesiredCheckRevision < 1 ||
		!validLogin(identity.Repository.Owner) || !validCredentialRepository(identity.Repository.Name) {
		return false
	}
	if identity.Shared {
		return run.GitHubIntegrationID == "" && run.GitHubConfigRevision == 0 && run.GitHubCredentialGeneration == 0 && identity.Access == (githubapp.TenantPrivateKeyAccess{})
	}
	return run.GitHubIntegrationID != "" && run.GitHubConfigRevision > 0 && run.GitHubCredentialGeneration > 0 &&
		identity.Access.TenantID == string(tenant) && identity.Access.IntegrationID == run.GitHubIntegrationID && identity.Access.GitHubAppID > 0 &&
		identity.Access.ConfigRevision == run.GitHubConfigRevision && identity.Access.CredentialGeneration == run.GitHubCredentialGeneration &&
		identity.Access.ReaderRoleARN != "" && identity.Access.SecretARN != "" && identity.Access.SecretVersionID != ""
}

func sameChecksIdentity(a, b ChecksRunIdentity) bool {
	return sameCheckoutIdentity(a.Run, b.Run) && a.Shared == b.Shared && a.DesiredCheckRevision == b.DesiredCheckRevision && a.Access == b.Access && a.Repository == b.Repository
}

func validLogin(value string) bool { return githubLoginPattern.MatchString(value) }
