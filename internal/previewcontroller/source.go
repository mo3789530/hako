package previewcontroller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/previewprotocol"
	"github.com/mo3789530/hako/internal/repositorycheckout"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

var ErrPreviewSourceUnavailable = errors.New("pinned Preview source is unavailable")

type PreviewSourceCredentialProvider interface {
	CheckoutPreview(context.Context, previewprotocol.Command) (string, githubapp.RepositoryAccess, error)
}

type PreviewRepositoryCheckout interface {
	Checkout(context.Context, previewprotocol.Command, string, githubapp.RepositoryAccess) (PreviewCheckedOutRepository, error)
}

type PreviewCheckedOutRepository struct {
	Path    string
	Cleanup func() error
}

type PreviewSourceArchiveStager interface {
	StageCheckout(context.Context, workloadjournal.Command, string) (workloadsource.MaterializedSource, error)
}

type PreviewSourceMaterializer struct {
	credentials PreviewSourceCredentialProvider
	checkout    PreviewRepositoryCheckout
	stager      PreviewSourceArchiveStager
}

func NewPreviewSourceMaterializer(credentials PreviewSourceCredentialProvider, checkout PreviewRepositoryCheckout, stager PreviewSourceArchiveStager) (*PreviewSourceMaterializer, error) {
	if credentials == nil || checkout == nil || stager == nil {
		return nil, ErrPreviewSourceUnavailable
	}
	return &PreviewSourceMaterializer{credentials: credentials, checkout: checkout, stager: stager}, nil
}

// Materialize fetches only the PR generation's pinned head, stages a
// credential-free archive under the generation/claim identity, and removes
// the temporary Git checkout before returning. Repository code is never run.
func (m *PreviewSourceMaterializer) Materialize(ctx context.Context, command previewprotocol.Command) (workloadsource.MaterializedSource, error) {
	if m == nil || ctx == nil || ctx.Err() != nil || command.Validate(previewprotocol.EventType(command.Action), command.RuntimeWorkspaceID) != nil ||
		command.Action != previewprotocol.ActionProvision {
		return workloadsource.MaterializedSource{}, ErrPreviewSourceUnavailable
	}
	token, repository, err := m.credentials.CheckoutPreview(ctx, command)
	if err != nil || !validPreviewCheckoutToken(token) || !validPreviewRepository(repository) {
		return workloadsource.MaterializedSource{}, ErrPreviewSourceUnavailable
	}
	checkedOut, err := m.checkout.Checkout(ctx, command, token, repository)
	if err != nil || strings.TrimSpace(checkedOut.Path) == "" {
		if checkedOut.Cleanup != nil {
			_ = checkedOut.Cleanup()
		}
		return workloadsource.MaterializedSource{}, ErrPreviewSourceUnavailable
	}
	identity := workloadjournal.Command{TenantID: command.TenantID, WorkloadRunID: command.RuntimeWorkspaceID,
		AttemptID: command.ClaimID, Generation: command.Generation, ResourcePlaneID: command.ResourcePlaneID}
	source, stageErr := m.stager.StageCheckout(ctx, identity, checkedOut.Path)
	var cleanupErr error
	if checkedOut.Cleanup != nil {
		cleanupErr = checkedOut.Cleanup()
	} else {
		cleanupErr = ErrPreviewSourceUnavailable
	}
	if stageErr != nil || cleanupErr != nil || !validPreviewSource(source, identity) {
		return workloadsource.MaterializedSource{}, ErrPreviewSourceUnavailable
	}
	return source, nil
}

// GitPreviewRepositoryCheckout adapts the common immutable SHA checkout code
// to a one-claim Preview token. The credential callback refuses any synthetic
// checkout identity that differs from the current Preview command.
type GitPreviewRepositoryCheckout struct{}

