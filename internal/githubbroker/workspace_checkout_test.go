package githubbroker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/githubapp"
)

func workspaceCheckoutFixture() (WorkspaceCheckoutIdentity, WorkspaceCheckoutResolution, githubapp.TenantPrivateKeyAccess) {
	identity := WorkspaceCheckoutIdentity{TenantID: "tenant_workspace", WorkspaceID: "ws_workspace", ResourcePlaneID: "rp-test"}
	access := githubapp.TenantPrivateKeyAccess{TenantID: string(identity.TenantID), IntegrationID: "integration_workspace", GitHubAppID: 81,
		ConfigRevision: 2, CredentialGeneration: 3, ReaderRoleARN: "arn:aws:iam::123456789012:role/hako/key-reader",
		SecretARN: "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/key-AbCdEf", SecretVersionID: "0123456789abcdef0123456789abcdef"}
	resolved := WorkspaceCheckoutResolution{Identity: identity, Access: access, Repository: githubapp.RepositoryAccess{Owner: "acme", Name: "workspace"},
		Install: 82, RepositoryID: 83, DefaultBranch: "main", CommitSHA: strings.Repeat("a", 40)}
	return identity, resolved, access
}

func TestWorkspaceCheckoutServiceRechecksImmutableTenantPin(t *testing.T) {
	identity, resolved, access := workspaceCheckoutFixture()
	now := time.Now().UTC()
	resolveCalls, mintCalls := 0, 0
	service, err := NewWorkspaceCheckoutService([]string{"rp-test"}, func(_ context.Context, caller string, requested WorkspaceCheckoutIdentity) (WorkspaceCheckoutResolution, error) {
		resolveCalls++
		if caller != "spiffe://hako/resource-plane/rp-test" || requested != identity {
			t.Fatal("resolver received caller-selected identity")
		}
		return resolved, nil
	}, func(_ context.Context, got WorkspaceCheckoutResolution) (string, time.Time, string, error) {
		mintCalls++
		if got != resolved || got.Access != access {
			t.Fatal("token minter did not receive exact Tenant repository pin")
		}
		return "ghs_workspace_token_secret", now.Add(time.Minute), resolved.CommitSHA, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := service.Issue(context.Background(), "spiffe://hako/resource-plane/rp-test", identity)
	if err != nil || grant.Token() != "ghs_workspace_token_secret" || grant.CommitSHA() != resolved.CommitSHA || grant.Repository() != resolved.Repository {
		t.Fatalf("Workspace checkout grant=%s err=%v", grant, err)
	}
	if resolveCalls != 2 || mintCalls != 1 || strings.Contains(grant.String(), grant.Token()) {
		t.Fatalf("resolve=%d mint=%d grant=%s", resolveCalls, mintCalls, grant)
	}

	initial := resolved
	initial.CommitSHA = ""
	resolveCalls = 0
	service, err = NewWorkspaceCheckoutService([]string{"rp-test"}, func(_ context.Context, _ string, _ WorkspaceCheckoutIdentity) (WorkspaceCheckoutResolution, error) {
		resolveCalls++
		if resolveCalls == 1 {
			return initial, nil
		}
		return resolved, nil
	}, func(_ context.Context, got WorkspaceCheckoutResolution) (string, time.Time, string, error) {
		if got.CommitSHA != "" {
			t.Fatal("initial checkout was already expected to have a commit pin")
		}
		return "ghs_workspace_token_secret", now.Add(time.Minute), resolved.CommitSHA, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	grant, err = service.Issue(context.Background(), "spiffe://hako/resource-plane/rp-test", identity)
	if err != nil || grant.CommitSHA() != resolved.CommitSHA {
		t.Fatalf("first checkout commit pin = %s err=%v", grant.CommitSHA(), err)
	}

	changed := resolved
	changed.CommitSHA = strings.Repeat("b", 40)
	resolveCalls = 0
	service, err = NewWorkspaceCheckoutService([]string{"rp-test"}, func(_ context.Context, _ string, _ WorkspaceCheckoutIdentity) (WorkspaceCheckoutResolution, error) {
		resolveCalls++
		if resolveCalls == 1 {
			return resolved, nil
		}
		return changed, nil
	}, func(context.Context, WorkspaceCheckoutResolution) (string, time.Time, string, error) {
		return "ghs_workspace_token_secret", now.Add(time.Minute), resolved.CommitSHA, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Issue(context.Background(), "spiffe://hako/resource-plane/rp-test", identity); err == nil {
		t.Fatal("changed commit pin was accepted")
	}
}

func TestWorkspaceCheckoutHTTPRequiresAuthenticatedPlaneAndOnlyWorkspaceIdentity(t *testing.T) {
	identity, resolved, _ := workspaceCheckoutFixture()
	service, err := NewWorkspaceCheckoutService([]string{"rp-test"}, func(_ context.Context, caller string, got WorkspaceCheckoutIdentity) (WorkspaceCheckoutResolution, error) {
		if caller != "spiffe://hako/resource-plane/rp-test" || got != identity {
			t.Fatalf("request-derived identity = %q %+v", caller, got)
		}
		return resolved, nil
	}, func(context.Context, WorkspaceCheckoutResolution) (string, time.Time, string, error) {
		return "ghs_workspace_token_secret", time.Now().Add(time.Minute), resolved.CommitSHA, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := WithWorkspaceCheckout(nil, service)
	invoke := func(caller string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, workspaceCheckoutRoute, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if caller != "" {
			cert := &x509.Certificate{URIs: []*url.URL{{Scheme: "spiffe", Host: "hako", Path: "/resource-plane/rp-test"}}}
			if caller != "spiffe://hako/resource-plane/rp-test" {
				cert.URIs[0] = &url.URL{Scheme: "spiffe", Host: "hako", Path: "/resource-plane/rp-other"}
			}
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	body := `{"tenant_id":"tenant_workspace","workspace_id":"ws_workspace"}`
	got := invoke("spiffe://hako/resource-plane/rp-test", body)
	if got.Code != http.StatusOK || got.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("authorized checkout response = %d %s", got.Code, got.Body.String())
	}
	var payload struct {
		Token  string             `json:"token"`
		Commit string             `json:"commit_sha"`
		Repo   repositoryResponse `json:"repository"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &payload); err != nil || payload.Token != "ghs_workspace_token_secret" || payload.Commit != resolved.CommitSHA || payload.Repo.Owner != "acme" {
		t.Fatalf("checkout response %+v err=%v", payload, err)
	}
	if got := invoke("", body); got.Code != http.StatusUnauthorized {
		t.Fatalf("missing mTLS identity status=%d", got.Code)
	}
	if got := invoke("spiffe://hako/resource-plane/rp-other", body); got.Code != http.StatusForbidden {
		t.Fatalf("foreign mTLS identity status=%d", got.Code)
	}
	if got := invoke("spiffe://hako/resource-plane/rp-test", `{"tenant_id":"tenant_workspace","workspace_id":"ws_workspace","installation_id":82}`); got.Code != http.StatusBadRequest {
		t.Fatalf("caller-selected repository field status=%d", got.Code)
	}
}

func TestHTTPWorkspaceCheckoutProviderSendsOnlyWorkspaceIdentity(t *testing.T) {
	identity, resolved, _ := workspaceCheckoutFixture()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != workspaceCheckoutRoute || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Workspace checkout request = %s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 2049))
		var fields map[string]json.RawMessage
		if err != nil || json.Unmarshal(body, &fields) != nil || len(fields) != 2 {
			t.Errorf("Workspace checkout request fields=%v error=%v", fields, err)
		}
		if _, ok := fields["installation_id"]; ok {
			t.Error("Resource Plane selected Installation instead of using broker pin")
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Token      string             `json:"token"`
			ExpiresAt  time.Time          `json:"expires_at"`
			Repository repositoryResponse `json:"repository"`
			CommitSHA  string             `json:"commit_sha"`
		}{
			Token: "ghs_workspace_token_secret", ExpiresAt: time.Now().Add(time.Minute), Repository: repositoryResponse{Owner: resolved.Repository.Owner, Name: resolved.Repository.Name}, CommitSHA: resolved.CommitSHA})
	}))
	defer server.Close()
	provider, err := NewHTTPWorkspaceCheckoutProvider(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	provider.now = time.Now
	grant, err := provider.WorkspaceCheckoutToken(context.Background(), identity)
	if err != nil || grant.Token() != "ghs_workspace_token_secret" || grant.CommitSHA() != resolved.CommitSHA || grant.Repository() != resolved.Repository {
		t.Fatalf("Workspace checkout grant=%s err=%v", grant, err)
	}
	if strings.Contains(grant.String(), grant.Token()) {
		t.Fatal("Workspace checkout grant exposed token")
	}
}
