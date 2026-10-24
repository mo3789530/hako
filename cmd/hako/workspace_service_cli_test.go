package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type workspaceServiceCLICredentials string

func (c workspaceServiceCLICredentials) Get(string, string) (string, error) { return string(c), nil }

func TestWorkspaceServiceCredentialRotationCLIUsesTenantScopedAPI(t *testing.T) {
	const token = "workspace-service-cli-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tenants/tenant_services/workspaces/ws_services/services/postgres/actions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Body == nil {
			t.Fatal("rotation request body is missing")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != `{"action":"rotate-credentials"}` {
			t.Errorf("rotation request body = %q, err=%v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"id":"svc_postgres","workspace_id":"ws_services","name":"postgres","desired_state":"running","observed_state":"running","credential_generation":2}`)
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	previous := cliCredentialGetter
	cliCredentialGetter = workspaceServiceCLICredentials(fmt.Sprintf(`{"access_token":%q,"token_type":"Bearer","expires_in":3600,"stored_at":%q}`,
		token, time.Now().UTC().Format(time.RFC3339)))
	t.Cleanup(func() { cliCredentialGetter = previous })

	service, err := requestWorkspaceServiceAction("rotate-credentials", "tenant_services", "ws_services", "postgres")
	if err != nil {
		t.Fatalf("requestWorkspaceServiceAction: %v", err)
	}
	if service.CredentialGeneration != 2 || service.Name != "postgres" {
		t.Fatalf("credential rotation response = %+v", service)
	}
}
