package githubbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var ErrActionsRunnerBootstrapTokenDenied = errors.New("GitHub Actions runner bootstrap token denied")

type ActionsRunnerBootstrapTokenStarter func(context.Context, string, ActionsRunnerRegistrationRequest) (ActionsRunnerJobIdentity, error)

type ActionsRunnerBootstrapTokenService struct {
	allowed map[string]struct{}
	start   ActionsRunnerBootstrapTokenStarter
	fail    ActionsRunnerJobFailure
	mint    ActionsRunnerRegistrationTokenMinter
	clock   func() time.Time
}

type HTTPActionsRunnerBootstrapTokenClient struct {
	base   string
	client *http.Client
}

func NewHTTPActionsRunnerBootstrapTokenClient(baseURL string, client *http.Client) (*HTTPActionsRunnerBootstrapTokenClient, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || client == nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, ErrUnavailable
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPActionsRunnerBootstrapTokenClient{base: strings.TrimRight(base.String(), "/"), client: &copyClient}, nil
}

// Issue requests a single, claim-bound bootstrap credential over the
// Resource Plane's preconfigured mTLS transport. Request data contains no
// Tenant or Repository coordinates; the broker derives them from its ledger.
func (c *HTTPActionsRunnerBootstrapTokenClient) Issue(ctx context.Context, request ActionsRunnerRegistrationRequest) (ActionsRunnerRegistrationGrant, error) {
	if c == nil || c.client == nil || ctx == nil || !validActionsRunnerRegistrationRequest(request) || request.RunAttempt <= 0 {
		return ActionsRunnerRegistrationGrant{}, ErrActionsRunnerBootstrapTokenDenied
	}
	body, err := json.Marshal(request)
	if err != nil {
		return ActionsRunnerRegistrationGrant{}, ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/actions-runner/bootstrap-token", bytes.NewReader(body))
	if err != nil {
		return ActionsRunnerRegistrationGrant{}, ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-store")
	resp, err := c.client.Do(req)
	if err != nil {
		return ActionsRunnerRegistrationGrant{}, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > brokerMaxBody || resp.Header.Get("Cache-Control") != "no-store" {
		return ActionsRunnerRegistrationGrant{}, ErrActionsRunnerBootstrapTokenDenied
	}
	var decoded response
	decoder := json.NewDecoder(io.LimitReader(resp.Body, brokerMaxBody+1))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&decoded) != nil {
		return ActionsRunnerRegistrationGrant{}, ErrUnavailable
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || decoded.Token == "" || decoded.Repository == nil || decoded.ExpiresAt.IsZero() {
		return ActionsRunnerRegistrationGrant{}, ErrActionsRunnerBootstrapTokenDenied
	}
	token, err := githubapp.NewRunnerRegistrationToken(decoded.Token, decoded.ExpiresAt)
	if err != nil || !token.ExpiresAt().After(time.Now().UTC().Add(2*time.Minute)) || token.ExpiresAt().After(time.Now().UTC().Add(65*time.Minute)) ||
		!validActionsRunnerRepositoryPart(decoded.Repository.Owner) || !validActionsRunnerRepositoryPart(decoded.Repository.Name) {
		return ActionsRunnerRegistrationGrant{}, ErrActionsRunnerBootstrapTokenDenied
	}
	return ActionsRunnerRegistrationGrant{Token: token, Repository: githubapp.RepositoryAccess{Owner: decoded.Repository.Owner, Name: decoded.Repository.Name}}, nil
}

// IssueModelCredential resolves a model key only after the Lambda MicroVM is
// bound to the launch claim. The HTTP client uses the same preconfigured mTLS
// transport as the one-time registration-token broker.
func (c *HTTPActionsRunnerBootstrapTokenClient) IssueModelCredential(ctx context.Context, request ActionsRunnerRegistrationRequest, microVMID string) ([]byte, error) {
	if c == nil || c.client == nil {
		return nil, ErrActionsRunnerBootstrapTokenDenied
	}
	client, err := NewHTTPActionsRunnerModelCredentialClient(c.base, c.client)
	if err != nil {
		return nil, ErrActionsRunnerBootstrapTokenDenied
	}
	return client.IssueModelCredential(ctx, request, microVMID)
}

func validActionsRunnerRepositoryPart(value string) bool {
	if len(value) == 0 || len(value) > 100 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._-", char)) {
			return false
		}
	}
	return true
}

func NewActionsRunnerBootstrapTokenService(resourcePlaneIDs []string, start ActionsRunnerBootstrapTokenStarter,
	fail ActionsRunnerJobFailure, mint ActionsRunnerRegistrationTokenMinter) (*ActionsRunnerBootstrapTokenService, error) {
	if len(resourcePlaneIDs) == 0 || start == nil || fail == nil || mint == nil {
		return nil, ErrUnavailable
	}
	allowed := make(map[string]struct{}, len(resourcePlaneIDs))
	for _, planeID := range resourcePlaneIDs {
		caller, err := ResourcePlaneCallerID(planeID)
		if err != nil {
			return nil, ErrUnavailable
		}
		if _, duplicate := allowed[caller]; duplicate {
			return nil, ErrUnavailable
		}
		allowed[caller] = struct{}{}
	}
	return &ActionsRunnerBootstrapTokenService{allowed: allowed, start: start, fail: fail, mint: mint, clock: time.Now}, nil
}

// Issue exposes one repository-scoped runner token only after a Resource
// Plane has durably bound its MicroVM to the live claim. The database starter
// consumes registration_token_issued_at atomically, so retries cannot mint a
// second GitHub registration token for the same job attempt.
func (s *ActionsRunnerBootstrapTokenService) Issue(ctx context.Context, caller string, request ActionsRunnerRegistrationRequest) (ActionsRunnerRegistrationGrant, error) {
	if s == nil || ctx == nil || !validActionsRunnerRegistrationRequest(request) || request.RunAttempt <= 0 {
		return ActionsRunnerRegistrationGrant{}, ErrActionsRunnerBootstrapTokenDenied
	}
	planeID, err := resourcePlaneIDFromCaller(caller)
	if err != nil {
		return ActionsRunnerRegistrationGrant{}, ErrActionsRunnerBootstrapTokenDenied
	}
	if _, ok := s.allowed[caller]; !ok {
		return ActionsRunnerRegistrationGrant{}, ErrActionsRunnerBootstrapTokenDenied
	}
	identity, err := s.start(ctx, planeID, request)
	if err != nil || !validActionsRunnerIdentity(identity, request, s.clock()) || identity.ResourcePlaneID != planeID || identity.LambdaMicroVMID == "" {
		return ActionsRunnerRegistrationGrant{}, ErrActionsRunnerBootstrapTokenDenied
	}
	token, repository, err := s.mint(ctx, identity)
	if err != nil || token.Value() == "" || !token.ExpiresAt().After(s.clock().Add(2*time.Minute)) ||
		token.ExpiresAt().After(s.clock().Add(65*time.Minute)) || !validActionsRunnerRepositoryPart(repository.Owner) || !validActionsRunnerRepositoryPart(repository.Name) {
		_ = s.fail(ctx, identity)
		return ActionsRunnerRegistrationGrant{}, ErrActionsRunnerBootstrapTokenDenied
	}
	return ActionsRunnerRegistrationGrant{Token: token, Repository: repository}, nil
}

// NewPostgresActionsRunnerBootstrapTokenStarter atomically verifies the
// certificate-derived Plane, exact starting claim and bound MicroVM, then
// records the one-time token issuance before making the external GitHub call.
func NewPostgresActionsRunnerBootstrapTokenStarter(pool transaction.Beginner, policy transaction.Policy) ActionsRunnerBootstrapTokenStarter {
	return func(ctx context.Context, planeID string, request ActionsRunnerRegistrationRequest) (ActionsRunnerJobIdentity, error) {
		if pool == nil || ctx == nil || !resourcePlaneIDPattern.MatchString(planeID) || request.RunAttempt <= 0 || !validActionsRunnerRegistrationRequest(request) {
			return ActionsRunnerJobIdentity{}, ErrActionsRunnerBootstrapTokenDenied
		}
		now := time.Now().UTC()
		return transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (ActionsRunnerJobIdentity, error) {
			var identity ActionsRunnerJobIdentity
			var labelsJSON []byte
			var revision, generation int64
			err := tx.QueryRow(ctx, `SELECT j.integration_id,j.delivery_id,j.tenant_id,j.installation_id,j.repository_id,
				j.workflow_job_id,j.workflow_run_id,j.run_attempt,j.workflow_name,j.job_name,j.head_branch,j.head_sha,j.labels,
				j.claim_id,j.lease_expires_at,j.resource_plane_id,j.lambda_microvm_id,i.config_revision,i.credential_generation
				FROM github_actions_runner_jobs j
				JOIN github_webhook_deliveries d ON d.integration_id=j.integration_id AND d.delivery_id=j.delivery_id
				JOIN tenant_github_integrations i ON i.id=j.integration_id AND i.tenant_id=j.tenant_id
				JOIN tenant_github_app_installation_bindings b ON b.tenant_id=i.tenant_id AND b.integration_id=i.id AND b.installation_id=j.installation_id
				JOIN tenant_github_installations x ON x.tenant_id=b.tenant_id AND x.installation_id=b.installation_id
				JOIN tenant_github_repositories r ON r.tenant_id=j.tenant_id AND r.installation_id=j.installation_id AND r.github_repository_id=j.repository_id
				WHERE j.integration_id=$1 AND j.workflow_job_id=$2 AND j.run_attempt=$3 AND j.state='starting' AND j.claim_id=$4
				AND j.lease_expires_at>$5 AND j.resource_plane_id=$6 AND j.lambda_microvm_id IS NOT NULL AND j.registration_token_issued_at IS NULL
				AND d.processing_status='processed' AND d.credential_generation=i.credential_generation
				AND i.status='active' AND x.status='active' AND x.suspended_at IS NULL
				AND r.actions_runner_enabled=TRUE AND r.actions_runner_enabled_at IS NOT NULL AND r.actions_runner_enabled_at<=d.received_at`,
				request.IntegrationID, request.WorkflowJobID, request.RunAttempt, request.ClaimID, now, planeID).
				Scan(&identity.IntegrationID, &identity.DeliveryID, &identity.TenantID, &identity.Installation, &identity.Repository,
					&identity.WorkflowJobID, &identity.WorkflowRunID, &identity.RunAttempt, &identity.WorkflowName, &identity.JobName,
					&identity.HeadBranch, &identity.HeadSHA, &labelsJSON, &identity.ClaimID, &identity.LeaseExpires,
					&identity.ResourcePlaneID, &identity.LambdaMicroVMID, &revision, &generation)
			if err != nil {
				return ActionsRunnerJobIdentity{}, ErrActionsRunnerBootstrapTokenDenied
			}
			if err := json.Unmarshal(labelsJSON, &identity.Labels); err != nil {
				return ActionsRunnerJobIdentity{}, ErrActionsRunnerBootstrapTokenDenied
			}
			tag, err := tx.Exec(ctx, `UPDATE github_actions_runner_jobs SET registration_token_issued_at=$5,updated_at=$5
				WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 AND state='starting' AND claim_id=$4
				AND lease_expires_at>$5 AND resource_plane_id=$6 AND lambda_microvm_id=$7 AND registration_token_issued_at IS NULL`,
				request.IntegrationID, request.WorkflowJobID, request.RunAttempt, request.ClaimID, now, planeID, identity.LambdaMicroVMID)
			if err != nil || tag.RowsAffected() != 1 {
				return ActionsRunnerJobIdentity{}, ErrActionsRunnerBootstrapTokenDenied
			}
			credential, err := githubregistry.ResolveActiveAppCredential(ctx, tx, identity.TenantID, identity.IntegrationID, revision, generation)
			if err != nil || credential.GitHubAppID <= 0 || credential.ConfigRevision != revision || credential.CredentialGeneration != generation {
				return ActionsRunnerJobIdentity{}, ErrActionsRunnerBootstrapTokenDenied
			}
			identity.Credential = githubapp.TenantPrivateKeyAccess{TenantID: string(credential.TenantID), IntegrationID: credential.IntegrationID,
				GitHubAppID: credential.GitHubAppID, ConfigRevision: credential.ConfigRevision, CredentialGeneration: credential.CredentialGeneration,
				ReaderRoleARN: credential.PrivateKeyReaderRoleARN, SecretARN: credential.PrivateKeySecretARN, SecretVersionID: credential.PrivateKeyVersionID}
			return identity, nil
		})
	}
}
