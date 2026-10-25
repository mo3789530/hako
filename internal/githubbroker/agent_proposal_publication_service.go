package githubbroker

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/workloadsource"
)

var ErrAgentProposalPublicationDenied = errors.New("Agent proposal publication is not authorized or could not be completed")

type AgentProposalPublicationIdentity struct {
	Run         domain.WorkloadRun
	Publication workloads.AgentProposalPublication
	Evidence    workloads.AgentProposalPublicationEvidence
	Archive     workloadsource.AgentProposalArchiveIdentity
	AppAccess   githubapp.TenantPrivateKeyAccess
}

type AgentProposalPublicationResolver func(context.Context, domain.TenantID, domain.WorkloadRunID, string) (AgentProposalPublicationIdentity, error)
type AgentProposalPublicationRecorder func(context.Context, AgentProposalPublicationIdentity, AgentProposalGitHubPublishResult, time.Time) error
type AgentProposalPublicationTokenMinter func(context.Context, AgentProposalPublicationIdentity) (AgentProposalWriteToken, githubapp.RepositoryAccess, error)

type AgentProposalPublicationPublisher interface {
	PublishFenced(context.Context, AgentProposalWriteToken, AgentProposalGitHubPublishRequest,
		func(context.Context) error) (AgentProposalGitHubPublishResult, error)
}

type AgentProposalPublicationService struct {
	resolve AgentProposalPublicationResolver
	record  AgentProposalPublicationRecorder
	read    workloadsource.AgentProposalArchiveReader
	mint    AgentProposalPublicationTokenMinter
	writer  AgentProposalPublicationPublisher
	now     func() time.Time
}

func NewAgentProposalPublicationService(resolve AgentProposalPublicationResolver, record AgentProposalPublicationRecorder,
	reader workloadsource.AgentProposalArchiveReader, mint AgentProposalPublicationTokenMinter,
	writer AgentProposalPublicationPublisher) (*AgentProposalPublicationService, error) {
	if resolve == nil || record == nil || reader == nil || mint == nil || writer == nil {
		return nil, ErrUnavailable
	}
	return &AgentProposalPublicationService{resolve: resolve, record: record, read: reader, mint: mint, writer: writer, now: time.Now}, nil
}

type AgentProposalPublicationRequest struct {
	TenantID       domain.TenantID      `json:"tenant_id"`
	WorkloadRunID  domain.WorkloadRunID `json:"workload_run_id"`
	AgentAttemptID string               `json:"agent_attempt_id"`
}

type AgentProposalPublicationResponse struct {
	TenantID          domain.TenantID                         `json:"tenant_id"`
	WorkloadRunID     domain.WorkloadRunID                    `json:"workload_run_id"`
	State             workloads.AgentProposalPublicationState `json:"state"`
	DefaultBranch     string                                  `json:"default_branch"`
	BranchName        string                                  `json:"branch_name"`
	BranchCommitSHA   string                                  `json:"branch_commit_sha,omitempty"`
	PullRequestNumber int64                                   `json:"pull_request_number,omitempty"`
	Recovered         bool                                    `json:"recovered"`
}

