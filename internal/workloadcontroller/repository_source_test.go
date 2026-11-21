package workloadcontroller

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/repositorycheckout"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

type repositoryCheckoutFunc func(context.Context, domain.WorkloadRun) (repositorycheckout.CheckedOutRepository, error)

func (f repositoryCheckoutFunc) Checkout(ctx context.Context, run domain.WorkloadRun) (repositorycheckout.CheckedOutRepository, error) {
	return f(ctx, run)
}

type sourceArchiveStagerFunc func(context.Context, workloadjournal.Command, string) (MaterializedSource, error)

func (f sourceArchiveStagerFunc) StageCheckout(ctx context.Context, id workloadjournal.Command, path string) (MaterializedSource, error) {
	return f(ctx, id, path)
}

type runCredentialProviderFunc func(context.Context, domain.WorkloadRun) (string, error)

func (f runCredentialProviderFunc) CheckoutRunToken(ctx context.Context, run domain.WorkloadRun) (string, error) {
	return f(ctx, run)
}

type proposalArchiveReaderFunc func(context.Context, workloadsource.AgentProposalArchiveIdentity) ([]byte, error)

func (f proposalArchiveReaderFunc) ReadAgentProposalArchive(ctx context.Context, identity workloadsource.AgentProposalArchiveIdentity) ([]byte, error) {
	return f(ctx, identity)
}

func proposalZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, contents := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func TestRepositorySourceMaterializerUsesOnlyTrustedImmutableRunFields(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	metadata := materializerMetadataFixture(now)
	var gotRun domain.WorkloadRun
	var gotIdentity workloadjournal.Command
	var gotPath string
	materializer, err := NewRepositorySourceMaterializer(repositoryCheckoutFunc(func(_ context.Context, run domain.WorkloadRun) (repositorycheckout.CheckedOutRepository, error) {
		gotRun = run
		return repositorycheckout.CheckedOutRepository{Path: "/private/checkout"}, nil
	}), sourceArchiveStagerFunc(func(_ context.Context, id workloadjournal.Command, path string) (MaterializedSource, error) {
		gotIdentity, gotPath = id, path
		return MaterializedSource{Digest: strings.Repeat("b", 64), ArtifactPrefix: "s3://hako-artifacts/workloads/tenant_1/run_1/attempt_1/"}, nil
	}), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	source, err := materializer.Materialize(context.Background(), metadata)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.ID != metadata.WorkloadRunID || gotRun.TenantID != metadata.TenantID || gotRun.Kind != domain.WorkloadJob ||
		gotRun.GitHubInstallation != metadata.GitHubInstallationID || gotRun.GitHubRepository != metadata.GitHubRepositoryID ||
		gotRun.GitHubIntegrationID != metadata.GitHubIntegrationID || gotRun.GitHubConfigRevision != metadata.GitHubConfigRevision ||
		gotRun.GitHubCredentialGeneration != metadata.GitHubCredentialGen || gotRun.CommitSHA != metadata.CommitSHA || gotRun.Ref != metadata.Ref ||
		gotRun.DesiredState != domain.WorkloadDesiredRunning || gotRun.State != domain.WorkloadProvisioning ||
		gotRun.TimeoutSeconds != 7200 || !gotRun.TimeoutAt.Equal(metadata.Deadline) || !gotRun.CreatedAt.Equal(metadata.RequestedAt) {
		t.Fatalf("checkout did not receive the immutable authorized Run: %+v", gotRun)
	}
	if gotIdentity.TenantID != string(metadata.TenantID) || gotIdentity.WorkloadRunID != string(metadata.WorkloadRunID) ||
		gotIdentity.AttemptID != metadata.AttemptID || gotIdentity.Generation != metadata.Generation ||
		gotIdentity.ResourcePlaneID != string(metadata.ResourcePlaneID) || gotPath != "/private/checkout" || source.Digest != strings.Repeat("b", 64) {
		t.Fatalf("stager received incorrect identity or source: id=%+v path=%q source=%+v", gotIdentity, gotPath, source)
	}
}

func TestRepositorySourceMaterializerDoesNotStageOnCheckoutOrMetadataFailure(t *testing.T) {
	now := time.Now().UTC()
	metadata := materializerMetadataFixture(now)
	checkoutCalls, stageCalls := 0, 0
	checkoutErr := errors.New("credential-bearing checkout error")
	materializer, err := NewRepositorySourceMaterializer(repositoryCheckoutFunc(func(context.Context, domain.WorkloadRun) (repositorycheckout.CheckedOutRepository, error) {
		checkoutCalls++
		return repositorycheckout.CheckedOutRepository{}, checkoutErr
	}), sourceArchiveStagerFunc(func(context.Context, workloadjournal.Command, string) (MaterializedSource, error) {
		stageCalls++
		return MaterializedSource{}, nil
	}), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := materializer.Materialize(context.Background(), metadata); !errors.Is(err, ErrExecutionMaterialization) || err == checkoutErr || checkoutCalls != 1 || stageCalls != 0 {
		t.Fatalf("checkout failure was not safely fenced: err=%v checkout=%d stage=%d", err, checkoutCalls, stageCalls)
	}
	metadata.Generation = 0
	if _, err := materializer.Materialize(context.Background(), metadata); !errors.Is(err, ErrExecutionMetadata) || checkoutCalls != 1 || stageCalls != 0 {
		t.Fatalf("invalid metadata reached checkout: err=%v checkout=%d stage=%d", err, checkoutCalls, stageCalls)
	}
}

func TestRepositorySourceMaterializerRejectsUnsafeStagerOutput(t *testing.T) {
	now := time.Now().UTC()
	metadata := materializerMetadataFixture(now)
	for _, source := range []MaterializedSource{
		{Digest: "not-a-digest", ArtifactPrefix: "s3://bucket/tenant_1/run_1/attempt_1/"},
		{Digest: strings.Repeat("a", 64), ArtifactPrefix: "s3://bucket/tenant_1/run_1/another-attempt/"},
	} {
		materializer, err := NewRepositorySourceMaterializer(repositoryCheckoutFunc(func(context.Context, domain.WorkloadRun) (repositorycheckout.CheckedOutRepository, error) {
			return repositorycheckout.CheckedOutRepository{Path: "/private/checkout"}, nil
		}), sourceArchiveStagerFunc(func(context.Context, workloadjournal.Command, string) (MaterializedSource, error) { return source, nil }), func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		if _, err := materializer.Materialize(context.Background(), metadata); !errors.Is(err, ErrExecutionMaterialization) {
			t.Fatalf("unsafe stager output accepted: %+v err=%v", source, err)
		}
	}
}

func TestTenantBrokeredMaterializerRejectsMissingTenantPinAndForeignRepository(t *testing.T) {
	now := time.Now().UTC()
	metadata := materializerMetadataFixture(now)
	credentialCalls := 0
	materializer, err := NewTenantBrokeredRepositorySourceMaterializer(runCredentialProviderFunc(func(context.Context, domain.WorkloadRun) (string, error) {
		credentialCalls++
		return "ghs_test_token_value", nil
	}), sourceArchiveStagerFunc(func(context.Context, workloadjournal.Command, string) (MaterializedSource, error) {
		t.Fatal("untrusted metadata reached source staging")
		return MaterializedSource{}, nil
	}), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}

	unpinned := metadata
	unpinned.GitHubIntegrationID, unpinned.GitHubConfigRevision, unpinned.GitHubCredentialGen = "", 0, 0
	if _, err := materializer.Materialize(context.Background(), unpinned); !errors.Is(err, ErrTenantCheckoutRequired) || credentialCalls != 0 {
		t.Fatalf("un-pinned Tenant Run was not rejected before credential lookup: calls=%d err=%v", credentialCalls, err)
	}

	authorizer := metadataRepositoryAuthorizer{tenantID: metadata.TenantID, installationID: metadata.GitHubInstallationID,
		repositoryID: metadata.GitHubRepositoryID, owner: metadata.GitHubOwner, repository: metadata.GitHubRepository}
	if access, err := authorizer.AuthorizeRepository(context.Background(), metadata.TenantID, metadata.GitHubInstallationID, metadata.GitHubRepositoryID); err != nil ||
		access.Owner != metadata.GitHubOwner || access.Name != metadata.GitHubRepository {
		t.Fatalf("trusted repository identity was not authorized: %+v err=%v", access, err)
	}
	for _, tc := range []struct {
		name         string
		tenant       domain.TenantID
		installation int64
		repository   int64
	}{
		{name: "foreign tenant", tenant: "tenant_other", installation: metadata.GitHubInstallationID, repository: metadata.GitHubRepositoryID},
		{name: "foreign installation", tenant: metadata.TenantID, installation: metadata.GitHubInstallationID + 1, repository: metadata.GitHubRepositoryID},
		{name: "foreign repository", tenant: metadata.TenantID, installation: metadata.GitHubInstallationID, repository: metadata.GitHubRepositoryID + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := authorizer.AuthorizeRepository(context.Background(), tc.tenant, tc.installation, tc.repository); !errors.Is(err, ErrTenantCheckoutRequired) {
				t.Fatalf("foreign repository identity was accepted: %v", err)
			}
		})
	}
}

func TestRepositorySourceMaterializerOverlaysDigestBoundAgentProposal(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	baseRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseRoot, "main.go"), []byte("package main\nconst value = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := workloadsource.CreateArchive(context.Background(), baseRoot, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	proposal := proposalZip(t, map[string]string{"main.go": "package main\nconst value = 2\n"})
	proposalDigest := sha256Hex(proposal)
	metadata := materializerMetadataFixture(now)
	metadata.SchemaVersion, metadata.Kind, metadata.TriggerEvent, metadata.TriggerBranch = workloadSpecSchemaV4, domain.WorkloadJob, "pull_request", "main"
	metadata.ProposalVerification = &ProposalVerificationMetadata{AgentRunID: "run_agent", AgentAttemptID: "attempt_agent",
		ProposalArtifactID: "artifact_proposal", ProposalSHA256: proposalDigest, BaseCommitSHA: metadata.CommitSHA}
	var gotProposalIdentity workloadsource.AgentProposalArchiveIdentity
	var staged []byte
	materialized, err := materializeRepositorySourceWithProposal(context.Background(), metadata, repositoryCheckoutFunc(func(_ context.Context, run domain.WorkloadRun) (repositorycheckout.CheckedOutRepository, error) {
		if run.CommitSHA != metadata.ProposalVerification.BaseCommitSHA {
			return repositorycheckout.CheckedOutRepository{}, errors.New("base commit changed")
		}
		return repositorycheckout.CheckedOutRepository{Path: baseRoot}, nil
	}), sourceArchiveStagerFunc(func(_ context.Context, id workloadjournal.Command, checkoutPath string) (MaterializedSource, error) {
		staged, err = os.ReadFile(filepath.Join(checkoutPath, "main.go"))
		return MaterializedSource{Digest: sha256Hex([]byte("verified source")), ArtifactPrefix: "s3://hako-artifacts/tenant_1/run_1/attempt_1/"}, err
	}), proposalArchiveReaderFunc(func(_ context.Context, identity workloadsource.AgentProposalArchiveIdentity) ([]byte, error) {
		gotProposalIdentity = identity
		return append([]byte(nil), proposal...), nil
	}), func() time.Time { return now })
	if err != nil {
		t.Fatalf("materialize verifier proposal source: %v", err)
	}
	if string(staged) != "package main\nconst value = 2\n" || gotProposalIdentity.TenantID != string(metadata.TenantID) ||
		gotProposalIdentity.WorkloadRunID != "run_agent" || gotProposalIdentity.AttemptID != "attempt_agent" || gotProposalIdentity.SHA256 != proposalDigest ||
		materialized.Digest != sha256Hex([]byte("verified source")) {
		t.Fatalf("proposal overlay/source identity mismatch: staged=%q proposal=%+v source=%+v", staged, gotProposalIdentity, materialized)
	}
}

func materializerMetadataFixture(now time.Time) ControlPlaneExecutionMetadata {
	return ControlPlaneExecutionMetadata{SchemaVersion: 1, TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 3,
		ResourcePlaneID: "rp_1", GitHubInstallationID: 71, GitHubRepositoryID: 81, GitHubOwner: "acme", GitHubRepository: "api",
		GitHubIntegrationID: "integration_1", GitHubConfigRevision: 2, GitHubCredentialGen: 4, CommitSHA: strings.Repeat("a", 40),
		Ref: "refs/heads/main", RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted,
		RequestedAt: now.Add(-time.Hour), Deadline: now.Add(time.Hour)}
}
