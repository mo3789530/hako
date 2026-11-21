package githubbroker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/githubapp"
)

func TestActionsRunnerChecksServiceRequiresControlPlaneAndStableJobCredentialPin(t *testing.T) {
	now := time.Now().UTC()
	identity := ActionsRunnerCheckIdentity{IntegrationID: "integration_runner", TenantID: "tenant_runner", Installation: 77, Repository: 88,
		WorkflowJobID: 99, WorkflowRunID: 100, RunAttempt: 1, CommitSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Revision: 3,
		Credential: githubapp.TenantPrivateKeyAccess{TenantID: "tenant_runner", IntegrationID: "integration_runner", GitHubAppID: 12,
			ConfigRevision: 3, CredentialGeneration: 4, ReaderRoleARN: "reader-role", SecretARN: "secret-arn", SecretVersionID: "version-1"},
		RepositoryURL: githubapp.RepositoryAccess{Owner: "acme", Name: "api"}}
	request := ActionsRunnerChecksRequest{IntegrationID: identity.IntegrationID, WorkflowJobID: identity.WorkflowJobID, RunAttempt: identity.RunAttempt}
	resolveCalls, mintCalls := 0, 0
	service, err := NewActionsRunnerChecksService(func(_ context.Context, got ActionsRunnerChecksRequest) (ActionsRunnerCheckIdentity, error) {
		resolveCalls++
		if got != request {
			return ActionsRunnerCheckIdentity{}, ErrActionsRunnerChecksDenied
		}
		resolved := identity
		if resolveCalls > 1 {
			resolved.Credential.CredentialGeneration++
		}
		return resolved, nil
	}, func(_ context.Context, got ActionsRunnerCheckIdentity) (githubapp.CheckToken, githubapp.RepositoryAccess, error) {
		mintCalls++
		if got != identity {
			return nil, githubapp.RepositoryAccess{}, ErrActionsRunnerChecksDenied
		}
		return fakeBrokerChecksToken{value: "checks-token-synthetic", expiry: now.Add(time.Hour)}, identity.RepositoryURL, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	if _, err := service.Issue(context.Background(), "spiffe://hako/resource-plane/rp-tokyo-01", request); !errors.Is(err, ErrActionsRunnerChecksDenied) || resolveCalls != 0 || mintCalls != 0 {
		t.Fatalf("Resource Plane reached the CP-only Checks credential route: resolve=%d mint=%d err=%v", resolveCalls, mintCalls, err)
	}
	if _, err := service.Issue(context.Background(), controlPlaneURI, request); !errors.Is(err, ErrActionsRunnerChecksDenied) || resolveCalls != 2 || mintCalls != 1 {
		t.Fatalf("credential rotation during issuance was not fenced: resolve=%d mint=%d err=%v", resolveCalls, mintCalls, err)
	}
}
