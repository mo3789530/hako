package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type workloadCLICredentials string

func (c workloadCLICredentials) Get(string, string) (string, error) { return string(c), nil }

func TestWorkloadCLIsUseTenantScopedAPIAndImmutableCommit(t *testing.T) {
	const commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const runJSON = `{"id":"run_cli","tenant_id":"tenant_cli","kind":"job","commit_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","ref":"refs/heads/main","state":"pending"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cli-workload-test" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tenants/tenant_cli/workload-runs":
			if r.Header.Get("Idempotency-Key") != "workload-cli-key-1234" {
				t.Errorf("create Idempotency-Key = %q", r.Header.Get("Idempotency-Key"))
			}
			var body struct {
				InstallationID int64  `json:"github_installation_id"`
				RepositoryID   int64  `json:"github_repository_id"`
				CommitSHA      string `json:"commit_sha"`
				Ref            string `json:"ref"`
				Timeout        int64  `json:"timeout_seconds"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.InstallationID != 42 || body.RepositoryID != 7 || body.CommitSHA != commit ||
				body.Ref != "refs/heads/main" || body.Timeout != 900 {
				t.Errorf("Workload create body = %+v", body)
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = fmt.Fprint(w, runJSON)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/tenant_cli/workload-runs":
			if r.URL.Query().Get("limit") != "10" || r.URL.Query().Get("offset") != "3" {
				t.Errorf("Workload list query = %s", r.URL.RawQuery)
			}
			_, _ = fmt.Fprintf(w, `{"items":[%s],"total":1,"limit":10,"offset":3}`, runJSON)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/tenant_cli/workload-runs/run_cli":
			_, _ = fmt.Fprint(w, runJSON)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tenants/tenant_cli/workload-runs/run_cli/cancel":
			w.WriteHeader(http.StatusAccepted)
			_, _ = fmt.Fprint(w, runJSON)
		default:
			t.Errorf("unexpected Workload API request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	previous := cliCredentialGetter
	cliCredentialGetter = workloadCLICredentials(fmt.Sprintf(`{"access_token":"cli-workload-test","token_type":"Bearer","expires_in":3600,"stored_at":%q}`,
		time.Now().UTC().Format(time.RFC3339)))
	t.Cleanup(func() { cliCredentialGetter = previous })

	run, key, err := createWorkloadCLI([]string{"--tenant", "tenant_cli", "--installation", "42", "--repository", "7", "--commit", commit,
		"--ref", "refs/heads/main", "--timeout-seconds", "900", "--idempotency-key", "workload-cli-key-1234"})
	if err != nil || key != "workload-cli-key-1234" || run.ID != "run_cli" || run.TenantID != "tenant_cli" || run.CommitSHA != commit {
		t.Fatalf("Workload create = %+v key=%q error=%v", run, key, err)
	}
	listed, err := listWorkloadRuns("tenant_cli", 10, 3)
	if err != nil || listed.Total != 1 || len(listed.Items) != 1 || listed.Items[0].ID != "run_cli" {
		t.Fatalf("Workload list = %+v error=%v", listed, err)
	}
	got, err := getWorkloadRun("tenant_cli", "run_cli")
	if err != nil || got.ID != "run_cli" {
		t.Fatalf("Workload show = %+v error=%v", got, err)
	}
	got, err = cancelWorkloadRun("tenant_cli", "run_cli")
	if err != nil || got.ID != "run_cli" {
		t.Fatalf("Workload cancel = %+v error=%v", got, err)
	}
}
