package githubbroker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
)

func TestActionsRunnerCleanupServiceRequiresMatchingPlaneAndLiveClaim(t *testing.T) {
	now := time.Now().UTC()
	request := ActionsRunnerCleanupRequest{IntegrationID: "integration_runner", WorkflowJobID: 99, RunAttempt: 1, ClaimID: "cleanup-claim-1"}
	identity := ActionsRunnerCleanupIdentity{
		ResourcePlaneID: "rp-tokyo-01", IntegrationID: request.IntegrationID, TenantID: "tenant_runner", Installation: 77, Repository: 88,
		WorkflowJobID: request.WorkflowJobID, RunAttempt: request.RunAttempt, RunnerID: 123, ClaimID: request.ClaimID, LeaseExpires: now.Add(time.Minute),
		Credential: githubapp.TenantPrivateKeyAccess{TenantID: "tenant_runner", IntegrationID: request.IntegrationID, GitHubAppID: 12,
			ConfigRevision: 3, CredentialGeneration: 4, SecretARN: "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:key",
			ReaderRoleARN: "arn:aws:iam::123456789012:role/key-reader", SecretVersionID: strings.Repeat("a", 32)},
	}
	resolved, deregistered := 0, 0
	service, err := NewActionsRunnerCleanupService(func(_ context.Context, planeID string, got ActionsRunnerCleanupRequest) (ActionsRunnerCleanupIdentity, error) {
		resolved++
		if got != request {
			t.Fatalf("resolver received plane=%q request=%+v", planeID, got)
		}
		return identity, nil
	}, func(_ context.Context, got ActionsRunnerCleanupIdentity) error {
		deregistered++
		if got.TenantID != identity.TenantID || got.RunnerID != identity.RunnerID {
			t.Fatalf("deregistrator received identity %+v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	service.clock = func() time.Time { return now }
	for _, caller := range []string{controlPlaneURI, "spiffe://hako/resource-plane/invalid/id"} {
		if err := service.Deregister(context.Background(), caller, request); !errors.Is(err, ErrActionsRunnerCleanupDenied) {
			t.Errorf("Deregister(%q) error=%v", caller, err)
		}
	}
	if resolved != 0 || deregistered != 0 {
		t.Fatal("unauthorized caller reached durable resolver or GitHub API")
	}
	if err := service.Deregister(context.Background(), "spiffe://hako/resource-plane/rp-other-01", request); !errors.Is(err, ErrActionsRunnerCleanupDenied) || deregistered != 0 {
		t.Fatalf("other Plane identity was not rejected by pinned cleanup record: error=%v deregistered=%d", err, deregistered)
	}
	if err := service.Deregister(context.Background(), "spiffe://hako/resource-plane/rp-tokyo-01", request); err != nil {
		t.Fatalf("authorized cleanup = %v", err)
	}
	if resolved != 2 || deregistered != 1 {
		t.Fatalf("resolver/deregistration calls = %d/%d", resolved, deregistered)
	}
	identity.LeaseExpires = now
	service.resolve = func(context.Context, string, ActionsRunnerCleanupRequest) (ActionsRunnerCleanupIdentity, error) {
		return identity, nil
	}
	if err := service.Deregister(context.Background(), "spiffe://hako/resource-plane/rp-tokyo-01", request); !errors.Is(err, ErrActionsRunnerCleanupDenied) || deregistered != 1 {
		t.Fatalf("expired lease cleanup error=%v deregistered=%d", err, deregistered)
	}
}

func TestActionsRunnerCleanupHTTPIsPlaneAuthenticatedAndDoesNotAcceptCoordinates(t *testing.T) {
	now := time.Now().UTC()
	request := ActionsRunnerCleanupRequest{IntegrationID: "integration_runner", WorkflowJobID: 99, RunAttempt: 1, ClaimID: "cleanup-claim-1"}
	identity := ActionsRunnerCleanupIdentity{ResourcePlaneID: "rp-tokyo-01", IntegrationID: request.IntegrationID, TenantID: "tenant_runner",
		Installation: 77, Repository: 88, WorkflowJobID: 99, RunAttempt: 1, RunnerID: 123, ClaimID: request.ClaimID, LeaseExpires: now.Add(time.Minute),
		Credential: githubapp.TenantPrivateKeyAccess{TenantID: "tenant_runner", IntegrationID: request.IntegrationID, GitHubAppID: 12, ConfigRevision: 3,
			CredentialGeneration: 4, SecretARN: "secret", ReaderRoleARN: "role", SecretVersionID: strings.Repeat("a", 32)}}
	service, err := NewActionsRunnerCleanupService(func(_ context.Context, planeID string, got ActionsRunnerCleanupRequest) (ActionsRunnerCleanupIdentity, error) {
		if planeID != "rp-tokyo-01" || got != request {
			t.Fatalf("unexpected cleanup request plane=%q request=%+v", planeID, got)
		}
		return identity, nil
	}, func(context.Context, ActionsRunnerCleanupIdentity) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	service.clock = func() time.Time { return now }
	caller := "spiffe://hako/resource-plane/rp-tokyo-01"
	handler := HandlerWithActionsRunnerAndCleanup(nil, nil, nil, nil, nil, service, []string{caller})
	call := func(uri string, verified bool, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/actions-runner/deregister", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		parsed, _ := url.Parse(uri)
		certificate := &x509.Certificate{URIs: []*url.URL{parsed}}
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}}
		if verified {
			req.TLS.VerifiedChains = [][]*x509.Certificate{{certificate}}
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	body := `{"integration_id":"integration_runner","workflow_job_id":99,"run_attempt":1,"claim_id":"cleanup-claim-1"}`
	if response := call(caller, false, body); response.Code != http.StatusUnauthorized {
		t.Fatalf("unverified Plane certificate status=%d", response.Code)
	}
	if response := call(controlPlaneURI, true, body); response.Code != http.StatusUnauthorized {
		t.Fatalf("Control Plane certificate status=%d", response.Code)
	}
	if response := call("spiffe://hako/resource-plane/rp-other-01", true, body); response.Code != http.StatusUnauthorized {
		t.Fatalf("unallowlisted Plane certificate status=%d", response.Code)
	}
	if response := call(caller, true, `{"integration_id":"integration_runner","workflow_job_id":99,"run_attempt":1,"claim_id":"cleanup-claim-1","tenant_id":"tenant_attacker"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("caller-selected Tenant coordinate status=%d", response.Code)
	}
	response := call(caller, true, body)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || response.Body.String() != "{}\n" {
		t.Fatalf("authorized cleanup response status=%d cache=%q body=%q", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
}

func TestHTTPActionsRunnerCleanupClientSendsOnlyDurableClaimKey(t *testing.T) {
	planeID := "rp-tokyo-01"
	runnerID := int64(123)
	job := githubwebhook.ActionsRunnerCleanupJob{IntegrationID: "integration_runner", ResourcePlaneID: &planeID, WorkflowJobID: 99, RunAttempt: 1,
		RunnerID: &runnerID, State: "claimed", ClaimID: "cleanup-claim-1"}
	called := false
	client, err := NewHTTPActionsRunnerCleanupClient("https://broker.example", planeID, &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		called = true
		if request.URL.String() != "https://broker.example/v1/actions-runner/deregister" || request.Method != http.MethodPost {
			t.Fatalf("unexpected broker request %s %s", request.Method, request.URL)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if json.Unmarshal(body, &decoded) != nil || len(decoded) != 4 || decoded["integration_id"] != job.IntegrationID || decoded["workflow_job_id"] != float64(99) || decoded["run_attempt"] != float64(1) || decoded["claim_id"] != job.ClaimID {
			t.Fatalf("cleanup request exposed coordinates or was malformed: %s", bytes.TrimSpace(body))
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Cache-Control": []string{"no-store"}}, Body: io.NopCloser(strings.NewReader("{}\n"))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Deregister(context.Background(), job); err != nil || !called {
		t.Fatalf("Deregister() error=%v called=%v", err, called)
	}
	job.ResourcePlaneID = nil
	if err := client.Deregister(context.Background(), job); !errors.Is(err, ErrActionsRunnerCleanupDenied) {
		t.Fatalf("unbound Resource Plane deregistration error=%v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestActionsRunnerCleanupServiceRejectsInvalidRequestsAndResolverErrors(t *testing.T) {
	resolved := 0
	service, err := NewActionsRunnerCleanupService(func(context.Context, string, ActionsRunnerCleanupRequest) (ActionsRunnerCleanupIdentity, error) {
		resolved++
		return ActionsRunnerCleanupIdentity{}, errors.New("private database detail")
	}, func(context.Context, ActionsRunnerCleanupIdentity) error {
		t.Fatal("deregistrator called after resolver failure")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Deregister(context.Background(), "spiffe://hako/resource-plane/rp-tokyo-01", ActionsRunnerCleanupRequest{IntegrationID: "integration_runner", WorkflowJobID: 99, ClaimID: "cleanup-claim-1"}); !errors.Is(err, ErrActionsRunnerCleanupDenied) || resolved != 0 {
		t.Fatalf("missing attempt request error=%v resolver calls=%d", err, resolved)
	}
	if err := service.Deregister(context.Background(), "spiffe://hako/resource-plane/rp-tokyo-01", ActionsRunnerCleanupRequest{IntegrationID: "integration_runner", WorkflowJobID: 99, RunAttempt: 1, ClaimID: "cleanup-claim-1"}); !errors.Is(err, ErrActionsRunnerCleanupDenied) || resolved != 1 || strings.Contains(err.Error(), "private database detail") {
		t.Fatalf("resolver failure leaked or was not denied: error=%v resolver calls=%d", err, resolved)
	}
}
