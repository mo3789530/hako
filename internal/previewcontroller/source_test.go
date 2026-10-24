package previewcontroller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/previewprotocol"
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
