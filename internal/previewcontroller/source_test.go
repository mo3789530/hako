package previewcontroller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/previewprotocol"
	"github.com/mo3789530/hako/internal/repositorycheckout"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

type previewSourceCredentialsFake struct {
	token      string
	repository githubapp.RepositoryAccess
	err        error
	calls      int
}

func (f *previewSourceCredentialsFake) CheckoutPreview(_ context.Context, command previewprotocol.Command) (string, githubapp.RepositoryAccess, error) {
	f.calls++
	if command.Action != previewprotocol.ActionProvision {
		return "", githubapp.RepositoryAccess{}, ErrPreviewSourceUnavailable
	}
	return f.token, f.repository, f.err
}

type previewRepositoryCheckoutFake struct {
	path       string
	cleanup    func() error
	token      string
	repository githubapp.RepositoryAccess
	command    previewprotocol.Command
	err        error
	calls      int
}

func (f *previewRepositoryCheckoutFake) Checkout(_ context.Context, command previewprotocol.Command, token string, repository githubapp.RepositoryAccess) (PreviewCheckedOutRepository, error) {
	f.calls++
	f.command, f.token, f.repository = command, token, repository
	return PreviewCheckedOutRepository{Path: f.path, Cleanup: f.cleanup}, f.err
}

type previewSourceStagerFake struct {
	source workloadsource.MaterializedSource
	err    error
	path   string
	id     workloadjournal.Command
	calls  int
}

func (f *previewSourceStagerFake) StageCheckout(_ context.Context, id workloadjournal.Command, path string) (workloadsource.MaterializedSource, error) {
	f.calls++
	f.id, f.path = id, path
	return f.source, f.err
}

func TestPreviewSourceMaterializerStagesPinnedGenerationAndCleansCheckout(t *testing.T) {
	now := time.Date(2026, 10, 25, 5, 0, 0, 0, time.UTC)
	command := validCommand(now, previewprotocol.ActionProvision)
	checkoutPath := t.TempDir()
	checkoutDir := filepath.Join(checkoutPath, "repo")
	if err := os.Mkdir(checkoutDir, 0700); err != nil {
		t.Fatal(err)
	}
	cleanupCalled := false
	credentials := &previewSourceCredentialsFake{token: "ghs-preview-token-for-checkout",
		repository: githubapp.RepositoryAccess{Owner: "preview-owner", Name: "preview-repo"}}
	checkout := &previewRepositoryCheckoutFake{path: checkoutDir, cleanup: func() error {
		cleanupCalled = true
		return os.RemoveAll(checkoutDir)
	}}
	stager := &previewSourceStagerFake{source: workloadsource.MaterializedSource{Digest: strings.Repeat("b", 64),
		ArtifactPrefix: "s3://hako-source/previews/tenant_1/prvws_0123456789abcdef0123456789abcdef/preview-claim-1234/"}}
	materializer, err := NewPreviewSourceMaterializer(credentials, checkout, stager)
	if err != nil {
		t.Fatal(err)
	}
	source, err := materializer.Materialize(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	wantIdentity := workloadjournal.Command{TenantID: command.TenantID, WorkloadRunID: command.RuntimeWorkspaceID,
		AttemptID: command.ClaimID, Generation: command.Generation, ResourcePlaneID: command.ResourcePlaneID}
	if source != stager.source || credentials.calls != 1 || checkout.calls != 1 || stager.calls != 1 || !cleanupCalled ||
		checkout.token != credentials.token || checkout.repository != credentials.repository || checkout.command != command ||
		stager.id != wantIdentity || stager.path != checkoutDir {
		t.Fatalf("Preview source pipeline did not preserve its generation pin: source=%+v credential=%+v checkout=%+v stage=%+v cleaned=%v",
			source, credentials, checkout, stager, cleanupCalled)
	}
	if _, err := os.Stat(checkoutDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary Git checkout still exists after staging: %v", err)
	}
}

func TestPreviewSourceMaterializerFailsClosedAndCleansPartialCheckout(t *testing.T) {
	now := time.Date(2026, 10, 25, 5, 0, 0, 0, time.UTC)
	command := validCommand(now, previewprotocol.ActionProvision)
	tests := []struct {
		name        string
		credentials *previewSourceCredentialsFake
		checkoutErr error
		stageErr    error
		source      workloadsource.MaterializedSource
		wantStage   bool
	}{
		{name: "credential unavailable", credentials: &previewSourceCredentialsFake{err: ErrPreviewSourceUnavailable}},
		{name: "empty token", credentials: &previewSourceCredentialsFake{repository: githubapp.RepositoryAccess{Owner: "acme", Name: "api"}}},
		{name: "invalid repository", credentials: &previewSourceCredentialsFake{token: "ghs-preview-token-for-checkout", repository: githubapp.RepositoryAccess{Owner: "../acme", Name: "api"}}},
		{name: "checkout failed", credentials: &previewSourceCredentialsFake{token: "ghs-preview-token-for-checkout", repository: githubapp.RepositoryAccess{Owner: "acme", Name: "api"}}, checkoutErr: ErrPreviewSourceUnavailable},
		{name: "staging failed", credentials: &previewSourceCredentialsFake{token: "ghs-preview-token-for-checkout", repository: githubapp.RepositoryAccess{Owner: "acme", Name: "api"}}, stageErr: ErrPreviewSourceUnavailable, wantStage: true},
		{name: "invalid digest", credentials: &previewSourceCredentialsFake{token: "ghs-preview-token-for-checkout", repository: githubapp.RepositoryAccess{Owner: "acme", Name: "api"}}, source: workloadsource.MaterializedSource{Digest: "bad", ArtifactPrefix: "s3://bucket/previews/tenant_1/prvws_0123456789abcdef0123456789abcdef/preview-claim-1234/"}, wantStage: true},
		{name: "foreign attempt prefix", credentials: &previewSourceCredentialsFake{token: "ghs-preview-token-for-checkout", repository: githubapp.RepositoryAccess{Owner: "acme", Name: "api"}}, source: workloadsource.MaterializedSource{Digest: strings.Repeat("c", 64), ArtifactPrefix: "s3://bucket/previews/tenant_1/prvws_0123456789abcdef0123456789abcdef/claim_foreign/"}, wantStage: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checkoutDir := filepath.Join(t.TempDir(), "checkout")
			if err := os.Mkdir(checkoutDir, 0700); err != nil {
				t.Fatal(err)
			}
			cleanupCalled := false
			checkout := &previewRepositoryCheckoutFake{path: checkoutDir, err: test.checkoutErr, cleanup: func() error {
				cleanupCalled = true
				return os.RemoveAll(checkoutDir)
			}}
			stager := &previewSourceStagerFake{source: test.source, err: test.stageErr}
			materializer, err := NewPreviewSourceMaterializer(test.credentials, checkout, stager)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := materializer.Materialize(context.Background(), command); !errors.Is(err, ErrPreviewSourceUnavailable) {
				t.Fatalf("unsafe source materialization did not fail closed: %v", err)
			}
			if (stager.calls == 1) != test.wantStage || cleanupCalled != (checkout.calls > 0) {
				t.Fatalf("unexpected partial source pipeline: checkout=%d stage=%d cleanup=%v", checkout.calls, stager.calls, cleanupCalled)
			}
			if cleanupCalled {
				if _, err := os.Stat(checkoutDir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("temporary checkout was not removed after failed stage: %v", err)
				}
			}
		})
	}
}