// Publish is callable only by the mTLS-authenticated Control Plane. All
// GitHub writes remain within the Broker process; the response contains no
// token, archive data, or storage reference.
func (s *AgentProposalPublicationService) Publish(ctx context.Context, caller string, request AgentProposalPublicationRequest) (AgentProposalPublicationResponse, error) {
	if s == nil || ctx == nil || caller != controlPlaneURI || request.TenantID == "" ||
		!validCheckoutRunID(request.WorkloadRunID) || !validCheckoutRunID(domain.WorkloadRunID(request.AgentAttemptID)) {
		return AgentProposalPublicationResponse{}, ErrAgentProposalPublicationDenied
	}
	identity, err := s.resolve(ctx, request.TenantID, request.WorkloadRunID, request.AgentAttemptID)
	if err != nil || !validPublicationIdentity(identity, request) {
		return AgentProposalPublicationResponse{}, ErrAgentProposalPublicationDenied
	}
	archive, err := s.read.ReadAgentProposalArchive(ctx, identity.Archive)
	if err != nil {
		return AgentProposalPublicationResponse{}, ErrAgentProposalPublicationDenied
	}
	defer clear(archive)
	token, repository, err := s.mint(ctx, identity)
	if err != nil || token == nil || repository != (githubapp.RepositoryAccess{Owner: identity.Evidence.GitHubOwner, Name: identity.Evidence.GitHubRepository}) {
		return AgentProposalPublicationResponse{}, ErrAgentProposalPublicationDenied
	}
	if err := s.checkIdentity(ctx, request, identity); err != nil {
		return AgentProposalPublicationResponse{}, ErrAgentProposalPublicationDenied
	}
	publishRequest := AgentProposalGitHubPublishRequest{Repository: repository,
		DefaultBranch: identity.Evidence.DefaultBranch, BaseBranch: identity.Evidence.DefaultBranch,
		BaseCommitSHA: identity.Evidence.BaseCommitSHA, BranchName: identity.Publication.BranchName,
		ProposalSHA256: identity.Evidence.ProposalSHA256, RunID: string(identity.Evidence.AgentRunID),
		AttemptID: identity.Evidence.AgentAttemptID, Archive: archive, Policy: identity.Evidence.Policy,
		PassedChecks: identity.Evidence.PassedChecks, OpenPullRequest: identity.Evidence.Policy.AllowPullRequests}
	fence := func(fenceCtx context.Context) error { return s.checkIdentity(fenceCtx, request, identity) }
	result, publishErr := s.writer.PublishFenced(ctx, token, publishRequest, fence)
	if err := s.checkIdentity(ctx, request, identity); err != nil {
		publishErr = ErrAgentProposalPublicationDenied
	}
	var recordErr error
	if result.BranchCommitSHA != "" && result.BranchName == identity.Publication.BranchName {
		recordErr = s.record(ctx, identity, result, s.now())
	} else if result.BranchCommitSHA != "" {
		recordErr = ErrAgentProposalPublicationDenied
	}
	if publishErr != nil || recordErr != nil || result.BranchCommitSHA == "" {
		return AgentProposalPublicationResponse{}, ErrAgentProposalPublicationDenied
	}
	state := workloads.AgentProposalPublicationBranchCreated
	if result.PullRequestNumber > 0 {
		state = workloads.AgentProposalPublicationPullRequestCreated
	}
	return AgentProposalPublicationResponse{TenantID: request.TenantID, WorkloadRunID: request.WorkloadRunID,
		State: state, DefaultBranch: identity.Evidence.DefaultBranch, BranchName: result.BranchName,
		BranchCommitSHA: result.BranchCommitSHA, PullRequestNumber: result.PullRequestNumber, Recovered: result.Recovered}, nil
}

func (s *AgentProposalPublicationService) checkIdentity(ctx context.Context, request AgentProposalPublicationRequest,
	previous AgentProposalPublicationIdentity) error {
	current, err := s.resolve(ctx, request.TenantID, request.WorkloadRunID, request.AgentAttemptID)
	if err != nil || !sameAgentProposalPublicationIdentity(previous, current) {
		return ErrAgentProposalPublicationDenied
	}
	return nil
}

func validPublicationIdentity(identity AgentProposalPublicationIdentity, request AgentProposalPublicationRequest) bool {
	return identity.Run.ID == request.WorkloadRunID && identity.Run.TenantID == request.TenantID && identity.Run.Kind == domain.WorkloadAgent &&
		identity.Evidence.TenantID == request.TenantID && identity.Evidence.AgentRunID == request.WorkloadRunID &&
		identity.Evidence.AgentAttemptID == request.AgentAttemptID && identity.Evidence.GitHubIntegrationID != "" &&
		identity.Run.GitHubInstallation == identity.Evidence.GitHubInstallationID && identity.Run.GitHubRepository == identity.Evidence.GitHubRepositoryID &&
		identity.Run.GitHubIntegrationID == identity.Evidence.GitHubIntegrationID &&
		identity.Run.GitHubConfigRevision == identity.Evidence.GitHubConfigRevision &&
		identity.Run.GitHubCredentialGeneration == identity.Evidence.GitHubCredentialGen &&
		identity.Publication.TenantID == request.TenantID && identity.Publication.AgentRunID == request.WorkloadRunID &&
		identity.Publication.AgentAttemptID == request.AgentAttemptID && identity.Publication.ProposalSHA256 == identity.Evidence.ProposalSHA256 &&
		identity.Publication.BaseCommitSHA == identity.Evidence.BaseCommitSHA && identity.Publication.BranchName != "" &&
		identity.Publication.ProposalArtifactID == identity.Evidence.ProposalArtifactID &&
		identity.Publication.VerifierRunID == identity.Evidence.VerifierRunID &&
		identity.Publication.VerifierAttemptID == identity.Evidence.VerifierAttemptID &&
		identity.Publication.InstallationID == identity.Evidence.GitHubInstallationID &&
		identity.Publication.RepositoryID == identity.Evidence.GitHubRepositoryID &&
		identity.Publication.IntegrationID == identity.Evidence.GitHubIntegrationID &&
		identity.Publication.ConfigRevision == identity.Evidence.GitHubConfigRevision &&
		identity.Publication.CredentialGeneration == identity.Evidence.GitHubCredentialGen &&
		identity.Publication.DefaultBranch == identity.Evidence.DefaultBranch &&
		identity.AppAccess.GitHubAppID > 0 && identity.AppAccess.ReaderRoleARN != "" && identity.AppAccess.SecretARN != "" &&
		identity.AppAccess.SecretVersionID != "" &&
		identity.Archive.TenantID == string(request.TenantID) && identity.Archive.WorkloadRunID == string(request.WorkloadRunID) &&
		identity.Archive.AttemptID == request.AgentAttemptID && identity.Archive.SHA256 == identity.Evidence.ProposalSHA256 &&
		identity.AppAccess.TenantID == string(request.TenantID) && identity.AppAccess.IntegrationID == identity.Evidence.GitHubIntegrationID &&
		identity.AppAccess.ConfigRevision == identity.Evidence.GitHubConfigRevision &&
		identity.AppAccess.CredentialGeneration == identity.Evidence.GitHubCredentialGen
}

