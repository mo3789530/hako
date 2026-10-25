package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type workspaceBasicCLICredentials string

func (c workspaceBasicCLICredentials) Get(string, string) (string, error) { return string(c), nil }

func TestWorkspaceBasicCLIUsesAuthenticatedTenantScopedAPI(t *testing.T) {
	const token = "workspace-basic-cli-token"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tenants/tenant_cli/workspaces":
			if got := r.Header.Get("Idempotency-Key"); got != "request-create-1" {
				t.Errorf("Idempotency-Key = %q", got)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != `{"name":"api-dev"}` {
				t.Errorf("create body = %q, err=%v", body, err)
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = fmt.Fprint(w, `{"workspace":{"id":"ws_cli","tenant_id":"tenant_cli","owner_id":"user_cli","name":"api-dev"},"status":{"desired_state":"running","observed_state":"provisioning"},"operation_id":"op_cli","operation_state":"pending"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/tenant_cli/workspaces" && r.URL.Query().Get("limit") == "7" && r.URL.Query().Get("offset") == "14":
			_, _ = fmt.Fprint(w, `{"items":[{"workspace":{"id":"ws_cli","tenant_id":"tenant_cli","owner_id":"user_cli","name":"api-dev"},"status":{"desired_state":"running","observed_state":"provisioning"}}],"total":1,"limit":7,"offset":14}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/tenant_cli/workspaces/ws_cli":
			_, _ = fmt.Fprint(w, `{"workspace":{"id":"ws_cli","tenant_id":"tenant_cli","owner_id":"user_cli","name":"api-dev"},"status":{"desired_state":"running","observed_state":"provisioning"}}`)
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	setWorkspaceBasicCLICredentials(t, token)

	created, err := createWorkspace("tenant_cli", "api-dev", "request-create-1")
	if err != nil || created.ID != "ws_cli" || created.OperationID != "op_cli" || created.ObservedState != "provisioning" {
		t.Fatalf("createWorkspace() = %+v, %v", created, err)
	}
	listed, err := listWorkspaces("tenant_cli", 7, 14)
	if err != nil || len(listed.Items) != 1 || listed.Total != 1 || listed.Items[0].Workspace.ID != "ws_cli" {
		t.Fatalf("listWorkspaces() = %+v, %v", listed, err)
	}
	got, err := getWorkspace("tenant_cli", "ws_cli")
	if err != nil || got.Workspace.Name != "api-dev" || got.Status.ObservedState != "provisioning" {
		t.Fatalf("getWorkspace() = %+v, %v", got, err)
	}
	if requests != 3 {
		t.Fatalf("request count = %d, want 3", requests)
	}
}

func TestWorkspaceBasicCLIRequiresLoginAndAPIURL(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()
	previous := cliCredentialGetter
	t.Cleanup(func() { cliCredentialGetter = previous })

	t.Run("missing login", func(t *testing.T) {
		t.Setenv("HAKO_API_URL", server.URL)
		cliCredentialGetter = workspaceBasicCLICredentials("")
		assertWorkspaceBasicCLIConfigurationFails(t)
	})
	t.Run("missing API URL", func(t *testing.T) {
		t.Setenv("HAKO_API_URL", "")
		setWorkspaceBasicCLICredentials(t, "workspace-basic-cli-token")
		assertWorkspaceBasicCLIConfigurationFails(t)
	})
	if requests != 0 {
		t.Fatalf("configuration failures reached API %d times", requests)
	}
}

func TestWorkspaceBasicCLIPropagatesAuthorizationFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer workspace-basic-cli-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		http.Error(w, `{"error":"tenant access denied"}`, http.StatusForbidden)
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	setWorkspaceBasicCLICredentials(t, "workspace-basic-cli-token")

	if _, err := listWorkspaces("tenant_cli", 10, 0); err == nil {
		t.Fatal("listWorkspaces accepted a forbidden Tenant response")
	}
}

func assertWorkspaceBasicCLIConfigurationFails(t *testing.T) {
	t.Helper()
	if _, err := createWorkspace("tenant_cli", "api-dev", "request-create-1"); err == nil {
		t.Error("createWorkspace accepted missing configuration")
	}
	if _, err := listWorkspaces("tenant_cli", 10, 0); err == nil {
		t.Error("listWorkspaces accepted missing configuration")
	}
	if _, err := getWorkspace("tenant_cli", "ws_cli"); err == nil {
		t.Error("getWorkspace accepted missing configuration")
	}
}

func setWorkspaceBasicCLICredentials(t *testing.T, token string) {
	t.Helper()
	previous := cliCredentialGetter
	cliCredentialGetter = workspaceBasicCLICredentials(fmt.Sprintf(`{"access_token":%q,"token_type":"Bearer","expires_in":3600,"stored_at":%q}`,
		token, time.Now().UTC().Format(time.RFC3339)))
	t.Cleanup(func() { cliCredentialGetter = previous })
}
