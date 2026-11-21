package workloadcontroller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/repositorycheckout"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

var ErrTenantCheckoutRequired = errors.New("Tenant-owned Workload Run requires its pinned checkout broker")

// SourceArchiveStager turns a trusted checkout into an attempt-scoped source
// object that the MicroVM Agent can consume. Implementations must be
// idempotent by identity and content digest and must not run repository code.
type SourceArchiveStager interface {
	StageCheckout(context.Context, workloadjournal.Command, string) (MaterializedSource, error)
}

type AgentProposalArchiveReader interface {
	ReadAgentProposalArchive(context.Context, workloadsource.AgentProposalArchiveIdentity) ([]byte, error)
}

// RepositorySourceMaterializer connects the immutable Control Plane metadata
// to the existing credential-brokered repository checkout package, then hands
// the verified tree to the attempt-scoped archive stager.
type RepositorySourceMaterializer struct {
	checkout repositorycheckout.RepositoryCheckout
	stager   SourceArchiveStager
	now      func() time.Time
}

func NewRepositorySourceMaterializer(checkout repositorycheckout.RepositoryCheckout, stager SourceArchiveStager, now func() time.Time) (*RepositorySourceMaterializer, error) {
	if checkout == nil || stager == nil {
		return nil, ErrExecutionMaterialization
	}
	if now == nil {
		now = time.Now
	}
	return &RepositorySourceMaterializer{checkout: checkout, stager: stager, now: now}, nil
}

func (m *RepositorySourceMaterializer) Materialize(ctx context.Context, metadata ControlPlaneExecutionMetadata) (MaterializedSource, error) {
	if m == nil {
		return MaterializedSource{}, ErrExecutionMetadata
	}
	return materializeRepositorySource(ctx, metadata, m.checkout, m.stager, m.now)
}

// NewTenantBrokeredRepositorySourceMaterializer composes immutable checkout
// for Tenant-owned Runs. Repository identity is authorized from the signed
// Control Plane metadata, while the short-lived installation token is resolved
// by a separate mTLS broker using only the persisted Tenant/Run pin. The
// legacy shared-token provider is deliberately an error, so an unsupported or
// missing Tenant pin cannot fall back to shared credentials.
func NewTenantBrokeredRepositorySourceMaterializer(credentials repositorycheckout.RunCredentialProvider,
	stager SourceArchiveStager, now func() time.Time) (*TenantBrokeredRepositorySourceMaterializer, error) {
	return NewTenantBrokeredRepositorySourceMaterializerWithProposalReader(credentials, stager, nil, now)
}

func NewTenantBrokeredRepositorySourceMaterializerWithProposalReader(credentials repositorycheckout.RunCredentialProvider,
	stager SourceArchiveStager, proposals AgentProposalArchiveReader, now func() time.Time) (*TenantBrokeredRepositorySourceMaterializer, error) {
	if credentials == nil || stager == nil {
		return nil, ErrExecutionMaterialization
	}
	if now == nil {
		now = time.Now
	}
	return &TenantBrokeredRepositorySourceMaterializer{credentials: credentials, stager: stager, proposals: proposals, now: now}, nil
}

type TenantBrokeredRepositorySourceMaterializer struct {
	credentials repositorycheckout.RunCredentialProvider
	stager      SourceArchiveStager
	proposals   AgentProposalArchiveReader
	now         func() time.Time
}

func (m *TenantBrokeredRepositorySourceMaterializer) Materialize(ctx context.Context, metadata ControlPlaneExecutionMetadata) (MaterializedSource, error) {
	if m == nil || m.credentials == nil || metadata.GitHubIntegrationID == "" || metadata.GitHubConfigRevision < 1 || metadata.GitHubCredentialGen < 1 {
		return MaterializedSource{}, ErrTenantCheckoutRequired
	}
	if metadata.ProposalVerification != nil && m.proposals == nil {
		return MaterializedSource{}, ErrExecutionMaterialization
	}
	if !validExecutionSourceMetadata(ctx, metadata, m.now) {
		return MaterializedSource{}, ErrExecutionMetadata
	}
	authorizer := metadataRepositoryAuthorizer{tenantID: metadata.TenantID, installationID: metadata.GitHubInstallationID,
		repositoryID: metadata.GitHubRepositoryID, owner: metadata.GitHubOwner, repository: metadata.GitHubRepository}
	legacyCredentials := repositorycheckout.CredentialProviderFunc(func(context.Context, domain.TenantID, int64, int64) (string, error) {
		return "", ErrTenantCheckoutRequired
	})
	checkout, err := repositorycheckout.NewGitRepositoryCheckoutWithRunCredentials(authorizer, legacyCredentials, m.credentials)
	if err != nil {
		return MaterializedSource{}, ErrExecutionMaterialization
	}
	return materializeRepositorySourceWithProposal(ctx, metadata, checkout, m.stager, m.proposals, m.now)
}