func sameAgentProposalPublicationIdentity(a, b AgentProposalPublicationIdentity) bool {
	return a.Run.ID == b.Run.ID && a.Run.TenantID == b.Run.TenantID && a.Run.Kind == b.Run.Kind &&
		a.Run.GitHubInstallation == b.Run.GitHubInstallation && a.Run.GitHubRepository == b.Run.GitHubRepository &&
		a.Run.GitHubIntegrationID == b.Run.GitHubIntegrationID && a.Run.GitHubConfigRevision == b.Run.GitHubConfigRevision &&
		a.Run.GitHubCredentialGeneration == b.Run.GitHubCredentialGeneration &&
		sameAgentProposalPublicationPins(a.Publication, b.Publication) &&
		a.Evidence.ProposalArtifactID == b.Evidence.ProposalArtifactID && a.Evidence.ProposalSHA256 == b.Evidence.ProposalSHA256 &&
		a.Evidence.BaseCommitSHA == b.Evidence.BaseCommitSHA && a.Evidence.VerifierRunID == b.Evidence.VerifierRunID &&
		a.Evidence.VerifierAttemptID == b.Evidence.VerifierAttemptID && a.Evidence.VerifierGeneration == b.Evidence.VerifierGeneration &&
		a.Evidence.GitHubInstallationID == b.Evidence.GitHubInstallationID && a.Evidence.GitHubRepositoryID == b.Evidence.GitHubRepositoryID &&
		a.Evidence.GitHubIntegrationID == b.Evidence.GitHubIntegrationID && a.Evidence.GitHubConfigRevision == b.Evidence.GitHubConfigRevision &&
		a.Evidence.GitHubCredentialGen == b.Evidence.GitHubCredentialGen && a.Evidence.GitHubOwner == b.Evidence.GitHubOwner &&
		a.Evidence.GitHubRepository == b.Evidence.GitHubRepository && a.Evidence.DefaultBranch == b.Evidence.DefaultBranch &&
		reflect.DeepEqual(a.Evidence.Policy, b.Evidence.Policy) && reflect.DeepEqual(a.Evidence.PassedChecks, b.Evidence.PassedChecks) &&
		a.AppAccess == b.AppAccess && a.Archive == b.Archive
}

func sameAgentProposalPublicationPins(a, b workloads.AgentProposalPublication) bool {
	return a.TenantID == b.TenantID && a.AgentRunID == b.AgentRunID && a.AgentAttemptID == b.AgentAttemptID &&
		a.ProposalArtifactID == b.ProposalArtifactID && a.ProposalSHA256 == b.ProposalSHA256 && a.BaseCommitSHA == b.BaseCommitSHA &&
		a.VerifierRunID == b.VerifierRunID && a.VerifierAttemptID == b.VerifierAttemptID && a.VerifierGeneration == b.VerifierGeneration &&
		a.InstallationID == b.InstallationID && a.RepositoryID == b.RepositoryID && a.IntegrationID == b.IntegrationID &&
		a.ConfigRevision == b.ConfigRevision && a.CredentialGeneration == b.CredentialGeneration && a.DefaultBranch == b.DefaultBranch &&
		a.PolicySHA256 == b.PolicySHA256 && a.BranchName == b.BranchName
}

