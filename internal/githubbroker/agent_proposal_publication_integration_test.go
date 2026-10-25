//go:build integration

package githubbroker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/testutil"
	"github.com/mo3789530/hako/internal/workloadsource"
)

func TestAgentProposalPublicationBrokerComposesPostgresResolverHTTPAndRecorder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	baseSHA, proposalSHA := strings.Repeat("a", 40), strings.Repeat("b", 64)
	archive := []byte("synthetic, untrusted proposal archive")
	for _, seed := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_pub_e2e','Publication E2E',$1)`, []any{now}},
		{`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_pub_e2e','publication-e2e','',$1)`, []any{now}},
		{`INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES ('rp_pub_e2e','aws','ap-northeast-1','{}')`, nil},
		{`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_pub_e2e',7101,'acme','active','usr_pub_e2e',$1,$1)`, []any{now}},
		{`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (7101,'tenant_pub_e2e',$1)`, []any{now}},
		{`INSERT INTO github_app_registrations(id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_pub_e2e',7102,'tenant_owned','tenant_pub_e2e','acme-hako','client-e2e',$1)`, []any{now}},
		{`INSERT INTO github_credential_bindings(id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id)
		 VALUES ('credential_pub_e2e','tenant_pub_e2e',7102,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/test-key-AbCdEf','unused-oauth','unused-webhook',$1,'arn:aws:iam::123456789012:role/hako/tenant-private-key-reader/e2e','0123456789abcdef0123456789abcdef')`, []any{now}},
		{`INSERT INTO tenant_github_integrations(id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at)
		 VALUES ('integration_pub_e2e','tenant_pub_e2e','registration_pub_e2e',7102,'credential_pub_e2e','active',3,4,'integration-key-e2e','integration-hash-e2e','usr_pub_e2e',$1,$1)`, []any{now}},
		{`INSERT INTO tenant_github_app_installation_bindings(tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_pub_e2e','integration_pub_e2e',7101,$1)`, []any{now}},
		{`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,factory_enabled,factory_enabled_at,agent_write_policy)
		 VALUES ('tenant_pub_e2e',7101,7103,'acme','api','main',$1,true,$1,'{"enabled":true,"branch_prefixes":["hako/agent/"],"allow_pull_requests":true,"required_checks":["hako/test"],"require_checks_to_pass":true}'::jsonb)`, []any{now}},
		{`INSERT INTO workload_runs(id,tenant_id,kind,requested_by,github_installation_id,github_repository_id,github_integration_id,github_config_revision,github_credential_generation,commit_sha,ref,trigger_event,trigger_branch,runtime_class,trust_level,desired_state,state,resource_plane_id,idempotency_key,request_hash,timeout_seconds,timeout_at,completed_at,revision,created_at,updated_at)
		 VALUES ('run_agent_pub_e2e','tenant_pub_e2e','agent','usr_pub_e2e',7101,7103,'integration_pub_e2e',3,4,$1,'refs/heads/main','issue_comment','main','standard','untrusted','running','succeeded','rp_pub_e2e','agent-key-pub-e2e','agent-hash-pub-e2e',1800,$2,$3,2,$4,$4)`, []any{baseSHA, now.Add(time.Hour), now, now}},
		{`INSERT INTO workload_runs(id,tenant_id,kind,requested_by,github_installation_id,github_repository_id,github_integration_id,github_config_revision,github_credential_generation,commit_sha,ref,trigger_event,trigger_branch,runtime_class,trust_level,desired_state,state,resource_plane_id,idempotency_key,request_hash,timeout_seconds,timeout_at,completed_at,revision,created_at,updated_at)
		 VALUES ('run_verifier_pub_e2e','tenant_pub_e2e','job','usr_pub_e2e',7101,7103,'integration_pub_e2e',3,4,$1,'refs/heads/main','pull_request','main','standard','untrusted','running','succeeded','rp_pub_e2e','verifier-key-pub-e2e','verifier-hash-pub-e2e',1800,$2,$3,2,$4,$4)`, []any{baseSHA, now.Add(time.Hour), now, now}},
		{`INSERT INTO workload_run_attempts(id,tenant_id,workload_run_id,generation,attempt_number,resource_plane_id,idempotency_token,state,cleanup_confirmed_at,created_at,updated_at)
		 VALUES ('attempt_agent_pub_e2e','tenant_pub_e2e','run_agent_pub_e2e',1,1,'rp_pub_e2e','agent-token-pub-e2e','succeeded',$1,$1,$1)`, []any{now}},
		{`INSERT INTO workload_run_attempts(id,tenant_id,workload_run_id,generation,attempt_number,resource_plane_id,idempotency_token,state,cleanup_confirmed_at,created_at,updated_at)
		 VALUES ('attempt_verifier_pub_e2e','tenant_pub_e2e','run_verifier_pub_e2e',1,1,'rp_pub_e2e','verifier-token-pub-e2e','succeeded',$1,$1,$1)`, []any{now}},
		{`INSERT INTO workload_artifacts(id,tenant_id,workload_run_id,kind,storage_ref,size_bytes,sha256,redacted,created_at)
		 VALUES ('artifact_pub_e2e','tenant_pub_e2e','run_agent_pub_e2e','agent_changes','s3://private/proposal.zip',128,$1,false,$2)`, []any{proposalSHA, now}},
		{`INSERT INTO workload_agent_proposal_verifications(tenant_id,agent_run_id,agent_attempt_id,proposal_artifact_id,proposal_sha256,base_commit_sha,verifier_run_id,verifier_attempt_id,created_at)
		 VALUES ('tenant_pub_e2e','run_agent_pub_e2e','attempt_agent_pub_e2e','artifact_pub_e2e',$1,$2,'run_verifier_pub_e2e','attempt_verifier_pub_e2e',$3)`, []any{proposalSHA, baseSHA, now}},
		{`INSERT INTO workload_pipeline_results(tenant_id,workload_run_id,generation,pipeline_name,status,exit_code,completed_at)
		 VALUES ('tenant_pub_e2e','run_verifier_pub_e2e',1,'test','succeeded',0,$1)`, []any{now}},
		{`INSERT INTO workload_agent_proposal_verification_checks(tenant_id,verifier_run_id,verifier_attempt_id,generation,pipeline_name,proposal_sha256,base_commit_sha,status,exit_code,completed_at)
		 VALUES ('tenant_pub_e2e','run_verifier_pub_e2e','attempt_verifier_pub_e2e',1,'test',$1,$2,'succeeded',0,$3)`, []any{proposalSHA, baseSHA, now}},
	} {
		if _, err := pool.Exec(ctx, seed.query, seed.args...); err != nil {
			t.Fatalf("seed composed proposal publication E2E: %v", err)
		}
	}
	reader := proposalArchiveReaderFunc(func(_ context.Context, identity workloadsource.AgentProposalArchiveIdentity) ([]byte, error) {
		if identity != (workloadsource.AgentProposalArchiveIdentity{TenantID: "tenant_pub_e2e", WorkloadRunID: "run_agent_pub_e2e", AttemptID: "attempt_agent_pub_e2e", SHA256: proposalSHA}) {
			t.Fatalf("archive reader received unpinned identity: %+v", identity)
		}
		return append([]byte(nil), archive...), nil
	})
	resolver := NewPostgresAgentProposalPublicationResolver(pool, transaction.DefaultPolicy())
	recorder := NewPostgresAgentProposalPublicationRecorder(pool, transaction.DefaultPolicy())
	var published AgentProposalGitHubPublishRequest
	var publishCalls int
	writer := proposalPublisherFunc(func(ctx context.Context, token AgentProposalWriteToken, request AgentProposalGitHubPublishRequest,
		fence func(context.Context) error) (AgentProposalGitHubPublishResult, error) {
		publishCalls++
		if token.Value() != "opaque" || request.Repository != (githubapp.RepositoryAccess{Owner: "acme", Name: "api"}) ||
			request.DefaultBranch != "main" || request.BaseBranch != "main" || request.BaseCommitSHA != baseSHA ||
			request.ProposalSHA256 != proposalSHA || request.RunID != "run_agent_pub_e2e" || request.AttemptID != "attempt_agent_pub_e2e" ||
			!request.OpenPullRequest || !request.PassedChecks["hako/test"] || string(request.Archive) != string(archive) {
			t.Fatalf("GitHub writer received unpinned proposal request: %+v", request)
		}
		published = request
		if err := fence(ctx); err != nil {
			return AgentProposalGitHubPublishResult{}, err
		}
		return AgentProposalGitHubPublishResult{BranchName: request.BranchName,
			BranchCommitSHA: strings.Repeat("d", 40), PullRequestNumber: 42}, nil
	})
	service, err := NewAgentProposalPublicationService(resolver, recorder, reader,
		func(_ context.Context, identity AgentProposalPublicationIdentity) (AgentProposalWriteToken, githubapp.RepositoryAccess, error) {
			if identity.AppAccess.IntegrationID != "integration_pub_e2e" || identity.AppAccess.ConfigRevision != 3 ||
				identity.AppAccess.CredentialGeneration != 4 || identity.AppAccess.SecretVersionID != "0123456789abcdef0123456789abcdef" {
				t.Fatalf("Broker resolved unexpected Tenant App pin: %+v", identity.AppAccess)
			}
			return proposalPublishToken{}, githubapp.RepositoryAccess{Owner: "acme", Name: "api"}, nil
		}, writer)
	if err != nil {
		t.Fatal(err)
	}
	handler := WithAgentProposalPublication(http.NotFoundHandler(), service)
	body, err := json.Marshal(AgentProposalPublicationRequest{TenantID: "tenant_pub_e2e", WorkloadRunID: "run_agent_pub_e2e", AgentAttemptID: "attempt_agent_pub_e2e"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, agentProposalPublishRoute, strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	cert := &x509.Certificate{URIs: []*url.URL{{Scheme: "spiffe", Host: "hako", Path: "/control-plane"}},
		Subject: pkix.Name{CommonName: "synthetic-control-plane"}, SerialNumber: big.NewInt(1)}
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("composed publication response = HTTP %d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var safe AgentProposalPublicationResponse
	if err := json.Unmarshal(response.Body.Bytes(), &safe); err != nil || safe.State != workloads.AgentProposalPublicationPullRequestCreated ||
		safe.PullRequestNumber != 42 || safe.BranchName != published.BranchName || safe.BranchCommitSHA != strings.Repeat("d", 40) {
		t.Fatalf("composed publication response = %+v error=%v body=%s", safe, err, response.Body.String())
	}
	for _, forbidden := range []string{"archive", "storage_ref", "secret", "token", proposalSHA} {
		if strings.Contains(strings.ToLower(response.Body.String()), strings.ToLower(forbidden)) {
			t.Fatalf("composed response leaked %q: %s", forbidden, response.Body.String())
		}
	}
	var state string
	var branch, commit string
	var pullRequest int64
	if err := pool.QueryRow(ctx, `SELECT state,branch_name,branch_commit_sha,pull_request_number FROM workload_agent_proposal_publications
		WHERE tenant_id='tenant_pub_e2e' AND agent_run_id='run_agent_pub_e2e' AND agent_attempt_id='attempt_agent_pub_e2e'`).
		Scan(&state, &branch, &commit, &pullRequest); err != nil || state != string(workloads.AgentProposalPublicationPullRequestCreated) ||
		branch != published.BranchName || commit != strings.Repeat("d", 40) || pullRequest != 42 {
		t.Fatalf("recorded publication state=%q branch=%q commit=%q PR=%d error=%v", state, branch, commit, pullRequest, err)
	}
	duplicateRequest := httptest.NewRequest(http.MethodPost, agentProposalPublishRoute, strings.NewReader(string(body)))
	duplicateRequest.Header.Set("Content-Type", "application/json")
	duplicateRequest.TLS = request.TLS
	duplicateResponse := httptest.NewRecorder()
	handler.ServeHTTP(duplicateResponse, duplicateRequest)
	if duplicateResponse.Code != http.StatusOK || publishCalls != 2 {
		t.Fatalf("idempotent duplicate publication = HTTP %d calls=%d body=%s", duplicateResponse.Code, publishCalls, duplicateResponse.Body.String())
	}
	var publicationRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_agent_proposal_publications WHERE tenant_id='tenant_pub_e2e'
		AND agent_run_id='run_agent_pub_e2e' AND agent_attempt_id='attempt_agent_pub_e2e'`).Scan(&publicationRows); err != nil || publicationRows != 1 {
		t.Fatalf("duplicate publication rows=%d error=%v", publicationRows, err)
	}
}
