package githubbroker

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
)

var ErrCheckoutDenied = errors.New("GitHub checkout credential unavailable")

var checkoutCommitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

type CheckoutRunResolver func(context.Context, domain.TenantID, domain.WorkloadRunID) (domain.WorkloadRun, githubapp.TenantPrivateKeyAccess, error)
type CheckoutCallerAuthorizer func(context.Context, string, domain.TenantID, domain.WorkloadRunID) error
type CheckoutTokenMinter func(context.Context, domain.WorkloadRun, githubapp.TenantPrivateKeyAccess) (string, time.Time, error)

type CheckoutGrant struct {
	token     string
	expiresAt time.Time
}

func (CheckoutGrant) String() string     { return "GitHub checkout grant [REDACTED]" }
func (g CheckoutGrant) GoString() string { return g.String() }
func (CheckoutGrant) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED GitHub checkout grant]"`), nil
}
func (g CheckoutGrant) Token() string        { return g.token }
func (g CheckoutGrant) ExpiresAt() time.Time { return g.expiresAt }

// CheckoutService issues a repository-scoped token only for a canonical,
// active Tenant-owned Run. The caller supplies identifiers, never credential
// metadata; both the Run and exact credential pin are resolved from storage.
type CheckoutService struct {
	authorize CheckoutCallerAuthorizer
	resolve   CheckoutRunResolver
	mint      CheckoutTokenMinter
	now       func() time.Time
}

func NewCheckoutService(authorize CheckoutCallerAuthorizer, resolve CheckoutRunResolver, mint CheckoutTokenMinter) (*CheckoutService, error) {
	if authorize == nil || resolve == nil || mint == nil {
		return nil, ErrUnavailable
	}
	return &CheckoutService{authorize: authorize, resolve: resolve, mint: mint, now: time.Now}, nil
}

