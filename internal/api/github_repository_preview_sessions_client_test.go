package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacesession"
)

func TestRepositoryPreviewSessionClientCreatesAndRevokesScopedTickets(t *testing.T) {
	var created, revoked bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer preview-access" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tenants/tenant_preview_client/github/installations/12/repositories/34/previews/56/sessions":
			var body struct {
				Permissions []workspacesession.Permission `json:"permissions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Permissions) != 1 || body.Permissions[0] != workspacesession.PermissionVSCode {
				t.Errorf("Preview session body = %+v, error = %v", body, err)
			}
			created = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(RepositoryPreviewSession{SessionID: "0123456789abcdef0123456789abcdef0123456789abcdef",
				WorkspaceID: "prvws_0123456789abcdef0123456789abcdef", Ticket: "short-lived-ticket", ExpiresAt: time.Now().Add(time.Minute),
				Permissions: []workspacesession.Permission{workspacesession.PermissionVSCode}})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/tenants/tenant_preview_client/github/installations/12/repositories/34/previews/56/sessions/0123456789abcdef0123456789abcdef0123456789abcdef":
			revoked = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Preview session request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	session, err := CreateRepositoryPreviewSession(context.Background(), server.URL, "preview-access", "tenant_preview_client", 12, 34, 56,
		[]workspacesession.Permission{workspacesession.PermissionVSCode}, server.Client())
	if err != nil || !created || session.SessionID == "" || session.WorkspaceID != domain.WorkspaceID("prvws_0123456789abcdef0123456789abcdef") || session.Ticket == "" {
		t.Fatalf("CreateRepositoryPreviewSession = %+v, created=%v, error=%v", session, created, err)
	}
	if err := RevokeRepositoryPreviewSession(context.Background(), server.URL, "preview-access", "tenant_preview_client", 12, 34, 56, string(session.SessionID), server.Client()); err != nil || !revoked {
		t.Fatalf("RevokeRepositoryPreviewSession revoked=%v, error=%v", revoked, err)
	}
}

func TestRepositoryPreviewSessionClientRejectsInvalidIdentityAndResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"session_id":"bad","workspace_id":"ws","ticket":"x","expires_at":"2099-10-25T00:00:00Z","permissions":["vscode"]}`))
	}))
	defer server.Close()
	if _, err := CreateRepositoryPreviewSession(context.Background(), server.URL, "token", "tenant_preview_client", 1, 2, 3,
		[]workspacesession.Permission{"admin"}, server.Client()); err == nil {
		t.Fatal("CreateRepositoryPreviewSession accepted an invalid permission")
	}
	if _, err := CreateRepositoryPreviewSession(context.Background(), server.URL, "token", "tenant_preview_client", 1, 2, 3,
		[]workspacesession.Permission{workspacesession.PermissionShell}, server.Client()); err == nil {
		t.Fatal("CreateRepositoryPreviewSession accepted a general Workspace shell permission")
	}
	if _, err := CreateRepositoryPreviewSession(context.Background(), server.URL, "token", "tenant_preview_client", 1, 2, 3, nil, server.Client()); err == nil || !strings.Contains(err.Error(), "invalid Preview session response") {
		t.Fatalf("CreateRepositoryPreviewSession invalid response error = %v", err)
	}
	if err := RevokeRepositoryPreviewSession(context.Background(), server.URL, "token", "tenant_preview_client", 1, 2, 3, "bad", server.Client()); err == nil {
		t.Fatal("RevokeRepositoryPreviewSession accepted an invalid Session ID")
	}
}