type metadataRepositoryAuthorizer struct {
	tenantID       domain.TenantID
	installationID int64
	repositoryID   int64
	owner          string
	repository     string
}

func (a metadataRepositoryAuthorizer) AuthorizeRepository(_ context.Context, tenant domain.TenantID, installationID, repositoryID int64) (githubapp.RepositoryAccess, error) {
	if tenant == "" || tenant != a.tenantID || installationID != a.installationID || repositoryID != a.repositoryID ||
		a.owner == "" || a.repository == "" {
		return githubapp.RepositoryAccess{}, ErrTenantCheckoutRequired
	}
	return githubapp.RepositoryAccess{Owner: a.owner, Name: a.repository}, nil
}

func materializeRepositorySource(ctx context.Context, metadata ControlPlaneExecutionMetadata, checkout repositorycheckout.RepositoryCheckout,
	stager SourceArchiveStager, now func() time.Time) (MaterializedSource, error) {
	return materializeRepositorySourceWithProposal(ctx, metadata, checkout, stager, nil, now)
}

func materializeRepositorySourceWithProposal(ctx context.Context, metadata ControlPlaneExecutionMetadata, checkout repositorycheckout.RepositoryCheckout,
	stager SourceArchiveStager, proposals AgentProposalArchiveReader, now func() time.Time) (MaterializedSource, error) {
	if ctx == nil || checkout == nil || stager == nil || now == nil || !validExecutionSourceMetadata(ctx, metadata, now) {
		return MaterializedSource{}, ErrExecutionMetadata
	}
	if metadata.ProposalVerification != nil && proposals == nil {
		return MaterializedSource{}, ErrExecutionMaterialization
	}
	interval := metadata.Deadline.Sub(metadata.RequestedAt)
	run := domain.WorkloadRun{ID: metadata.WorkloadRunID, TenantID: metadata.TenantID, Kind: effectiveWorkloadKind(metadata),
		GitHubInstallation: metadata.GitHubInstallationID, GitHubRepository: metadata.GitHubRepositoryID,
		GitHubIntegrationID: metadata.GitHubIntegrationID, GitHubConfigRevision: metadata.GitHubConfigRevision,
		GitHubCredentialGeneration: metadata.GitHubCredentialGen, CommitSHA: metadata.CommitSHA, Ref: metadata.Ref,
		RuntimeClass: metadata.RuntimeClass, TrustLevel: metadata.TrustLevel, DesiredState: domain.WorkloadDesiredRunning,
		State: domain.WorkloadProvisioning, ResourcePlaneID: metadata.ResourcePlaneID, TimeoutSeconds: int64(interval / time.Second),
		TimeoutAt: metadata.Deadline.UTC(), CreatedAt: metadata.RequestedAt.UTC()}
	checkedOut, err := checkout.Checkout(ctx, run)
	if err != nil {
		return MaterializedSource{}, ErrExecutionMaterialization
	}
	if checkedOut.Path == "" {
		_ = checkedOut.Cleanup()
		return MaterializedSource{}, ErrExecutionMaterialization
	}
	checkoutPath := checkedOut.Path
	var cleanupOverlay func()
	if verification := metadata.ProposalVerification; verification != nil {
		proposalIdentity := workloadsource.AgentProposalArchiveIdentity{TenantID: string(metadata.TenantID),
			WorkloadRunID: string(verification.AgentRunID), AttemptID: verification.AgentAttemptID, SHA256: verification.ProposalSHA256}
		proposalBytes, readErr := proposals.ReadAgentProposalArchive(ctx, proposalIdentity)
		if readErr != nil || len(proposalBytes) < 1 || len(proposalBytes) > workloadsource.MaxAgentProposalArchiveBytes {
			clear(proposalBytes)
			_ = checkedOut.Cleanup()
			return MaterializedSource{}, ErrExecutionMaterialization
		}
		defer clear(proposalBytes)
		baseArchive, archiveErr := workloadsource.CreateArchive(ctx, checkedOut.Path, workloadsource.DefaultMaxArchiveBytes)
		if archiveErr != nil {
			_ = checkedOut.Cleanup()
			return MaterializedSource{}, ErrExecutionMaterialization
		}
		proposalSource, applyErr := workloadsource.ApplyAgentProposalArchive(ctx, baseArchive.Reader(), baseArchive.Size(),
			baseArchive.SHA256(), proposalBytes, workloadsource.DefaultMaxArchiveBytes)
		baseCloseErr := baseArchive.Close()
		if applyErr != nil || baseCloseErr != nil || proposalSource.Archive == nil ||
			proposalSource.ProposalSHA256 != verification.ProposalSHA256 || verification.BaseCommitSHA != metadata.CommitSHA {
			if proposalSource.Archive != nil {
				_ = proposalSource.Archive.Close()
			}
			_ = checkedOut.Cleanup()
			return MaterializedSource{}, ErrExecutionMaterialization
		}
		overlayRoot, tempErr := os.MkdirTemp("", "hako-verifier-source-*")
		if tempErr != nil {
			_ = proposalSource.Archive.Close()
			_ = checkedOut.Cleanup()
			return MaterializedSource{}, ErrExecutionMaterialization
		}
		cleanupOverlay = func() { _ = os.RemoveAll(overlayRoot) }
		checkoutPath = filepath.Join(overlayRoot, "checkout")
		if workloadsource.ExtractArchive(ctx, proposalSource.Archive.Reader(), proposalSource.Archive.Size(),
			proposalSource.Archive.SHA256(), checkoutPath, workloadsource.DefaultMaxArchiveBytes) != nil {
			_ = proposalSource.Archive.Close()
			cleanupOverlay()
			_ = checkedOut.Cleanup()
			return MaterializedSource{}, ErrExecutionMaterialization
		}
		if proposalSource.Archive.Close() != nil {
			cleanupOverlay()
			_ = checkedOut.Cleanup()
			return MaterializedSource{}, ErrExecutionMaterialization
		}
	}
	if cleanupOverlay != nil {
		defer cleanupOverlay()
	}
	source, stageErr := stager.StageCheckout(ctx, workloadjournal.Command{TenantID: string(metadata.TenantID), WorkloadRunID: string(metadata.WorkloadRunID),
		AttemptID: metadata.AttemptID, Generation: metadata.Generation, ResourcePlaneID: string(metadata.ResourcePlaneID)}, checkoutPath)
	cleanupErr := checkedOut.Cleanup()
	if stageErr != nil || cleanupErr != nil || !digestPattern.MatchString(source.Digest) ||
		!validArtifactPrefix(source.ArtifactPrefix, workloadjournal.Command{TenantID: string(metadata.TenantID), WorkloadRunID: string(metadata.WorkloadRunID),
			AttemptID: metadata.AttemptID, Generation: metadata.Generation, ResourcePlaneID: string(metadata.ResourcePlaneID)}) {
		return MaterializedSource{}, ErrExecutionMaterialization
	}
	return source, nil
}

func validExecutionSourceMetadata(ctx context.Context, metadata ControlPlaneExecutionMetadata, now func() time.Time) bool {
	if ctx == nil || ctx.Err() != nil || now == nil || !validExecutionMetadata(metadata, now().UTC()) ||
		!metadataIDPattern.MatchString(string(metadata.TenantID)) || !metadataIDPattern.MatchString(string(metadata.WorkloadRunID)) ||
		!metadataIDPattern.MatchString(metadata.AttemptID) || !metadataIDPattern.MatchString(string(metadata.ResourcePlaneID)) || metadata.Generation < 1 {
		return false
	}
	interval := metadata.Deadline.Sub(metadata.RequestedAt)
	return interval >= time.Second && interval <= 24*time.Hour && interval%time.Second == 0
}