// NewPostgresCheckoutRunResolver reads the Run and active App pin in one
// read-only transaction. It accepts only Tenant-owned Runs; shared Runs must
// continue through their existing shared credential provider.
func NewPostgresCheckoutRunResolver(pool transaction.Beginner, policy transaction.Policy) CheckoutRunResolver {
	return func(ctx context.Context, tenant domain.TenantID, runID domain.WorkloadRunID) (domain.WorkloadRun, githubapp.TenantPrivateKeyAccess, error) {
		if pool == nil || tenant == "" || !validCheckoutRunID(runID) {
			return domain.WorkloadRun{}, githubapp.TenantPrivateKeyAccess{}, ErrCheckoutDenied
		}
		type resolved struct {
			run    domain.WorkloadRun
			access githubapp.TenantPrivateKeyAccess
		}
		value, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (resolved, error) {
			run, err := workloads.Get(ctx, tx, tenant, runID)
			if err != nil || !eligibleTenantCheckoutRun(run, tenant, time.Now()) {
				return resolved{}, ErrCheckoutDenied
			}
			var repositoryBound bool
			err = tx.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM tenant_github_app_installation_bindings b
				JOIN tenant_github_installations i ON i.tenant_id=b.tenant_id AND i.installation_id=b.installation_id
				JOIN tenant_github_repositories r ON r.tenant_id=b.tenant_id AND r.installation_id=b.installation_id
				WHERE b.tenant_id=$1 AND b.integration_id=$2 AND b.installation_id=$3 AND r.github_repository_id=$4
				AND i.status='active' AND i.suspended_at IS NULL
			)`, tenant, run.GitHubIntegrationID, run.GitHubInstallation, run.GitHubRepository).Scan(&repositoryBound)
			if err != nil || !repositoryBound {
				return resolved{}, ErrCheckoutDenied
			}
			credential, err := githubregistry.ResolveActiveAppCredential(ctx, tx, tenant, run.GitHubIntegrationID, run.GitHubConfigRevision, run.GitHubCredentialGeneration)
			if err != nil || credential.TenantID != run.TenantID || credential.IntegrationID != run.GitHubIntegrationID ||
				credential.GitHubAppID <= 0 || credential.ConfigRevision != run.GitHubConfigRevision || credential.CredentialGeneration != run.GitHubCredentialGeneration {
				return resolved{}, ErrCheckoutDenied
			}
			access := githubapp.TenantPrivateKeyAccess{TenantID: string(credential.TenantID), IntegrationID: credential.IntegrationID,
				GitHubAppID: credential.GitHubAppID, ConfigRevision: credential.ConfigRevision, CredentialGeneration: credential.CredentialGeneration,
				ReaderRoleARN: credential.PrivateKeyReaderRoleARN, SecretARN: credential.PrivateKeySecretARN, SecretVersionID: credential.PrivateKeyVersionID}
			return resolved{run: run, access: access}, nil
		})
		if err != nil {
			return domain.WorkloadRun{}, githubapp.TenantPrivateKeyAccess{}, ErrCheckoutDenied
		}
		return value.run, value.access, nil
	}
}

// NewAWSCheckoutTokenMinter composes the exact Tenant private-key reader and
// live Repository/App authorizers. It must be constructed only in the trusted
// broker workload; it never returns AWS credentials or the App private key.
func NewAWSCheckoutTokenMinter(config aws.Config, pool transaction.Beginner, policy transaction.Policy, httpClient *http.Client) (CheckoutTokenMinter, error) {
	if strings.TrimSpace(config.Region) == "" || pool == nil {
		return nil, ErrUnavailable
	}
	authorizer := githubregistry.CredentialAuthorizer{Pool: pool, Policy: policy}
	return func(ctx context.Context, run domain.WorkloadRun, access githubapp.TenantPrivateKeyAccess) (string, time.Time, error) {
		client, err := githubapp.NewTenantAppTokenClientFromConfig(access, config, authorizer, httpClient)
		if err != nil {
			return "", time.Time{}, ErrUnavailable
		}
		token, err := client.CheckoutRunInstallationToken(ctx, run)
		if err != nil {
			return "", time.Time{}, ErrUnavailable
		}
		return token.Value(), token.ExpiresAt(), nil
	}, nil
}

func (s *CheckoutService) Issue(ctx context.Context, caller string, tenant domain.TenantID, runID domain.WorkloadRunID) (CheckoutGrant, error) {
	if s == nil || ctx == nil || caller != controlPlaneURI || tenant == "" || !validCheckoutRunID(runID) {
		return CheckoutGrant{}, ErrCheckoutDenied
	}
	if err := s.authorize(ctx, caller, tenant, runID); err != nil {
		return CheckoutGrant{}, ErrCheckoutDenied
	}
	run, access, err := s.resolve(ctx, tenant, runID)
	if err != nil || !eligibleTenantCheckoutRun(run, tenant, s.now()) || !accessMatchesRun(access, run) {
		return CheckoutGrant{}, ErrCheckoutDenied
	}
	token, expiresAt, err := s.mint(ctx, run, access)
	if err != nil || !validBrokerToken(token, expiresAt, s.now()) {
		return CheckoutGrant{}, ErrCheckoutDenied
	}
	// Fence rotation, disable, and Run cancellation that occurred while calling
	// GitHub. No token is returned unless the original identity is still live.
	if err = s.authorize(ctx, caller, tenant, runID); err != nil {
		return CheckoutGrant{}, ErrCheckoutDenied
	}
	current, currentAccess, err := s.resolve(ctx, tenant, runID)
	if err != nil || !eligibleTenantCheckoutRun(current, tenant, s.now()) || !sameCheckoutIdentity(run, current) || currentAccess != access {
		return CheckoutGrant{}, ErrCheckoutDenied
	}
	return CheckoutGrant{token: token, expiresAt: expiresAt}, nil
}

func eligibleTenantCheckoutRun(run domain.WorkloadRun, tenant domain.TenantID, now time.Time) bool {
	if run.ID == "" || run.TenantID != tenant || run.Kind != domain.WorkloadJob || run.GitHubIntegrationID == "" ||
		run.GitHubConfigRevision < 1 || run.GitHubCredentialGeneration < 1 || run.GitHubInstallation < 1 || run.GitHubRepository < 1 ||
		run.DesiredState != domain.WorkloadDesiredRunning || !checkoutCommitPattern.MatchString(run.CommitSHA) || run.TimeoutAt.IsZero() || !run.TimeoutAt.After(now) {
		return false
	}
	switch run.State {
	case domain.WorkloadPending, domain.WorkloadQueued, domain.WorkloadProvisioning, domain.WorkloadRunning:
		return true
	default:
		return false
	}
}

func accessMatchesRun(access githubapp.TenantPrivateKeyAccess, run domain.WorkloadRun) bool {
	return access.TenantID == string(run.TenantID) && access.IntegrationID == run.GitHubIntegrationID && access.GitHubAppID > 0 &&
		access.ConfigRevision == run.GitHubConfigRevision && access.CredentialGeneration == run.GitHubCredentialGeneration &&
		access.ReaderRoleARN != "" && access.SecretARN != "" && access.SecretVersionID != ""
}

func sameCheckoutIdentity(a, b domain.WorkloadRun) bool {
	return a.ID == b.ID && a.TenantID == b.TenantID && a.Kind == b.Kind && a.GitHubInstallation == b.GitHubInstallation &&
		a.GitHubRepository == b.GitHubRepository && a.GitHubIntegrationID == b.GitHubIntegrationID &&
		a.GitHubConfigRevision == b.GitHubConfigRevision && a.GitHubCredentialGeneration == b.GitHubCredentialGeneration && a.CommitSHA == b.CommitSHA
}

func validCheckoutRunID(id domain.WorkloadRunID) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range string(id) {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func validBrokerToken(value string, expiresAt, now time.Time) bool {
	if len(value) < 16 || len(value) > 4096 || !expiresAt.After(now) || expiresAt.After(now.Add(2*time.Hour)) {
		return false
	}
	return strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n\x00")
}
