package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type workspaceBasicCLICredentials string

func (c workspaceBasicCLICredentials) Get(string, string) (string, error) { return string(c), nil }

type workspaceBasicCLIWriterError struct{}

func (workspaceBasicCLIWriterError) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

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

func TestWorkspaceBasicCLIDispatchRendersCreateListAndGet(t *testing.T) {
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
		case r.Method == http.MethodPost:
			if r.URL.Path != "/v1/tenants/tenant_cli/workspaces" || r.Header.Get("Idempotency-Key") != "request-cli-1" {
				t.Errorf("create request = %s %s idempotency=%q", r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"))
			}
			_, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusAccepted)
			_, _ = fmt.Fprint(w, `{"workspace":{"id":"ws_cli","tenant_id":"tenant_cli","owner_id":"user_cli","name":"api-dev"},"status":{"desired_state":"running","observed_state":"provisioning"},"operation_id":"op_cli","operation_state":"pending"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/tenant_cli/workspaces" && r.URL.Query().Get("limit") == "5" && r.URL.Query().Get("offset") == "10":
			_, _ = fmt.Fprint(w, `{"items":[{"workspace":{"id":"ws_cli","name":"api-dev"},"status":{"desired_state":"running","observed_state":"provisioning"}}],"total":1,"limit":5,"offset":10}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/tenant_cli/workspaces/ws_cli":
			_, _ = fmt.Fprint(w, `{"workspace":{"id":"ws_cli","name":"api-dev"},"status":{"desired_state":"running","observed_state":"provisioning"}}`)
		default:
			t.Errorf("unexpected CLI API request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	setWorkspaceBasicCLICredentials(t, token)

	var stdout, stderr bytes.Buffer
	if code := runWorkspaceBasicCLI("create", []string{"tenant_cli", "api-dev", "--idempotency-key", "request-cli-1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("create CLI code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Workspace api-dev accepted: ws_cli") || !strings.Contains(stdout.String(), "request-key=request-cli-1") {
		t.Fatalf("create CLI output = %q", stdout.String())
	}
	stdout.Reset()
	if code := runWorkspaceBasicCLI("list", []string{"tenant_cli", "5", "10"}, &stdout, &stderr); code != 0 {
		t.Fatalf("list CLI code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ws_cli\tapi-dev\trunning\tprovisioning") || !strings.Contains(stdout.String(), "Showing 1 of 1 workspaces (limit=5 offset=10)") {
		t.Fatalf("list CLI output = %q", stdout.String())
	}
	stdout.Reset()
	if code := runWorkspaceBasicCLI("get", []string{"tenant_cli", "ws_cli"}, &stdout, &stderr); code != 0 {
		t.Fatalf("get CLI code=%d stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); got != "ws_cli\tapi-dev\trunning\tprovisioning\n" {
		t.Fatalf("get CLI output = %q", got)
	}
	if requests != 3 {
		t.Fatalf("request count = %d, want 3", requests)
	}
}

func TestWorkspaceBasicCLIDispatchRejectsInvalidArgumentsAndOutputFailure(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"workspace":{"id":"ws_cli","name":"api-dev"},"status":{"desired_state":"running","observed_state":"provisioning"},"operation_id":"op_cli","operation_state":"pending"}`)
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	setWorkspaceBasicCLICredentials(t, "workspace-basic-cli-token")

	var stdout, stderr bytes.Buffer
	if code := runWorkspaceBasicCLI("create", []string{"tenant_cli", "api-dev", "--bad-option", "value"}, &stdout, &stderr); code != 2 {
		t.Fatalf("invalid create arguments exit code = %d, stderr=%q", code, stderr.String())
	}
	if code := runWorkspaceBasicCLI("list", []string{"tenant_cli", "not-a-limit"}, &stdout, &stderr); code != 2 {
		t.Fatalf("invalid list page exit code = %d, stderr=%q", code, stderr.String())
	}
	if requests != 0 {
		t.Fatalf("invalid arguments reached API %d times", requests)
	}
	if code := runWorkspaceBasicCLI("create", []string{"tenant_cli", "api-dev", "--idempotency-key", "request-cli-2"}, workspaceBasicCLIWriterError{}, &stderr); code != 1 {
		t.Fatalf("create output failure exit code = %d, stderr=%q", code, stderr.String())
	}
	if requests != 1 {
		t.Fatalf("accepted create request count = %d, want 1", requests)
	}
	if code := runWorkspaceBasicCLI("unknown", nil, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "unsupported Workspace command") {
		t.Fatalf("unknown command code=%d stderr=%q", code, stderr.String())
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
