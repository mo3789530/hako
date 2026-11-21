package githubapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestVerifyEnvironmentApprovalRequiresExactEnvironmentAndReviewer(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr error
	}{
		{name: "configured user reviewer", status: http.StatusOK, body: `{"name":"production","protection_rules":[{"type":"required_reviewers","prevent_self_review":true,"reviewers":[{"type":"User","reviewer":{"id":12,"login":"release-manager"}}]}]}`},
		{name: "configured team reviewer", status: http.StatusOK, body: `{"name":"production","protection_rules":[{"type":"required_reviewers","prevent_self_review":true,"reviewers":[{"type":"Team","reviewer":{"id":13,"login":"release-team"}}]}]}`},
		{name: "self review allowed", status: http.StatusOK, body: `{"name":"production","protection_rules":[{"type":"required_reviewers","prevent_self_review":false,"reviewers":[{"type":"User","reviewer":{"id":12,"login":"release-manager"}}]}]}`, wantErr: ErrEnvironmentApprovalMissing},
		{name: "no protection", status: http.StatusOK, body: `{"name":"production","protection_rules":[]}`, wantErr: ErrEnvironmentApprovalMissing},
		{name: "empty reviewers", status: http.StatusOK, body: `{"name":"production","protection_rules":[{"type":"required_reviewers","reviewers":[]}]}`, wantErr: ErrEnvironmentApprovalMissing},
		{name: "unknown reviewer", status: http.StatusOK, body: `{"name":"production","protection_rules":[{"type":"required_reviewers","reviewers":[{"type":"Bot","reviewer":{"id":12}}]}]}`, wantErr: ErrEnvironmentApprovalMissing},
		{name: "wrong environment", status: http.StatusOK, body: `{"name":"staging","protection_rules":[{"type":"required_reviewers","reviewers":[{"type":"User","reviewer":{"id":12}}]}]}`, wantErr: ErrEnvironmentProtectionUnavailable},
		{name: "GitHub unavailable", status: http.StatusServiceUnavailable, body: `{}`, wantErr: ErrEnvironmentProtectionUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/api/environments/production" {
					t.Errorf("request = %s %s", r.Method, r.URL.String())
				}
				if r.Header.Get("Authorization") != "Bearer synthetic-token" || r.Header.Get("X-GitHub-Api-Version") == "" {
					t.Error("request omitted scoped token or API version")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			client := &TokenClient{http: server.Client(), apiURL: server.URL, now: func() time.Time { return now }}
			token := InstallationToken{value: "synthetic-token", expiresAt: now.Add(5 * time.Minute)}
			err := client.VerifyEnvironmentApproval(context.Background(), token, RepositoryAccess{Owner: "acme", Name: "api"}, "production")
			if tt.wantErr == nil && err != nil {
				t.Fatalf("VerifyEnvironmentApproval() error = %v", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("VerifyEnvironmentApproval() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyEnvironmentApprovalRejectsInvalidInputsWithoutRequest(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer server.Close()
	now := time.Now().UTC()
	client := &TokenClient{http: server.Client(), apiURL: server.URL, now: func() time.Time { return now }}
	validToken := InstallationToken{value: "synthetic-token", expiresAt: now.Add(time.Minute)}
	for _, input := range []struct {
		token       InstallationToken
		repository  RepositoryAccess
		environment string
	}{
		{repository: RepositoryAccess{Owner: "acme", Name: "api"}, environment: "production"},
		{token: InstallationToken{value: "expired", expiresAt: now.Add(-time.Second)}, repository: RepositoryAccess{Owner: "acme", Name: "api"}, environment: "production"},
		{token: validToken, repository: RepositoryAccess{Owner: "acme", Name: "api"}, environment: "../production"},
		{token: validToken, repository: RepositoryAccess{Owner: "acme", Name: "api"}, environment: "prod/name"},
	} {
		if err := client.VerifyEnvironmentApproval(context.Background(), input.token, input.repository, input.environment); !errors.Is(err, ErrEnvironmentProtectionUnavailable) {
			t.Fatalf("invalid input error = %v", err)
		}
	}
	if called {
		t.Fatal("invalid input reached GitHub")
	}
}

func TestVerifyEnvironmentApprovalForRunRequiresLivePinnedRunAndApprovalRecord(t *testing.T) {
	now := time.Now().UTC()
	for _, tt := range []struct {
		name      string
		pending   string
		approvals string
		runStatus string
		runSHA    string
		wantErr   error
	}{
		{name: "approved live attempt", pending: `[]`, approvals: `[{"state":"approved","environments":[{"name":"production"}],"user":{"id":88}}]`, runStatus: "in_progress", runSHA: strings.Repeat("a", 40)},
		{name: "approval still pending", pending: `[{"environment":{"name":"production"}}]`, approvals: `[]`, runStatus: "in_progress", runSHA: strings.Repeat("a", 40), wantErr: ErrEnvironmentApprovalMissing},
		{name: "no review record", pending: `[]`, approvals: `[]`, runStatus: "in_progress", runSHA: strings.Repeat("a", 40), wantErr: ErrEnvironmentApprovalMissing},
		{name: "wrong commit", pending: `[]`, approvals: `[{"state":"approved","environments":[{"name":"production"}],"user":{"id":88}}]`, runStatus: "in_progress", runSHA: strings.Repeat("b", 40), wantErr: ErrEnvironmentRunMismatch},
		{name: "run no longer active", pending: `[]`, approvals: `[{"state":"approved","environments":[{"name":"production"}],"user":{"id":88}}]`, runStatus: "completed", runSHA: strings.Repeat("a", 40), wantErr: ErrEnvironmentRunMismatch},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer synthetic-token" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/repos/acme/payments/environments/production":
					_, _ = io.WriteString(w, `{"name":"production","protection_rules":[{"type":"required_reviewers","prevent_self_review":true,"reviewers":[{"type":"User","reviewer":{"id":12,"login":"release-manager"}}]}]}`)
				case "/repos/acme/payments/actions/runs/12/attempts/1":
					_, _ = fmt.Fprintf(w, `{"id":12,"run_attempt":1,"status":%q,"event":"workflow_dispatch","head_branch":"main","head_sha":%q,"path":".github/workflows/deploy.yml@main","repository":{"id":81,"owner":{"id":82}}}`, tt.runStatus, tt.runSHA)
				case "/repos/acme/payments/actions/runs/12/pending_deployments":
					_, _ = io.WriteString(w, tt.pending)
				case "/repos/acme/payments/actions/runs/12/approvals":
					_, _ = io.WriteString(w, tt.approvals)
				default:
					t.Errorf("unexpected GitHub endpoint %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			client := &TokenClient{http: server.Client(), apiURL: server.URL, now: func() time.Time { return now }}
			token := InstallationToken{value: "synthetic-token", expiresAt: now.Add(5 * time.Minute)}
			identity := EnvironmentRunIdentity{RepositoryID: 81, RepositoryOwnerID: 82, RunID: "12", RunAttempt: "1", Ref: "refs/heads/main",
				SHA: strings.Repeat("a", 40), WorkflowRef: "acme/payments/.github/workflows/deploy.yml@refs/heads/main", EventName: "workflow_dispatch", Environment: "production"}
			err := client.VerifyEnvironmentApprovalForRun(context.Background(), token, RepositoryAccess{Owner: "acme", Name: "payments"}, identity)
			if tt.wantErr == nil && err != nil {
				t.Fatalf("VerifyEnvironmentApprovalForRun() error = %v", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("VerifyEnvironmentApprovalForRun() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestWorkflowFilePathSupportsGitHubRunAndOIDCClaimFormats(t *testing.T) {
	for input, want := range map[string]string{
		".github/workflows/deploy.yml@main":                          ".github/workflows/deploy.yml",
		"acme/payments/.github/workflows/deploy.yml@refs/heads/main": ".github/workflows/deploy.yml",
		"acme/payments/.github/workflows/deploy.yml":                 ".github/workflows/deploy.yml",
		"deploy.yml@main": "",
	} {
		if got := workflowFilePath(input); got != want {
			t.Errorf("workflowFilePath(%q) = %q, want %q", input, got, want)
		}
	}
}
