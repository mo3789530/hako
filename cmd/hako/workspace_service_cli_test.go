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

func TestWorkspaceServiceAddAndListCLIUseTenantScopedAPI(t *testing.T) {
	const token = "workspace-service-cli-token"
	service := `{"id":"svc_postgres","tenant_id":"tenant_services","workspace_id":"ws_services","name":"db","catalog_name":"postgres","desired_state":"running","observed_state":"pending","desired_revision":1}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/tenants/tenant_services/workspaces/ws_services/services" {
			t.Errorf("unexpected path: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodPost:
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != `{"catalog_name":"postgres","name":"db"}` {
				t.Errorf("add request body = %q, err=%v", body, err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = fmt.Fprint(w, service)
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"items":[%s]}`, service)
		default:
			t.Errorf("unexpected method: %s", r.Method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	previous := cliCredentialGetter
	cliCredentialGetter = workspaceServiceCLICredentials(fmt.Sprintf(`{"access_token":%q,"token_type":"Bearer","expires_in":3600,"stored_at":%q}`,
		token, time.Now().UTC().Format(time.RFC3339)))
	t.Cleanup(func() { cliCredentialGetter = previous })

	added, err := addWorkspaceService("tenant_services", "ws_services", "postgres", "db")
	if err != nil || added.ID != "svc_postgres" || added.DesiredState != "running" {
		t.Fatalf("addWorkspaceService() = %+v, %v", added, err)
	}
	listed, err := listWorkspaceServices("tenant_services", "ws_services")
	if err != nil || len(listed.Items) != 1 || listed.Items[0].ID != added.ID {
		t.Fatalf("listWorkspaceServices() = %+v, %v", listed, err)
	}
}

func TestWorkspaceServiceAddAndListCLIRequireLoginAndAPIURL(t *testing.T) {
	for _, test := range []struct {
		name  string
		url   string
		creds string
	}{
		{name: "missing login", url: "http://127.0.0.1:1", creds: ""},
		{name: "missing API URL", url: "", creds: `{"access_token":"service-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HAKO_API_URL", test.url)
			previous := cliCredentialGetter
			cliCredentialGetter = workspaceServiceCLICredentials(test.creds)
			t.Cleanup(func() { cliCredentialGetter = previous })
			if _, err := addWorkspaceService("tenant_services", "ws_services", "postgres", "db"); err == nil {
				t.Fatal("addWorkspaceService accepted missing configuration")
			}
			if _, err := listWorkspaceServices("tenant_services", "ws_services"); err == nil {
				t.Fatal("listWorkspaceServices accepted missing configuration")
			}
		})
	}
}

func TestWorkspaceServiceActionCLIStartsServiceThroughAuthorizedAPI(t *testing.T) {
	const token = "workspace-service-action-token"
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
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != `{"action":"start"}` {
			t.Errorf("action request body = %q, err=%v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"id":"svc_postgres","name":"postgres","desired_state":"running","observed_state":"pending"}`)
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	previous := cliCredentialGetter
	cliCredentialGetter = workspaceServiceCLICredentials(fmt.Sprintf(`{"access_token":%q,"token_type":"Bearer","expires_in":3600,"stored_at":%q}`,
		token, time.Now().UTC().Format(time.RFC3339)))
	t.Cleanup(func() { cliCredentialGetter = previous })

	var stdout, stderr bytes.Buffer
	code := runWorkspaceServiceActionCLI([]string{"start", "tenant_services", "ws_services", "postgres"}, nil, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "Service postgres start accepted (desired=running observed=pending)") {
		t.Fatalf("service action CLI code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestWorkspaceServiceActionCLIRemovalCancellationDoesNotCallAPI(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	var stdout, stderr bytes.Buffer
	code := runWorkspaceServiceActionCLI([]string{"remove", "tenant_services", "ws_services", "postgres"},
		strings.NewReader("different-name\n"), &stdout, &stderr)
	if code != 0 || requests != 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "Workspace Service removal cancelled") {
		t.Fatalf("cancelled remove code=%d requests=%d stdout=%q stderr=%q", code, requests, stdout.String(), stderr.String())
	}
}

func TestWorkspaceServiceActionCLIDisallowsAutomationFlagForStart(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	var stdout, stderr bytes.Buffer
	code := runWorkspaceServiceActionCLI([]string{"start", "tenant_services", "ws_services", "postgres", "--yes"}, nil, &stdout, &stderr)
	if code != 2 || requests != 0 || !strings.Contains(stderr.String(), "--yes is supported only for service remove") {
		t.Fatalf("start --yes code=%d requests=%d stderr=%q", code, requests, stderr.String())
	}
}

func TestWorkspaceServiceActionCLIHandlesOutputAndConfirmationFailures(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"id":"svc_postgres","name":"postgres","desired_state":"running","observed_state":"pending"}`)
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	previous := cliCredentialGetter
	cliCredentialGetter = workspaceServiceCLICredentials(fmt.Sprintf(`{"access_token":"workspace-service-action-token","token_type":"Bearer","expires_in":3600,"stored_at":%q}`,
		time.Now().UTC().Format(time.RFC3339)))
	t.Cleanup(func() { cliCredentialGetter = previous })

	args := []string{"start", "tenant_services", "ws_services", "postgres"}
	var stderr bytes.Buffer
	if code := runWorkspaceServiceActionCLI(args, nil, nil, &stderr); code != 2 {
		t.Fatalf("nil stdout exit code = %d", code)
	}
	if code := runWorkspaceServiceActionCLI(args, nil, &bytes.Buffer{}, nil); code != 2 {
		t.Fatalf("nil stderr exit code = %d", code)
	}
	if requests != 0 {
		t.Fatalf("nil stream calls reached API %d times", requests)
	}
	if code := runWorkspaceServiceActionCLI(args, nil, workspaceBasicCLIWriterError{}, &stderr); code != 1 {
		t.Fatalf("failed success-output exit code = %d stderr=%q", code, stderr.String())
	}
	if requests != 1 || !strings.Contains(stderr.String(), "write output:") {
		t.Fatalf("write failure requests=%d stderr=%q", requests, stderr.String())
	}

	requests = 0
	if code := runWorkspaceServiceActionCLI([]string{"remove", "tenant_services", "ws_services", "postgres"},
		strings.NewReader("postgres\n"), &bytes.Buffer{}, workspaceBasicCLIWriterError{}); code != 1 {
		t.Fatalf("failed confirmation-prompt exit code = %d", code)
	}
	if requests != 0 {
		t.Fatalf("confirmation output failure reached API %d times", requests)
	}
}