func TestPreviewSourceMaterializerRejectsCleanupAndNonProvisionCommands(t *testing.T) {
	now := time.Date(2026, 10, 25, 5, 0, 0, 0, time.UTC)
	credentials := &previewSourceCredentialsFake{token: "ghs-preview-token-for-checkout", repository: githubapp.RepositoryAccess{Owner: "acme", Name: "api"}}
	checkout := &previewRepositoryCheckoutFake{}
	stager := &previewSourceStagerFake{}
	materializer, err := NewPreviewSourceMaterializer(credentials, checkout, stager)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := validCommand(now, previewprotocol.ActionCleanup)
	if _, err := materializer.Materialize(context.Background(), cleanup); !errors.Is(err, ErrPreviewSourceUnavailable) || credentials.calls != 0 {
		t.Fatalf("cleanup attempted source checkout: err=%v credential calls=%d", err, credentials.calls)
	}
	invalid := validCommand(now, previewprotocol.ActionProvision)
	invalid.HeadSHA = strings.Repeat("z", 40)
	if _, err := materializer.Materialize(context.Background(), invalid); !errors.Is(err, ErrPreviewSourceUnavailable) || credentials.calls != 0 {
		t.Fatalf("invalid source pin attempted broker request: err=%v credential calls=%d", err, credentials.calls)
	}
}

