package githubbroker

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net/url"
	"strings"

	"github.com/mo3789530/hako/internal/agenttask"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/workloadsource"
)

type proposalPublishToken struct{}

func (proposalPublishToken) Value() string        { return "opaque" }
func (proposalPublishToken) ExpiresAt() time.Time { return time.Now().Add(10 * time.Minute) }

type proposalArchiveReaderFunc func(context.Context, workloadsource.AgentProposalArchiveIdentity) ([]byte, error)

func (f proposalArchiveReaderFunc) ReadAgentProposalArchive(ctx context.Context, id workloadsource.AgentProposalArchiveIdentity) ([]byte, error) {
	return f(ctx, id)
}

type proposalPublisherFunc func(context.Context, AgentProposalWriteToken, AgentProposalGitHubPublishRequest,
	func(context.Context) error) (AgentProposalGitHubPublishResult, error)

func (f proposalPublisherFunc) PublishFenced(ctx context.Context, token AgentProposalWriteToken, request AgentProposalGitHubPublishRequest,
	fence func(context.Context) error) (AgentProposalGitHubPublishResult, error) {
	return f(ctx, token, request, fence)
}

func proposalPublicationFixture() (AgentProposalPublicationIdentity, AgentProposalPublicationRequest) {
	request := AgentProposalPublicationRequest{TenantID: "tenant_publish", WorkloadRunID: "run_publish", AgentAttemptID: "attempt_publish"}
	policy := agenttask.WritePolicy{Enabled: true, BranchPrefixes: []string{"hako/"}, RequiredChecks: []string{"hako/test"}, AllowPullRequests: true}
	evidence := workloads.AgentProposalPublicationEvidence{TenantID: request.TenantID, AgentRunID: request.WorkloadRunID,
		AgentAttemptID: request.AgentAttemptID, ProposalArtifactID: "artifact_publish", ProposalSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseCommitSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", VerifierRunID: "run_verify", VerifierAttemptID: "attempt_verify",
		VerifierGeneration: 2, GitHubInstallationID: 42, GitHubRepositoryID: 43, GitHubIntegrationID: "integration_publish",
		GitHubConfigRevision: 3, GitHubCredentialGen: 4, GitHubOwner: "acme", GitHubRepository: "service", DefaultBranch: "main",
		Policy: policy, PassedChecks: map[string]bool{"hako/test": true}}
	publication := workloads.AgentProposalPublication{TenantID: request.TenantID, AgentRunID: request.WorkloadRunID,
		AgentAttemptID: request.AgentAttemptID, ProposalArtifactID: evidence.ProposalArtifactID, ProposalSHA256: evidence.ProposalSHA256,
		BaseCommitSHA: evidence.BaseCommitSHA, VerifierRunID: evidence.VerifierRunID, VerifierAttemptID: evidence.VerifierAttemptID,
		VerifierGeneration: evidence.VerifierGeneration, InstallationID: evidence.GitHubInstallationID, RepositoryID: evidence.GitHubRepositoryID,
		IntegrationID: evidence.GitHubIntegrationID, ConfigRevision: evidence.GitHubConfigRevision, CredentialGeneration: evidence.GitHubCredentialGen,
		DefaultBranch: evidence.DefaultBranch, PolicySHA256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", BranchName: "hako/proposal-deadbeef",
		State: workloads.AgentProposalPublicationRequested}
	run := domain.WorkloadRun{ID: request.WorkloadRunID, TenantID: request.TenantID, Kind: domain.WorkloadAgent,
		GitHubInstallation: evidence.GitHubInstallationID, GitHubRepository: evidence.GitHubRepositoryID,
		GitHubIntegrationID: evidence.GitHubIntegrationID, GitHubConfigRevision: evidence.GitHubConfigRevision,
		GitHubCredentialGeneration: evidence.GitHubCredentialGen, CommitSHA: evidence.BaseCommitSHA}
	app := githubapp.TenantPrivateKeyAccess{TenantID: string(request.TenantID), IntegrationID: evidence.GitHubIntegrationID,
		GitHubAppID: 45, ConfigRevision: evidence.GitHubConfigRevision, CredentialGeneration: evidence.GitHubCredentialGen,
		ReaderRoleARN: "arn:aws:iam::123456789012:role/hako-reader", SecretARN: "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/key-abc",
		SecretVersionID: "0123456789abcdef0123456789abcdef"}
	identity := AgentProposalPublicationIdentity{Run: run, Publication: publication, Evidence: evidence, AppAccess: app,
		Archive: workloadsource.AgentProposalArchiveIdentity{TenantID: string(request.TenantID), WorkloadRunID: string(request.WorkloadRunID),
			AttemptID: request.AgentAttemptID, SHA256: evidence.ProposalSHA256}}
	return identity, request
}