func (GitPreviewRepositoryCheckout) Checkout(ctx context.Context, command previewprotocol.Command, token string, repository githubapp.RepositoryAccess) (PreviewCheckedOutRepository, error) {
	identity := previewRun(command)
	authorizer := previewRepositoryAuthorizer{identity: identity, repository: repository}
	legacy := repositorycheckout.CredentialProviderFunc(func(context.Context, domain.TenantID, int64, int64) (string, error) {
		return "", repositorycheckout.ErrCheckoutCredentialFailure
	})
	runCredentials := previewRunCredential{identity: identity, token: token}
	checkout, err := repositorycheckout.NewGitRepositoryCheckoutWithRunCredentials(authorizer, legacy, runCredentials)
	if err != nil {
		return PreviewCheckedOutRepository{}, ErrPreviewSourceUnavailable
	}
	checkedOut, err := checkout.Checkout(ctx, identity)
	if err != nil {
		return PreviewCheckedOutRepository{}, ErrPreviewSourceUnavailable
	}
	return PreviewCheckedOutRepository{Path: checkedOut.Path, Cleanup: checkedOut.Cleanup}, nil
}

type previewRunCredential struct {
	identity domain.WorkloadRun
	token    string
}

func (c previewRunCredential) CheckoutRunToken(_ context.Context, run domain.WorkloadRun) (string, error) {
	if c.token == "" || run.ID != c.identity.ID || run.TenantID != c.identity.TenantID || run.GitHubInstallation != c.identity.GitHubInstallation ||
		run.GitHubRepository != c.identity.GitHubRepository || run.CommitSHA != c.identity.CommitSHA || run.GitHubIntegrationID != c.identity.GitHubIntegrationID ||
		run.GitHubConfigRevision != c.identity.GitHubConfigRevision || run.GitHubCredentialGeneration != c.identity.GitHubCredentialGeneration {
		return "", repositorycheckout.ErrCheckoutCredentialFailure
	}
	return c.token, nil
}

type previewRepositoryAuthorizer struct {
	identity   domain.WorkloadRun
	repository githubapp.RepositoryAccess
}

func (a previewRepositoryAuthorizer) AuthorizeRepository(_ context.Context, tenant domain.TenantID, installationID, repositoryID int64) (githubapp.RepositoryAccess, error) {
	if tenant != a.identity.TenantID || installationID != a.identity.GitHubInstallation || repositoryID != a.identity.GitHubRepository ||
		!validPreviewRepository(a.repository) {
		return githubapp.RepositoryAccess{}, repositorycheckout.ErrCheckoutNotAuthorized
	}
	return a.repository, nil
}

func previewRun(command previewprotocol.Command) domain.WorkloadRun {
	return domain.WorkloadRun{ID: domain.WorkloadRunID(command.RuntimeWorkspaceID), TenantID: domain.TenantID(command.TenantID), Kind: domain.WorkloadJob,
		GitHubInstallation: command.InstallationID, GitHubRepository: command.RepositoryID, GitHubIntegrationID: "preview-generation-pinned",
		GitHubConfigRevision: 1, GitHubCredentialGeneration: 1, CommitSHA: strings.ToLower(command.HeadSHA),
		DesiredState: domain.WorkloadDesiredRunning, State: domain.WorkloadProvisioning, CreatedAt: time.Now().UTC(), TimeoutAt: command.LeaseExpiresAt.UTC()}
}

func validPreviewCheckoutToken(token string) bool {
	return len(token) >= 16 && len(token) <= 4096 && strings.TrimSpace(token) == token && !strings.ContainsAny(token, "\r\n\x00")
}

func validPreviewRepository(repository githubapp.RepositoryAccess) bool {
	return validPreviewRepositoryPart(repository.Owner, 39) && validPreviewRepositoryPart(repository.Name, 100)
}

func validPreviewRepositoryPart(value string, max int) bool {
	if value == "" || len(value) > max || value == "." || value == ".." || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || (max == 100 && (char == '_' || char == '.'))) {
			return false
		}
	}
	return true
}

func validPreviewSource(source workloadsource.MaterializedSource, identity workloadjournal.Command) bool {
	if len(source.Digest) != 64 {
		return false
	}
	for _, char := range source.Digest {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	parsed, err := url.Parse(source.ArtifactPrefix)
	if err != nil || parsed.Scheme != "s3" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		!strings.HasSuffix(parsed.Path, fmt.Sprintf("/%s/%s/%s/", identity.TenantID, identity.WorkloadRunID, identity.AttemptID)) {
		return false
	}
	for _, part := range strings.Split(parsed.Path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

var _ githubapp.RepositoryAuthorizer = previewRepositoryAuthorizer{}
var _ repositorycheckout.RunCredentialProvider = previewRunCredential{}