// NewPostgresAgentProposalPublicationResolver re-reads the Agent Run, current
// verifier evidence, and active Tenant App credential binding on every call.
// It reserves the deterministic publication identity in the same transaction.
func NewPostgresAgentProposalPublicationResolver(pool transaction.Beginner, policy transaction.Policy) AgentProposalPublicationResolver {
	return func(ctx context.Context, tenantID domain.TenantID, runID domain.WorkloadRunID, attemptID string) (AgentProposalPublicationIdentity, error) {
		if pool == nil || ctx == nil || tenantID == "" || runID == "" || attemptID == "" {
			return AgentProposalPublicationIdentity{}, ErrAgentProposalPublicationDenied
		}
		return transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (AgentProposalPublicationIdentity, error) {
			run, err := workloads.Get(ctx, tx, tenantID, runID)
			if err != nil || run.Kind != domain.WorkloadAgent {
				return AgentProposalPublicationIdentity{}, ErrAgentProposalPublicationDenied
			}
			now := time.Now().UTC()
			evidence, err := workloads.ResolveAgentProposalPublicationEvidence(ctx, tx, tenantID, runID, attemptID, now)
			if err != nil || evidence.GitHubIntegrationID == "" {
				return AgentProposalPublicationIdentity{}, ErrAgentProposalPublicationDenied
			}
			credential, err := githubregistry.ResolveActiveAppCredential(ctx, tx, tenantID, evidence.GitHubIntegrationID,
				evidence.GitHubConfigRevision, evidence.GitHubCredentialGen)
			if err != nil || credential.GitHubAppID <= 0 {
				return AgentProposalPublicationIdentity{}, ErrAgentProposalPublicationDenied
			}
			app := githubapp.TenantPrivateKeyAccess{TenantID: string(credential.TenantID), IntegrationID: credential.IntegrationID,
				GitHubAppID: credential.GitHubAppID, ConfigRevision: credential.ConfigRevision, CredentialGeneration: credential.CredentialGeneration,
				ReaderRoleARN: credential.PrivateKeyReaderRoleARN, SecretARN: credential.PrivateKeySecretARN, SecretVersionID: credential.PrivateKeyVersionID}
			requested := workloads.AgentProposalPublication{TenantID: tenantID, AgentRunID: runID, AgentAttemptID: attemptID,
				ProposalArtifactID: evidence.ProposalArtifactID, ProposalSHA256: evidence.ProposalSHA256, BaseCommitSHA: evidence.BaseCommitSHA,
				VerifierRunID: evidence.VerifierRunID, VerifierAttemptID: evidence.VerifierAttemptID,
				InstallationID: evidence.GitHubInstallationID, RepositoryID: evidence.GitHubRepositoryID,
				IntegrationID: evidence.GitHubIntegrationID, ConfigRevision: evidence.GitHubConfigRevision,
				CredentialGeneration: evidence.GitHubCredentialGen}
			publication, err := workloads.CreateOrGetAgentProposalPublication(ctx, tx, requested, now)
			if err != nil {
				return AgentProposalPublicationIdentity{}, ErrAgentProposalPublicationDenied
			}
			return AgentProposalPublicationIdentity{Run: run, Publication: publication, Evidence: evidence,
				Archive:   workloadsource.AgentProposalArchiveIdentity{TenantID: string(tenantID), WorkloadRunID: string(runID), AttemptID: attemptID, SHA256: evidence.ProposalSHA256},
				AppAccess: app}, nil
		})
	}
}

func NewPostgresAgentProposalPublicationRecorder(pool transaction.Beginner, policy transaction.Policy) AgentProposalPublicationRecorder {
	return func(ctx context.Context, identity AgentProposalPublicationIdentity, result AgentProposalGitHubPublishResult, now time.Time) error {
		if pool == nil || ctx == nil || result.BranchCommitSHA == "" || now.IsZero() {
			return ErrAgentProposalPublicationDenied
		}
		_, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
			if _, err := workloads.RecordAgentProposalPublicationBranch(ctx, tx, identity.Publication.TenantID,
				identity.Publication.AgentRunID, identity.Publication.AgentAttemptID, result.BranchCommitSHA, now); err != nil {
				return struct{}{}, err
			}
			if result.PullRequestNumber > 0 {
				_, err := workloads.RecordAgentProposalPublicationPullRequest(ctx, tx, identity.Publication.TenantID,
					identity.Publication.AgentRunID, identity.Publication.AgentAttemptID, result.PullRequestNumber, now)
				return struct{}{}, err
			}
			return struct{}{}, nil
		})
		if err != nil {
			return ErrAgentProposalPublicationDenied
		}
		return nil
	}
}
