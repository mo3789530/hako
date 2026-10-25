package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestShowAgentPublicationCLIPrintsSafeStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tenants/tenant_publish/workload-runs/run_publish/agent-publication" ||
			r.Header.Get("Authorization") != "Bearer publication-access" {
			t.Errorf("unexpected publication status request %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"tenant_id":"tenant_publish","workload_run_id":"run_publish","state":"branch_created","base_branch":"main","branch_name":"hako/agent/proposal-abc","branch_commit_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:01:00Z"}`)
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	previous := cliCredentialGetter
	cliCredentialGetter = workloadCLICredentials(fmt.Sprintf(`{"access_token":"publication-access","token_type":"Bearer","expires_in":3600,"stored_at":%q}`,
		time.Now().UTC().Format(time.RFC3339)))
	t.Cleanup(func() { cliCredentialGetter = previous })

	var output strings.Builder
	if err := showAgentPublicationCLI([]string{"tenant_publish", "run_publish"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"state":"branch_created"`) || strings.Contains(output.String(), "storage_ref") ||
		strings.Contains(output.String(), "token") || strings.Contains(output.String(), "secret") {
		t.Fatalf("CLI output did not contain only safe publication status: %s", output.String())
	}
}

func TestSubmitAgentPublicationCLIUsesSafeRetryEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tenants/tenant_publish/workload-runs/run_publish/agent-publication/publish" ||
			r.Header.Get("Authorization") != "Bearer publication-access" || r.ContentLength != 0 {
			t.Errorf("unexpected publication submit request %s %s length=%d", r.Method, r.URL.Path, r.ContentLength)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = fmt.Fprint(w, `{"tenant_id":"tenant_publish","workload_run_id":"run_publish","state":"pull_request_created","base_branch":"main","branch_name":"hako/proposal-abc","branch_commit_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","pull_request_number":23}`)
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	previous := cliCredentialGetter
	cliCredentialGetter = workloadCLICredentials(fmt.Sprintf(`{"access_token":"publication-access","token_type":"Bearer","expires_in":3600,"stored_at":%q}`,
		time.Now().UTC().Format(time.RFC3339)))
	t.Cleanup(func() { cliCredentialGetter = previous })
	var output strings.Builder
	if err := submitAgentPublicationCLI([]string{"tenant_publish", "run_publish"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"state":"pull_request_created"`) || strings.Contains(output.String(), "token") ||
		strings.Contains(output.String(), "storage_ref") || strings.Contains(output.String(), "secret") {
		t.Fatalf("CLI submit exposed unsafe data or lost state: %s", output.String())
	}
}
