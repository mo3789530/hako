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

func TestActionsRunnerBootstrapTokenIsResourcePlaneScopedAndClaimBound(t *testing.T) {
	now := time.Now().UTC()
	identity := actionsRunnerIdentity(now)
	identity.ResourcePlaneID = "rp-tokyo-01"
	identity.LambdaMicroVMID = "runner-microvm-1"
	started, failed, minted := 0, 0, 0
	service, err := NewActionsRunnerBootstrapTokenService([]string{"rp-tokyo-01"}, func(_ context.Context, planeID string, request ActionsRunnerRegistrationRequest) (ActionsRunnerJobIdentity, error) {
		started++
		if planeID != identity.ResourcePlaneID || request.IntegrationID != identity.IntegrationID || request.WorkflowJobID != identity.WorkflowJobID || request.RunAttempt != identity.RunAttempt || request.ClaimID != identity.ClaimID {
			t.Fatal("bootstrap request was not constrained to the exact Resource Plane and claim")
		}
		return identity, nil
	}, func(context.Context, ActionsRunnerJobIdentity) error { failed++; return nil }, func(context.Context, ActionsRunnerJobIdentity) (githubapp.RunnerRegistrationToken, githubapp.RepositoryAccess, error) {
		minted++
		token, err := githubapp.NewRunnerRegistrationToken("synthetic-registration-token", now.Add(time.Hour))
		return token, githubapp.RepositoryAccess{Owner: "acme", Name: "api"}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	service.clock = func() time.Time { return now }
	request := ActionsRunnerRegistrationRequest{IntegrationID: identity.IntegrationID, WorkflowJobID: identity.WorkflowJobID, RunAttempt: identity.RunAttempt, ClaimID: identity.ClaimID}
	if _, err := service.Issue(context.Background(), controlPlaneURI, request); err == nil || started != 0 {
		t.Fatal("Control Plane identity used Resource Plane bootstrap route")
	}
	if _, err := service.Issue(context.Background(), "spiffe://hako/resource-plane/rp-us-01", request); err == nil || started != 0 {
		t.Fatal("unconfigured Resource Plane reached bootstrap token starter")
	}
	grant, err := service.Issue(context.Background(), "spiffe://hako/resource-plane/rp-tokyo-01", request)
	if err != nil || grant.Token.Value() != "synthetic-registration-token" || grant.Repository != (githubapp.RepositoryAccess{Owner: "acme", Name: "api"}) || started != 1 || minted != 1 || failed != 0 {
		t.Fatalf("bootstrap grant=%v repo=%+v counts=%d/%d/%d err=%v", grant.Token, grant.Repository, started, minted, failed, err)
	}
}

func TestActionsRunnerBootstrapTokenHTTPRequiresVerifiedAllowedResourcePlane(t *testing.T) {
	now := time.Now().UTC()
	identity := actionsRunnerIdentity(now)
	identity.ResourcePlaneID = "rp-tokyo-01"
	identity.LambdaMicroVMID = "runner-microvm-1"
	service, err := NewActionsRunnerBootstrapTokenService([]string{"rp-tokyo-01"}, func(context.Context, string, ActionsRunnerRegistrationRequest) (ActionsRunnerJobIdentity, error) {
		return identity, nil
	}, func(context.Context, ActionsRunnerJobIdentity) error { return nil }, func(context.Context, ActionsRunnerJobIdentity) (githubapp.RunnerRegistrationToken, githubapp.RepositoryAccess, error) {
		token, err := githubapp.NewRunnerRegistrationToken("synthetic-registration-token", now.Add(time.Hour))
		return token, githubapp.RepositoryAccess{Owner: "acme", Name: "api"}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	service.clock = func() time.Time { return now }
	handler := HandlerWithActionsRunnerBrokerLifecycle(nil, nil, nil, nil, nil, nil, nil, nil, nil, service, []string{"spiffe://hako/resource-plane/rp-tokyo-01"})
	body := `{"integration_id":"integration_runner","workflow_job_id":99,"run_attempt":1,"claim_id":"controller-claim-1"}`
	call := func(uri string, verified bool, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/actions-runner/bootstrap-token", strings.NewReader(body))
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
	if response := call("spiffe://hako/resource-plane/rp-tokyo-01", false, body); response.Code != http.StatusUnauthorized {
		t.Fatalf("unverified plane status=%d", response.Code)
	}
	if response := call(controlPlaneURI, true, body); response.Code != http.StatusUnauthorized {
		t.Fatalf("Control Plane used Resource Plane-only route: status=%d", response.Code)
	}
	if response := call("spiffe://hako/resource-plane/rp-us-01", true, body); response.Code != http.StatusUnauthorized {
		t.Fatalf("unconfigured plane status=%d", response.Code)
	}
	if response := call("spiffe://hako/resource-plane/rp-tokyo-01", true, `{"integration_id":"integration_runner","workflow_job_id":99,"run_attempt":1,"claim_id":"controller-claim-1","tenant_id":"attacker"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("caller-selected Tenant coordinate was accepted: status=%d", response.Code)
	}
	response := call("spiffe://hako/resource-plane/rp-tokyo-01", true, body)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), "synthetic-registration-token") {
		t.Fatalf("valid bootstrap token response status=%d cache=%q body=%s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil || decoded["expires_at"] == nil {
		t.Fatalf("bootstrap grant response=%+v err=%v", decoded, err)
	}
}
