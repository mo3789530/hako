package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type publicationCLIWriterError struct{}

func (publicationCLIWriterError) Write([]byte) (int, error) {
	return 0, errors.New("writer unavailable")
}

func setPublicationCLILogin(t *testing.T) {
	t.Helper()
	previous := cliCredentialGetter
	cliCredentialGetter = workloadCLICredentials(fmt.Sprintf(`{"access_token":"publication-access","token_type":"Bearer","expires_in":3600,"stored_at":%q}`,
		time.Now().UTC().Format(time.RFC3339)))
	t.Cleanup(func() { cliCredentialGetter = previous })
}

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
	setPublicationCLILogin(t)

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
	setPublicationCLILogin(t)
	var output strings.Builder
	if err := submitAgentPublicationCLI([]string{"tenant_publish", "run_publish"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"state":"pull_request_created"`) || strings.Contains(output.String(), "token") ||
		strings.Contains(output.String(), "storage_ref") || strings.Contains(output.String(), "secret") {
		t.Fatalf("CLI submit exposed unsafe data or lost state: %s", output.String())
	}
}

func TestSubmitAgentPublicationCLIReportsQueuedVerificationWithoutPublicationDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/agent-publication/publish") {
			t.Errorf("unexpected queued publication request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"tenant_id":"tenant_publish","workload_run_id":"run_publish","state":"verification_queued"}`)
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	setPublicationCLILogin(t)
	var output strings.Builder
	if err := submitAgentPublicationCLI([]string{"tenant_publish", "run_publish"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"state":"verification_queued"`) ||
		!strings.Contains(output.String(), `"branch_name":""`) || strings.Contains(output.String(), "artifact_id") ||
		strings.Contains(output.String(), "proposal_digest") || strings.Contains(output.String(), "token") {
		t.Fatalf("queued verification response exposed publication internals: %s", output.String())
	}
}

func TestAgentPublicationCLIValidatesInputAndFailurePaths(t *testing.T) {
	for _, operation := range []struct {
		name string
		call func([]string, io.Writer) error
	}{
		{name: "show", call: showAgentPublicationCLI},
		{name: "submit", call: submitAgentPublicationCLI},
	} {
		t.Run(operation.name, func(t *testing.T) {
			args := []string{"tenant_publish", "run_publish"}
			for _, test := range []struct {
				name       string
				args       []string
				output     io.Writer
				login      bool
				apiURL     string
				response   string
				statusCode int
				want       string
			}{
				{name: "invalid arguments", args: []string{"tenant_publish"}, output: io.Discard, want: "usage:"},
				{name: "nil output", args: args, want: "usage:"},
				{name: "missing login", args: args, output: io.Discard, want: "saved Hako login is invalid"},
				{name: "missing API URL", args: args, output: io.Discard, login: true, want: "HAKO_API_URL is required"},
				{name: "API rejection", args: args, output: io.Discard, login: true, apiURL: "server", statusCode: http.StatusForbidden, want: "HTTP 403"},
				{name: "output failure", args: args, output: publicationCLIWriterError{}, login: true, apiURL: "server",
					statusCode: http.StatusOK, want: "write Agent publication"},
			} {
				t.Run(test.name, func(t *testing.T) {
					t.Setenv("HAKO_API_URL", test.apiURL)
					if test.login {
						setPublicationCLILogin(t)
					} else {
						previous := cliCredentialGetter
						cliCredentialGetter = workloadCLICredentials("")
						t.Cleanup(func() { cliCredentialGetter = previous })
					}
					if test.apiURL == "server" {
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							wantMethod := http.MethodGet
							if operation.name == "submit" {
								wantMethod = http.MethodPost
							}
							if r.Method != wantMethod || r.Header.Get("Authorization") != "Bearer publication-access" {
								t.Errorf("unexpected API request %s auth=%q", r.Method, r.Header.Get("Authorization"))
							}
							w.Header().Set("Content-Type", "application/json")
							if test.name == "output failure" {
								if operation.name == "show" {
									_, _ = fmt.Fprint(w, `{"tenant_id":"tenant_publish","workload_run_id":"run_publish","state":"branch_created","base_branch":"main","branch_name":"hako/proposal","branch_commit_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:01:00Z"}`)
									return
								}
								w.Header().Set("Cache-Control", "no-store")
								w.WriteHeader(http.StatusAccepted)
								_, _ = fmt.Fprint(w, `{"tenant_id":"tenant_publish","workload_run_id":"run_publish","state":"verification_queued"}`)
								return
							}
							w.WriteHeader(test.statusCode)
							_, _ = fmt.Fprint(w, test.response)
						}))
						t.Setenv("HAKO_API_URL", server.URL)
						defer server.Close()
					}
					err := operation.call(test.args, test.output)
					if err == nil || !strings.Contains(err.Error(), test.want) {
						t.Fatalf("%s error = %v, want %q", operation.name, err, test.want)
					}
					if strings.Contains(err.Error(), "publication-access") || strings.Contains(err.Error(), "secret_value") {
						t.Fatalf("error exposed credential or response data: %v", err)
					}
				})
			}
		})
	}
}
