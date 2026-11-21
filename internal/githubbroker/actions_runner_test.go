package githubbroker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/githubapp"
)

func actionsRunnerIdentity(now time.Time) ActionsRunnerJobIdentity {
	return ActionsRunnerJobIdentity{
		IntegrationID: "integration_runner", DeliveryID: "d3b2c3d4-e5f6-4789-8abc-def012345678", TenantID: "tenant_runner",
		Installation: 77, Repository: 88, WorkflowJobID: 99, WorkflowRunID: 100, RunAttempt: 1,
		WorkflowName: "CI", JobName: "test", HeadBranch: "main", HeadSHA: strings.Repeat("a", 40), Labels: []string{"self-hosted", "hako"},
		ClaimID: "controller-claim-1", LeaseExpires: now.Add(time.Minute),
		Credential: githubapp.TenantPrivateKeyAccess{TenantID: "tenant_runner", IntegrationID: "integration_runner", GitHubAppID: 12,
			ConfigRevision: 3, CredentialGeneration: 4, ReaderRoleARN: "arn:aws:iam::123456789012:role/key-reader",
			SecretARN: "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:key", SecretVersionID: strings.Repeat("a", 32)},
	}
}

func TestActionsRunnerRegistrationServiceBindsIssuanceToClaimedControlPlaneJob(t *testing.T) {
	now := time.Now().UTC()
	identity := actionsRunnerIdentity(now)
	started, failed, minted := 0, 0, 0
	service, err := NewActionsRunnerRegistrationService(func(_ context.Context, request ActionsRunnerRegistrationRequest) (ActionsRunnerJobIdentity, error) {
		started++
		if request.IntegrationID != identity.IntegrationID || request.WorkflowJobID != identity.WorkflowJobID || request.RunAttempt != identity.RunAttempt || request.ClaimID != identity.ClaimID {
			t.Fatal("request identity was not forwarded to the durable claim resolver")
		}
		return identity, nil
	}, func(context.Context, ActionsRunnerJobIdentity) error {
		failed++
		return nil
	}, func(context.Context, ActionsRunnerJobIdentity) (githubapp.RunnerRegistrationToken, githubapp.RepositoryAccess, error) {
		minted++
		token, err := githubapp.NewRunnerRegistrationToken("synthetic-registration-token", now.Add(time.Hour))
		return token, githubapp.RepositoryAccess{Owner: "acme", Name: "api"}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	service.clock = func() time.Time { return now }
	request := ActionsRunnerRegistrationRequest{IntegrationID: identity.IntegrationID, WorkflowJobID: identity.WorkflowJobID, RunAttempt: identity.RunAttempt, ClaimID: identity.ClaimID}
	if _, err := service.Issue(context.Background(), "spiffe://hako/resource-plane/rp-one", request); err == nil || started != 0 || minted != 0 {
		t.Fatal("Resource Plane caller reached runner registration-token issuance")
	}
	grant, err := service.Issue(context.Background(), controlPlaneURI, request)
	if err != nil || grant.Token.Value() != "synthetic-registration-token" || grant.Repository != (githubapp.RepositoryAccess{Owner: "acme", Name: "api"}) || started != 1 || minted != 1 || failed != 0 {
		t.Fatalf("runner token grant=%v repository=%+v counts=(%d,%d,%d) error=%v", grant.Token, grant.Repository, started, minted, failed, err)
	}
	if strings.Contains(grant.Token.String(), grant.Token.Value()) {
		t.Fatal("runner token formatting exposed its value")
	}
}

func TestActionsRunnerRegistrationHTTPIsControlPlaneOnlyAndNoStore(t *testing.T) {
	now := time.Now().UTC()
	identity := actionsRunnerIdentity(now)
	service, err := NewActionsRunnerRegistrationService(func(context.Context, ActionsRunnerRegistrationRequest) (ActionsRunnerJobIdentity, error) {
		return identity, nil
	},
		func(context.Context, ActionsRunnerJobIdentity) error { return nil },
		func(context.Context, ActionsRunnerJobIdentity) (githubapp.RunnerRegistrationToken, githubapp.RepositoryAccess, error) {
			token, err := githubapp.NewRunnerRegistrationToken("synthetic-registration-token", now.Add(time.Hour))
			return token, githubapp.RepositoryAccess{Owner: "acme", Name: "api"}, err
		})
	if err != nil {
		t.Fatal(err)
	}
	service.clock = func() time.Time { return now }
	handler := HandlerWithActionsRunner(nil, nil, nil, nil, service)
	body := `{"integration_id":"integration_runner","workflow_job_id":99,"run_attempt":1,"claim_id":"controller-claim-1"}`
	call := func(uri string, verified bool, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/actions-runner/registration-token", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		parsed, _ := url.Parse(uri)
		certificate := &x509.Certificate{URIs: []*url.URL{parsed}}
		request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}}
		if verified {
			request.TLS.VerifiedChains = [][]*x509.Certificate{{certificate}}
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := call(controlPlaneURI, false, body); response.Code != http.StatusUnauthorized {
		t.Fatalf("unverified Control Plane status=%d", response.Code)
	}
	if response := call("spiffe://hako/resource-plane/rp-one", true, body); response.Code != http.StatusUnauthorized {
		t.Fatalf("Resource Plane token endpoint status=%d", response.Code)
	}
	if response := call(controlPlaneURI, true, `{"integration_id":"integration_runner","workflow_job_id":99,"run_attempt":1,"claim_id":"controller-claim-1","tenant_id":"tenant_runner"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown caller-selected Tenant field status=%d", response.Code)
	}
	response := call(controlPlaneURI, true, body)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), "synthetic-registration-token") || !strings.Contains(response.Body.String(), `"owner":"acme"`) {
		t.Fatalf("Control Plane registration token response status=%d cache=%q body=%s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil || decoded["expires_at"] == nil {
		t.Fatalf("invalid runner token response=%+v error=%v", decoded, err)
	}
}
