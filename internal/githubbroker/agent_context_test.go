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

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubwebhook"
	storegithubwebhook "github.com/mo3789530/hako/internal/store/githubwebhook"
)

func agentContextPin() AgentIssueContextPin {
	return AgentIssueContextPin{Authorization: storegithubwebhook.IssueCommentAgentAuthorization{
		TenantID: domain.TenantID("tenant_agent"), UserID: "usr_agent", Installation: 77,
		Repository: githubwebhook.RepositoryRef{GitHubID: 88, Owner: "acme", Name: "api"}, IssueNumber: 17, CommentID: 19},
		IntegrationID: "integration_agent", App: githubapp.TenantPrivateKeyAccess{TenantID: "tenant_agent", IntegrationID: "integration_agent",
			GitHubAppID: 12, ConfigRevision: 3, CredentialGeneration: 4, ReaderRoleARN: "arn:aws:iam::123456789012:role/hako-reader",
			SecretARN: "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:app-key", SecretVersionID: "version-123"}}
}

func TestAgentIssueContextServiceRevalidatesAuthorizationAfterGitHubRead(t *testing.T) {
	pin := agentContextPin()
	resolveCalls, readCalls := 0, 0
	service, err := NewAgentIssueContextService(func(_ context.Context, integrationID, deliveryID string) (AgentIssueContextPin, error) {
		resolveCalls++
		if integrationID != pin.IntegrationID || deliveryID != "a1b2c3d4-e5f6-4789-8abc-def012345678" {
			return AgentIssueContextPin{}, ErrAgentIssueContextUnavailable
		}
		return pin, nil
	}, func(_ context.Context, got AgentIssueContextPin) (githubapp.AgentIssueContext, error) {
		readCalls++
		if !sameAgentIssueContextPin(pin, got) {
			return githubapp.AgentIssueContext{}, ErrAgentIssueContextUnavailable
		}
		return githubapp.AgentIssueContext{IssueNumber: 17, Title: "Fix bug", Body: "details", CommitSHA: strings.Repeat("a", 40), DefaultRef: "main"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.Resolve(context.Background(), pin.IntegrationID, "a1b2c3d4-e5f6-4789-8abc-def012345678")
	if err != nil || got.IssueNumber != 17 || resolveCalls != 2 || readCalls != 1 {
		t.Fatalf("Agent context=%+v resolve=%d read=%d error=%v", got, resolveCalls, readCalls, err)
	}

	resolveCalls, readCalls = 0, 0
	service, err = NewAgentIssueContextService(func(context.Context, string, string) (AgentIssueContextPin, error) {
		resolveCalls++
		if resolveCalls == 1 {
			return pin, nil
		}
		rotated := pin
		rotated.App.CredentialGeneration++
		return rotated, nil
	}, func(context.Context, AgentIssueContextPin) (githubapp.AgentIssueContext, error) {
		readCalls++
		return githubapp.AgentIssueContext{IssueNumber: 17, Title: "Fix bug", CommitSHA: strings.Repeat("a", 40), DefaultRef: "main"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resolve(context.Background(), pin.IntegrationID, "a1b2c3d4-e5f6-4789-8abc-def012345678"); err == nil || resolveCalls != 2 || readCalls != 1 {
		t.Fatalf("service returned context after credential rotation: resolve=%d read=%d error=%v", resolveCalls, readCalls, err)
	}
}

func TestAgentIssueContextRouteRequiresControlPlaneMTLSAndStrictRequest(t *testing.T) {
	issue := githubapp.AgentIssueContext{IssueNumber: 17, Title: "Fix bug", Body: "details", CommitSHA: strings.Repeat("b", 40), DefaultRef: "main"}
	service, err := NewAgentIssueContextService(func(_ context.Context, integrationID, deliveryID string) (AgentIssueContextPin, error) {
		if integrationID != "integration_agent" || deliveryID != "a1b2c3d4-e5f6-4789-8abc-def012345678" {
			return AgentIssueContextPin{}, ErrAgentIssueContextUnavailable
		}
		return agentContextPin(), nil
	}, func(context.Context, AgentIssueContextPin) (githubapp.AgentIssueContext, error) { return issue, nil })
	if err != nil {
		t.Fatal(err)
	}
	handler := WithAgentIssueContext(http.NotFoundHandler(), service)
	invoke := func(method, body string, caller string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, agentIssueContextRoute, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if caller != "" {
			u, _ := url.Parse(caller)
			cert := &x509.Certificate{URIs: []*url.URL{u}}
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder
	}
	valid := `{"integration_id":"integration_agent","delivery_id":"a1b2c3d4-e5f6-4789-8abc-def012345678"}`
	if got := invoke(http.MethodPost, valid, ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated response status = %d", got.Code)
	}
	if got := invoke(http.MethodPost, valid, "spiffe://hako/resource-plane/rp-tokyo-01"); got.Code != http.StatusUnauthorized {
		t.Fatalf("Resource Plane response status = %d", got.Code)
	}
	controlPlane := "spiffe://hako/control-plane"
	if got := invoke(http.MethodPost, `{"integration_id":"integration_agent","delivery_id":"a1b2c3d4-e5f6-4789-8abc-def012345678","tenant_id":"attacker"}`, controlPlane); got.Code != http.StatusBadRequest {
		t.Fatalf("unknown field response status = %d", got.Code)
	}
	if got := invoke(http.MethodPost, valid, controlPlane); got.Code != http.StatusOK || got.Header().Get("Cache-Control") != "no-store" || !strings.Contains(got.Body.String(), issue.CommitSHA) {
		t.Fatalf("Control Plane response status=%d header=%q body=%s", got.Code, got.Header().Get("Cache-Control"), got.Body.String())
	}
	if got := invoke(http.MethodGet, "", controlPlane); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method response status = %d", got.Code)
	}
}

func TestAgentIssueContextProviderRejectsNonHTTPSAndBadResponses(t *testing.T) {
	if _, err := NewHTTPAgentIssueContextProvider("http://broker.invalid", &http.Client{}); err == nil {
		t.Fatal("HTTP broker URL accepted")
	}
	for _, handler := range []http.HandlerFunc{
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte(`{"issue_number":17,"title":"x","body":"","commit_sha":"invalid","default_ref":"main"}`))
		},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusFound)
		},
	} {
		server := httptest.NewTLSServer(handler)
		client, err := NewHTTPAgentIssueContextProvider(server.URL, server.Client())
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		_, err = client.Resolve(context.Background(), "integration_agent", "a1b2c3d4-e5f6-4789-8abc-def012345678")
		server.Close()
		if err == nil {
			t.Fatal("invalid broker response accepted")
		}
	}
}