func TestPreviewRunCredentialRequiresExactImmutablePin(t *testing.T) {
	command := validCommand(time.Date(2026, 10, 25, 5, 0, 0, 0, time.UTC), previewprotocol.ActionProvision)
	command.HeadSHA = strings.ToUpper(command.HeadSHA)
	identity := previewRun(command)
	credential := previewRunCredential{identity: identity, token: "ghs-preview-token-for-checkout"}
	got, err := credential.CheckoutRunToken(context.Background(), identity)
	if err != nil || got != credential.token {
		t.Fatalf("exact pinned Run credential = %q, %v", got, err)
	}
	tests := []struct {
		name   string
		mutate func(*domain.WorkloadRun)
	}{
		{name: "run id", mutate: func(run *domain.WorkloadRun) { run.ID += "-foreign" }},
		{name: "tenant", mutate: func(run *domain.WorkloadRun) { run.TenantID = "tenant_foreign" }},
		{name: "installation", mutate: func(run *domain.WorkloadRun) { run.GitHubInstallation++ }},
		{name: "repository", mutate: func(run *domain.WorkloadRun) { run.GitHubRepository++ }},
		{name: "commit", mutate: func(run *domain.WorkloadRun) { run.CommitSHA = strings.Repeat("b", 40) }},
		{name: "integration", mutate: func(run *domain.WorkloadRun) { run.GitHubIntegrationID = "integration_foreign" }},
		{name: "config revision", mutate: func(run *domain.WorkloadRun) { run.GitHubConfigRevision++ }},
		{name: "credential generation", mutate: func(run *domain.WorkloadRun) { run.GitHubCredentialGeneration++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			foreign := identity
			test.mutate(&foreign)
			if token, err := credential.CheckoutRunToken(context.Background(), foreign); !errors.Is(err, repositorycheckout.ErrCheckoutCredentialFailure) || token != "" {
				t.Fatalf("foreign immutable Run pin received credential %q: %v", token, err)
			}
		})
	}
	empty := previewRunCredential{identity: identity}
	if token, err := empty.CheckoutRunToken(context.Background(), identity); !errors.Is(err, repositorycheckout.ErrCheckoutCredentialFailure) || token != "" {
		t.Fatalf("empty credential returned token %q: %v", token, err)
	}
}

func TestPreviewRepositoryAuthorizerRequiresExactTenantAndRepository(t *testing.T) {
	command := validCommand(time.Now().UTC(), previewprotocol.ActionProvision)
	identity := previewRun(command)
	repository := githubapp.RepositoryAccess{Owner: "preview-owner", Name: "preview_repo.v1"}
	authorizer := previewRepositoryAuthorizer{identity: identity, repository: repository}
	got, err := authorizer.AuthorizeRepository(context.Background(), identity.TenantID, identity.GitHubInstallation, identity.GitHubRepository)
	if err != nil || got != repository {
		t.Fatalf("exact Repository authorization = %+v, %v", got, err)
	}
	for _, test := range []struct {
		name           string
		tenant         domain.TenantID
		installationID int64
		repositoryID   int64
	}{
		{name: "foreign tenant", tenant: "tenant_foreign", installationID: identity.GitHubInstallation, repositoryID: identity.GitHubRepository},
		{name: "foreign installation", tenant: identity.TenantID, installationID: identity.GitHubInstallation + 1, repositoryID: identity.GitHubRepository},
		{name: "foreign repository", tenant: identity.TenantID, installationID: identity.GitHubInstallation, repositoryID: identity.GitHubRepository + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := authorizer.AuthorizeRepository(context.Background(), test.tenant, test.installationID, test.repositoryID)
			if !errors.Is(err, repositorycheckout.ErrCheckoutNotAuthorized) {
				t.Fatalf("foreign Repository identity error = %v", err)
			}
		})
	}
	for _, invalid := range []githubapp.RepositoryAccess{
		{}, {Owner: "-preview", Name: "repo"}, {Owner: "preview/owner", Name: "repo"},
		{Owner: "preview-owner", Name: "../repo"}, {Owner: "preview-owner", Name: strings.Repeat("r", 101)},
	} {
		invalidAuthorizer := previewRepositoryAuthorizer{identity: identity, repository: invalid}
		if _, err := invalidAuthorizer.AuthorizeRepository(context.Background(), identity.TenantID, identity.GitHubInstallation, identity.GitHubRepository); !errors.Is(err, repositorycheckout.ErrCheckoutNotAuthorized) {
			t.Errorf("invalid Repository path %+v was authorized: %v", invalid, err)
		}
	}
}

func TestGitPreviewRepositoryCheckoutRejectsInvalidCommitBeforeNetwork(t *testing.T) {
	command := validCommand(time.Now().UTC(), previewprotocol.ActionProvision)
	command.HeadSHA = strings.Repeat("z", 40)
	_, err := (GitPreviewRepositoryCheckout{}).Checkout(context.Background(), command,
		"ghs-preview-token-for-checkout", githubapp.RepositoryAccess{Owner: "preview-owner", Name: "preview-repo"})
	if !errors.Is(err, ErrPreviewSourceUnavailable) {
		t.Fatalf("invalid pinned commit checkout error = %v", err)
	}
}
