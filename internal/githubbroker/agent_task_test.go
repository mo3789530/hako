package githubbroker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/agenttask"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/workloads"
)

func validAgentTaskFixture() workloads.AgentTaskRecord {
	commitSHA := strings.Repeat("a", 40)
	task := workloads.AgentTaskRecord{TenantID: "tenant_agent", WorkloadRunID: "run_agent", RequestedBy: "usr_agent", IntegrationID: "integration_agent",
		DeliveryID: "a1b2c3d4-e5f6-4789-8abc-def012345678", InstallationID: 17, RepositoryID: 18, IssueNumber: 19,
		CommentID: 20, DefaultBranch: "main", CommitSHA: commitSHA, Title: "Fix bug", Body: "details",
		CreatedAt: time.Now().UTC()}
	task.ContextSHA256 = agenttask.SHA256(agenttask.Context{TenantID: string(task.TenantID), WorkloadRunID: string(task.WorkloadRunID),
		IntegrationID: task.IntegrationID, DeliveryID: task.DeliveryID, InstallationID: task.InstallationID,
		RepositoryID: task.RepositoryID, IssueNumber: task.IssueNumber, CommentID: task.CommentID,
		DefaultBranch: task.DefaultBranch, CommitSHA: task.CommitSHA, Title: task.Title, Body: task.Body})
	return task
}

func validAgentTaskIdentityFixture() AgentTaskIdentity {
	return AgentTaskIdentity{TenantID: domain.TenantID("tenant_agent"), WorkloadRunID: domain.WorkloadRunID("run_agent"),
		AttemptID: "attempt_agent", Generation: 1, ResourcePlaneID: "rp-tokyo-01"}
}

func TestAgentTaskServiceRequiresAllowlistedPlaneAndMatchingIdentity(t *testing.T) {
	identity := validAgentTaskIdentityFixture()
	task := validAgentTaskFixture()
	resolveCalls := 0
	service, err := NewAgentTaskService([]string{"rp-tokyo-01"}, func(_ context.Context, caller string, got AgentTaskIdentity) (workloads.AgentTaskRecord, error) {
		resolveCalls++
		if caller != resourcePlaneURIPrefix+"rp-tokyo-01" || got != identity {
			t.Fatalf("unexpected resolver input: caller=%q identity=%+v", caller, got)
		}
		return task, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resolve(context.Background(), resourcePlaneURIPrefix+"rp-us-01", identity); err == nil || resolveCalls != 0 {
		t.Fatalf("foreign Plane resolved task: calls=%d error=%v", resolveCalls, err)
	}
	got, err := service.Resolve(context.Background(), resourcePlaneURIPrefix+"rp-tokyo-01", identity)
	if err != nil || got.Body != task.Body || resolveCalls != 1 {
		t.Fatalf("authorized task=%+v calls=%d error=%v", got, resolveCalls, err)
	}
	wrongTask := task
	wrongTask.WorkloadRunID = "run_other"
	service, _ = NewAgentTaskService([]string{"rp-tokyo-01"}, func(context.Context, string, AgentTaskIdentity) (workloads.AgentTaskRecord, error) {
		return wrongTask, nil
	})
	if _, err := service.Resolve(context.Background(), resourcePlaneURIPrefix+"rp-tokyo-01", identity); err == nil {
		t.Fatal("cross-Run task was returned")
	}
}

func TestAgentTaskRouteRequiresVerifiedAllowlistedResourcePlaneAndNoStore(t *testing.T) {
	identity := validAgentTaskIdentityFixture()
	task := validAgentTaskFixture()
	service, err := NewAgentTaskService([]string{"rp-tokyo-01"}, func(_ context.Context, caller string, got AgentTaskIdentity) (workloads.AgentTaskRecord, error) {
		if caller != resourcePlaneURIPrefix+"rp-tokyo-01" || got != identity {
			return workloads.AgentTaskRecord{}, ErrAgentTaskUnavailable
		}
		return task, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := WithAgentTask(http.NotFoundHandler(), service)
	call := func(body, caller string, verified bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, agentTaskRoute, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if caller != "" {
			u, _ := url.Parse(caller)
			cert := &x509.Certificate{URIs: []*url.URL{u}}
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
			if verified {
				req.TLS.VerifiedChains = [][]*x509.Certificate{{cert}}
			}
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder
	}
	body := `{"tenant_id":"tenant_agent","workload_run_id":"run_agent","attempt_id":"attempt_agent","generation":1,"resource_plane_id":"rp-tokyo-01"}`
	if got := call(body, resourcePlaneURIPrefix+"rp-tokyo-01", false); got.Code != http.StatusUnauthorized {
		t.Fatalf("unverified cert status=%d", got.Code)
	}
	if got := call(body, resourcePlaneURIPrefix+"rp-us-01", true); got.Code != http.StatusForbidden {
		t.Fatalf("foreign Plane status=%d", got.Code)
	}
	if got := call(strings.TrimSuffix(body, "}")+`,"issue_body":"attacker"}`, resourcePlaneURIPrefix+"rp-tokyo-01", true); got.Code != http.StatusBadRequest {
		t.Fatalf("unknown request field status=%d", got.Code)
	}
	if got := call(body, resourcePlaneURIPrefix+"rp-tokyo-01", true); got.Code != http.StatusOK || got.Header().Get("Cache-Control") != "no-store" || !strings.Contains(got.Body.String(), "details") {
		t.Fatalf("authorized response status=%d cache=%q body=%q", got.Code, got.Header().Get("Cache-Control"), got.Body.String())
	}
	get := httptest.NewRequest(http.MethodGet, agentTaskRoute, nil)
	get.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{URIs: []*url.URL{{Scheme: "spiffe", Host: "hako", Path: "/resource-plane/rp-tokyo-01"}}}}, VerifiedChains: [][]*x509.Certificate{{{}}}}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, get)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d", recorder.Code)
	}
}

func TestAgentTaskServiceRequiresConfiguredPlaneAndValidIdentity(t *testing.T) {
	if _, err := NewAgentTaskService(nil, func(context.Context, string, AgentTaskIdentity) (workloads.AgentTaskRecord, error) {
		return workloads.AgentTaskRecord{}, nil
	}); err == nil {
		t.Fatal("empty Plane allowlist accepted")
	}
	if _, err := NewAgentTaskService([]string{"rp/tokyo"}, func(context.Context, string, AgentTaskIdentity) (workloads.AgentTaskRecord, error) {
		return workloads.AgentTaskRecord{}, nil
	}); err == nil {
		t.Fatal("invalid Plane allowlist accepted")
	}
}
