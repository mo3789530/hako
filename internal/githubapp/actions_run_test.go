package githubapp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type actionsRunTestToken struct{}

func (actionsRunTestToken) Value() string        { return "synthetic-actions-read-token" }
func (actionsRunTestToken) ExpiresAt() time.Time { return time.Now().Add(time.Hour) }

func TestActionsRunClientFetchesExactAuthoritativeRunJobAndWorkflow(t *testing.T) {
	var runID, jobID int64 = 303, 404
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-actions-read-token" || r.Header.Get("X-GitHub-Api-Version") == "" {
			t.Errorf("missing scoped token/API version: %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/repos/acme/api/actions/runs/303":
			if r.Method != http.MethodGet {
				t.Errorf("run lookup method = %s", r.Method)
			}
			_, _ = fmt.Fprintf(w, `{"id":%d,"run_attempt":2,"workflow_id":505,"name":"Claude","head_branch":"main","head_sha":"%s","event":"push","repository":{"id":202},"head_repository":{"id":202},"html_url":"ignored-extra-field"}`, runID, strings.Repeat("A", 40))
		case "/repos/acme/api/actions/workflows/505":
			_, _ = fmt.Fprint(w, `{"id":505,"path":".github/workflows/claude.yml","state":"active"}`)
		case "/repos/acme/api/actions/runs/303/attempts/2/jobs":
			if r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("page") != "1" {
				t.Errorf("unexpected job pagination query: %s", r.URL.RawQuery)
			}
			_, _ = fmt.Fprintf(w, `{"total_count":1,"jobs":[{"id":%d,"run_id":303,"run_attempt":2,"name":"Claude review","head_branch":"main","labels":["self-hosted","hako","arm64"],"status":"queued"}]}`, jobID)
		default:
			t.Errorf("unexpected GitHub path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewActionsRunClient(server.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.apiURL = server.URL
	snapshot, err := client.Fetch(context.Background(), actionsRunTestToken{}, RepositoryAccess{Owner: "acme", Name: "api"}, 202, runID, 2, jobID)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if snapshot.RepositoryID != 202 || snapshot.HeadRepository != 202 || snapshot.RunID != runID || snapshot.RunAttempt != 2 ||
		snapshot.WorkflowID != 505 || snapshot.WorkflowPath != ".github/workflows/claude.yml" || snapshot.WorkflowRef != "refs/heads/main" || snapshot.WorkflowName != "Claude" ||
		snapshot.HeadSHA != strings.Repeat("a", 40) || snapshot.HeadBranch != "main" || snapshot.Event != "push" || snapshot.JobID != jobID ||
		snapshot.JobName != "Claude review" || snapshot.JobHeadBranch != "main" || len(snapshot.Labels) != 3 {
		t.Fatalf("Fetch() snapshot = %+v", snapshot)
	}
}

func TestActionsRunClientRejectsIdentityAndAmbiguity(t *testing.T) {
	tests := map[string]http.HandlerFunc{
		"foreign repository": func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/repos/acme/api/actions/runs/303":
				_, _ = fmt.Fprintf(w, `{"id":303,"run_attempt":2,"workflow_id":505,"head_branch":"main","head_sha":"%s","event":"push","repository":{"id":999},"head_repository":{"id":999}}`, strings.Repeat("a", 40))
			default:
				t.Error("client made a follow-up call for a foreign repository")
			}
		},
		"duplicate job": func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/repos/acme/api/actions/runs/303":
				_, _ = fmt.Fprintf(w, `{"id":303,"run_attempt":2,"workflow_id":505,"head_branch":"main","head_sha":"%s","event":"push","repository":{"id":202},"head_repository":{"id":202}}`, strings.Repeat("a", 40))
			case "/repos/acme/api/actions/workflows/505":
				_, _ = fmt.Fprint(w, `{"id":505,"path":".github/workflows/claude.yml"}`)
			case "/repos/acme/api/actions/runs/303/attempts/2/jobs":
				_, _ = fmt.Fprint(w, `{"total_count":2,"jobs":[{"id":404,"run_id":303,"run_attempt":2,"name":"review","head_branch":"main","labels":["hako"]},{"id":404,"run_id":303,"run_attempt":2,"name":"review","head_branch":"main","labels":["hako"]}]}`)
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
			}
		},
	}
	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			defer server.Close()
			client, _ := NewActionsRunClient(server.Client())
			client.apiURL = server.URL
			if _, err := client.Fetch(context.Background(), actionsRunTestToken{}, RepositoryAccess{Owner: "acme", Name: "api"}, 202, 303, 2, 404); err == nil {
				t.Fatal("invalid authoritative metadata accepted")
			}
		})
	}
}