func TestAgentProposalPublicationServiceRequiresControlPlaneBeforeLookup(t *testing.T) {
	identity, request := proposalPublicationFixture()
	called := false
	service, err := NewAgentProposalPublicationService(func(context.Context, domain.TenantID, domain.WorkloadRunID, string) (AgentProposalPublicationIdentity, error) {
		called = true
		return identity, nil
	}, func(context.Context, AgentProposalPublicationIdentity, AgentProposalGitHubPublishResult, time.Time) error {
		return nil
	},
		proposalArchiveReaderFunc(func(context.Context, workloadsource.AgentProposalArchiveIdentity) ([]byte, error) {
			return []byte("archive"), nil
		}),
		func(context.Context, AgentProposalPublicationIdentity) (AgentProposalWriteToken, githubapp.RepositoryAccess, error) {
			return proposalPublishToken{}, githubapp.RepositoryAccess{Owner: "acme", Name: "service"}, nil
		},
		proposalPublisherFunc(func(context.Context, AgentProposalWriteToken, AgentProposalGitHubPublishRequest, func(context.Context) error) (AgentProposalGitHubPublishResult, error) {
			return AgentProposalGitHubPublishResult{}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Publish(context.Background(), "spiffe://hako/resource-plane/rp-one", request); !errors.Is(err, ErrAgentProposalPublicationDenied) || called {
		t.Fatalf("foreign caller result err=%v resolver_called=%t", err, called)
	}
}

func TestAgentProposalPublicationServiceFencesAfterTokenMintAndRecordsPartialBranch(t *testing.T) {
	identity, request := proposalPublicationFixture()
	current := identity
	resolveCalls, publishCalls := 0, 0
	var recorded AgentProposalGitHubPublishResult
	service, err := NewAgentProposalPublicationService(func(context.Context, domain.TenantID, domain.WorkloadRunID, string) (AgentProposalPublicationIdentity, error) {
		resolveCalls++
		if resolveCalls == 4 { // the App generation rotates after the remote call
			current.Evidence.GitHubCredentialGen++
		}
		return current, nil
	}, func(_ context.Context, _ AgentProposalPublicationIdentity, result AgentProposalGitHubPublishResult, _ time.Time) error {
		recorded = result
		return nil
	}, proposalArchiveReaderFunc(func(context.Context, workloadsource.AgentProposalArchiveIdentity) ([]byte, error) {
		return []byte("archive"), nil
	}),
		func(context.Context, AgentProposalPublicationIdentity) (AgentProposalWriteToken, githubapp.RepositoryAccess, error) {
			return proposalPublishToken{}, githubapp.RepositoryAccess{Owner: "acme", Name: "service"}, nil
		}, proposalPublisherFunc(func(ctx context.Context, _ AgentProposalWriteToken, _ AgentProposalGitHubPublishRequest,
			fence func(context.Context) error) (AgentProposalGitHubPublishResult, error) {
			publishCalls++
			if err := fence(ctx); err != nil {
				return AgentProposalGitHubPublishResult{}, err
			}
			return AgentProposalGitHubPublishResult{BranchName: identity.Publication.BranchName,
				BranchCommitSHA: "dddddddddddddddddddddddddddddddddddddddd"}, errors.New("pull request create response lost")
		}))
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Publish(context.Background(), controlPlaneURI, request)
	if !errors.Is(err, ErrAgentProposalPublicationDenied) || response.BranchName != "" || publishCalls != 1 || recorded.BranchCommitSHA == "" {
		t.Fatalf("response=%+v err=%v publish_calls=%d recorded=%+v", response, err, publishCalls, recorded)
	}
}

func TestAgentProposalPublicationServiceSuccessReturnsOnlySafeMetadata(t *testing.T) {
	identity, request := proposalPublicationFixture()
	archive := []byte("synthetic proposal archive")
	var observed AgentProposalGitHubPublishRequest
	service, err := NewAgentProposalPublicationService(func(context.Context, domain.TenantID, domain.WorkloadRunID, string) (AgentProposalPublicationIdentity, error) {
		return identity, nil
	}, func(context.Context, AgentProposalPublicationIdentity, AgentProposalGitHubPublishResult, time.Time) error {
		return nil
	},
		proposalArchiveReaderFunc(func(context.Context, workloadsource.AgentProposalArchiveIdentity) ([]byte, error) {
			return append([]byte(nil), archive...), nil
		}),
		func(context.Context, AgentProposalPublicationIdentity) (AgentProposalWriteToken, githubapp.RepositoryAccess, error) {
			return proposalPublishToken{}, githubapp.RepositoryAccess{Owner: "acme", Name: "service"}, nil
		}, proposalPublisherFunc(func(ctx context.Context, _ AgentProposalWriteToken, input AgentProposalGitHubPublishRequest,
			fence func(context.Context) error) (AgentProposalGitHubPublishResult, error) {
			observed = input
			observed.Archive = append([]byte(nil), input.Archive...)
			if err := fence(ctx); err != nil {
				return AgentProposalGitHubPublishResult{}, err
			}
			return AgentProposalGitHubPublishResult{BranchName: identity.Publication.BranchName,
				BranchCommitSHA: "dddddddddddddddddddddddddddddddddddddddd", PullRequestNumber: 17}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Publish(context.Background(), controlPlaneURI, request)
	if err != nil || response.State != workloads.AgentProposalPublicationPullRequestCreated || response.PullRequestNumber != 17 || string(observed.Archive) != string(archive) {
		t.Fatalf("response=%+v err=%v observed=%+v", response, err, observed)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"opaque", "archive", "secret", "token", "artifact"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestAgentProposalPublicationHTTPRouteRequiresControlPlaneAndRejectsUnknownFields(t *testing.T) {
	identity, request := proposalPublicationFixture()
	service, err := NewAgentProposalPublicationService(func(context.Context, domain.TenantID, domain.WorkloadRunID, string) (AgentProposalPublicationIdentity, error) {
		return identity, nil
	}, func(context.Context, AgentProposalPublicationIdentity, AgentProposalGitHubPublishResult, time.Time) error {
		return nil
	},
		proposalArchiveReaderFunc(func(context.Context, workloadsource.AgentProposalArchiveIdentity) ([]byte, error) {
			return []byte("archive"), nil
		}),
		func(context.Context, AgentProposalPublicationIdentity) (AgentProposalWriteToken, githubapp.RepositoryAccess, error) {
			return proposalPublishToken{}, githubapp.RepositoryAccess{Owner: "acme", Name: "service"}, nil
		},
		proposalPublisherFunc(func(context.Context, AgentProposalWriteToken, AgentProposalGitHubPublishRequest, func(context.Context) error) (AgentProposalGitHubPublishResult, error) {
			return AgentProposalGitHubPublishResult{BranchName: identity.Publication.BranchName, BranchCommitSHA: "dddddddddddddddddddddddddddddddddddddddd"}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	handler := WithAgentProposalPublication(http.NotFoundHandler(), service)
	body, _ := json.Marshal(request)
	unauthenticated := httptest.NewRequest(http.MethodPost, agentProposalPublishRoute, bytes.NewReader(body))
	unauthenticated.Header.Set("Content-Type", "application/json")
	unauthResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthResponse, unauthenticated)
	if unauthResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", unauthResponse.Code)
	}
	bad := strings.TrimSuffix(string(body), "}") + `,"token":"attacker"}`
	requestWithTLS := func(payload string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, agentProposalPublishRoute, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		cert := &x509.Certificate{URIs: []*url.URL{{Scheme: "spiffe", Host: "hako", Path: "/control-plane"}}, Subject: pkix.Name{CommonName: "test"}, SerialNumber: big.NewInt(1)}
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
		return req
	}
	badResponse := httptest.NewRecorder()
	handler.ServeHTTP(badResponse, requestWithTLS(bad))
	if badResponse.Code != http.StatusBadRequest {
		t.Fatalf("unknown-field status=%d body=%s", badResponse.Code, badResponse.Body.String())
	}
	goodResponse := httptest.NewRecorder()
	handler.ServeHTTP(goodResponse, requestWithTLS(string(body)))
	if goodResponse.Code != http.StatusOK || goodResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("success status=%d headers=%v body=%s", goodResponse.Code, goodResponse.Header(), goodResponse.Body.String())
	}
}

func TestHTTPAgentProposalPublicationClientRequiresSafeNoStoreResponse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != agentProposalPublishRoute || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected internal publication request: %s %s", r.Method, r.URL.Path)
		}
		var request AgentProposalPublicationRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.TenantID != "tenant_publish" || request.AgentAttemptID != "attempt_publish" {
			t.Errorf("unexpected internal publication body: %+v error=%v", request, err)
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tenant_id":"tenant_publish","workload_run_id":"run_publish","state":"branch_created","default_branch":"main","branch_name":"hako/proposal-abc","branch_commit_sha":"dddddddddddddddddddddddddddddddddddddddd"}`))
	}))
	defer server.Close()
	client, err := NewHTTPAgentProposalPublicationClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, request := proposalPublicationFixture()
	result, err := client.Publish(context.Background(), request)
	if err != nil || result.State != workloads.AgentProposalPublicationBranchCreated || result.BranchCommitSHA == "" {
		t.Fatalf("publication client result=%+v error=%v", result, err)
	}
}
