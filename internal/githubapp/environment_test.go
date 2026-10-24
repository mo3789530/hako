package githubapp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
		{name: "configured user reviewer", status: http.StatusOK, body: `{"name":"production","protection_rules":[{"type":"required_reviewers","reviewers":[{"type":"User","reviewer":{"id":12,"login":"release-manager"}}]}]}`},
		{name: "configured team reviewer", status: http.StatusOK, body: `{"name":"production","protection_rules":[{"type":"required_reviewers","reviewers":[{"type":"Team","reviewer":{"id":13,"login":"release-team"}}]}]}`},
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
