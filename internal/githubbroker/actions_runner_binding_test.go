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
)

func TestActionsRunnerBindingRequiresAllowlistedPlaneAndExactLiveClaim(t *testing.T) {
	request := ActionsRunnerBindingRequest{IntegrationID: "integration_runner", WorkflowJobID: 99, RunAttempt: 1, ClaimID: "runner-claim-1", MicroVMID: "microvm-runner-1"}
	bound := false
	service, err := NewActionsRunnerBindingService([]string{"rp-tokyo-01"}, func(_ context.Context, plane string, got ActionsRunnerBindingRequest) (bool, error) {
		if plane != "rp-tokyo-01" || got != request {
			t.Fatalf("unexpected claim bind identity: plane=%q request=%+v", plane, got)
		}
		bound = true
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Bind(context.Background(), "spiffe://hako/resource-plane/rp-us-01", request); err == nil || bound {
		t.Fatal("non-allowlisted Resource Plane bound a MicroVM")
	}
	if err := service.Bind(context.Background(), "spiffe://hako/resource-plane/rp-tokyo-01", request); err != nil || !bound {
		t.Fatalf("authorized claim bind failed: bound=%v err=%v", bound, err)
	}
	for name, invalid := range map[string]ActionsRunnerBindingRequest{
		"invalid attempt":     {IntegrationID: request.IntegrationID, WorkflowJobID: request.WorkflowJobID, ClaimID: request.ClaimID, MicroVMID: request.MicroVMID},
		"invalid provider id": {IntegrationID: request.IntegrationID, WorkflowJobID: request.WorkflowJobID, RunAttempt: 1, ClaimID: request.ClaimID, MicroVMID: " vm"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := service.Bind(context.Background(), "spiffe://hako/resource-plane/rp-tokyo-01", invalid); err == nil {
				t.Fatal("invalid binding request accepted")
			}
		})
	}
}

func TestActionsRunnerBindingHTTPIsResourcePlaneOnlyAndNoStore(t *testing.T) {
	request := ActionsRunnerBindingRequest{IntegrationID: "integration_runner", WorkflowJobID: 99, RunAttempt: 1, ClaimID: "runner-claim-1", MicroVMID: "microvm-runner-1"}
	service, err := NewActionsRunnerBindingService([]string{"rp-tokyo-01"}, func(_ context.Context, plane string, got ActionsRunnerBindingRequest) (bool, error) {
		return plane == "rp-tokyo-01" && got == request, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := HandlerWithActionsRunnerLifecycle(nil, nil, nil, nil, nil, nil, nil, service, []string{"spiffe://hako/resource-plane/rp-tokyo-01"})
	body, _ := json.Marshal(request)
	call := func(caller string, verified bool, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/actions-runner/bind", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		parsed, _ := url.Parse(caller)
		certificate := &x509.Certificate{URIs: []*url.URL{parsed}}
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}}
		if verified {
			req.TLS.VerifiedChains = [][]*x509.Certificate{{certificate}}
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	if response := call("spiffe://hako/resource-plane/rp-tokyo-01", false, string(body)); response.Code != http.StatusUnauthorized {
		t.Fatalf("unverified Resource Plane cert status=%d", response.Code)
	}
	if response := call("spiffe://hako/resource-plane/rp-us-01", true, string(body)); response.Code != http.StatusUnauthorized {
		t.Fatalf("non-allowlisted Resource Plane status=%d", response.Code)
	}
	if response := call(controlPlaneURI, true, string(body)); response.Code != http.StatusUnauthorized {
		t.Fatalf("Control Plane caller reached Resource Plane binding route: status=%d", response.Code)
	}
	if response := call("spiffe://hako/resource-plane/rp-tokyo-01", true, `{"integration_id":"integration_runner","workflow_job_id":99,"run_attempt":1,"claim_id":"runner-claim-1","microvm_id":"microvm-runner-1","tenant_id":"tenant_runner"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("caller-selected Tenant ID accepted: status=%d", response.Code)
	}
	response := call("spiffe://hako/resource-plane/rp-tokyo-01", true, string(body))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("valid binding request status=%d cache=%q body=%s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &map[string]any{}); err != nil {
		t.Fatalf("invalid bind response: %v", err)
	}
}
