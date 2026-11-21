package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkspaceLifecycleActionCLIRequestsAndWaits(t *testing.T) {
	const token = "workspace-action-cli-token"
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tenants/tenant_action/workspaces/ws_action/actions" ||
			r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Idempotency-Key") == "" {
			t.Errorf("unexpected action request: %s %s authorization=%q idempotency=%q", r.Method, r.URL.Path,
				r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"))
		}
		var body struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode action request: %v", err)
		}
		actions = append(actions, body.Action)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"workspace":{"workspace":{"id":"ws_action","tenant_id":"tenant_action","owner_id":"user_action","name":"api-dev"},"status":{"desired_state":"running","observed_state":"provisioning"}},"operation_id":"op_action","operation_type":"`+body.Action+`","operation_state":"pending"}`)
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	setWorkspaceBasicCLICredentials(t, token)

	var stdout, stderr bytes.Buffer
	if code := runWorkspaceLifecycleActionCLI("suspend", []string{"tenant_action", "ws_action", "--idempotency-key", "suspend-1"}, nil, &stdout, &stderr, nil); code != 0 {
		t.Fatalf("suspend CLI code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Workspace ws_action accepted:") || !strings.Contains(stdout.String(), "operation=op_action") {
		t.Fatalf("suspend CLI output = %q", stdout.String())
	}
	waitCalls := 0
	if code := runWorkspaceLifecycleActionCLI("resume", []string{"tenant_action", "ws_action", "--wait", "--idempotency-key", "resume-1"}, nil, &stdout, &stderr,
		func(tenantID, workspaceID string) error {
			waitCalls++
			if tenantID != "tenant_action" || workspaceID != "ws_action" {
				t.Errorf("wait identity = %q/%q", tenantID, workspaceID)
			}
			return nil
		}); code != 0 {
		t.Fatalf("resume CLI code=%d stderr=%q", code, stderr.String())
	}
	if waitCalls != 1 {
		t.Fatalf("readiness wait calls = %d, want 1", waitCalls)
	}
	if got := strings.Join(actions, ","); got != "suspend,resume" {
		t.Fatalf("requested actions before delete = %q", got)
	}
	if code := runWorkspaceLifecycleActionCLI("delete", []string{"tenant_action", "ws_action", "--idempotency-key", "delete-1"},
		strings.NewReader("ws_action\n"), &stdout, &stderr, nil); code != 0 {
		t.Fatalf("confirmed delete CLI code=%d stderr=%q", code, stderr.String())
	}
	if got := strings.Join(actions, ","); got != "suspend,resume,delete" {
		t.Fatalf("requested actions = %q", got)
	}
}

func TestWorkspaceLifecycleActionCLIDeleteCancellationAndInvalidOptionsAvoidAPI(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	setWorkspaceBasicCLICredentials(t, "workspace-action-cli-token")

	var stdout, stderr bytes.Buffer
	if code := runWorkspaceLifecycleActionCLI("delete", []string{"tenant_action", "ws_action", "--idempotency-key", "delete-cancel"},
		strings.NewReader("yes\n"), &stdout, &stderr, nil); code != 0 {
		t.Fatalf("cancelled delete CLI code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Workspace deletion cancelled") {
		t.Fatalf("cancelled delete output = %q", stderr.String())
	}
	if code := runWorkspaceLifecycleActionCLI("suspend", []string{"tenant_action", "ws_action", "--yes"}, nil, &stdout, &stderr, nil); code != 2 {
		t.Fatalf("invalid suspend option exit code = %d", code)
	}
	if code := runWorkspaceLifecycleActionCLI("resume", []string{"tenant_action", "ws_action", "--wait"}, nil, &stdout, &stderr, nil); code != 1 {
		t.Fatalf("resume without readiness waiter exit code = %d", code)
	}
	if requests != 0 {
		t.Fatalf("cancelled/invalid lifecycle calls reached API %d times", requests)
	}
}

func TestWorkspaceLifecycleActionCLISurfacesReadinessFailureAfterAcceptedResume(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"workspace":{"workspace":{"id":"ws_action","tenant_id":"tenant_action","owner_id":"user_action","name":"api-dev"},"status":{"desired_state":"running","observed_state":"provisioning"}},"operation_id":"op_action","operation_type":"resume","operation_state":"pending"}`)
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	setWorkspaceBasicCLICredentials(t, "workspace-action-cli-token")

	var stdout, stderr bytes.Buffer
	if code := runWorkspaceLifecycleActionCLI("resume", []string{"tenant_action", "ws_action", "--wait"}, nil, &stdout, &stderr,
		func(string, string) error { return errors.New("readiness timed out") }); code != 1 {
		t.Fatalf("resume wait failure exit code = %d", code)
	}
	if requests != 1 || !strings.Contains(stdout.String(), "operation=op_action") ||
		!strings.Contains(stderr.String(), "operation op_action was accepted") {
		t.Fatalf("resume wait failure requests=%d stdout=%q stderr=%q", requests, stdout.String(), stderr.String())
	}
}
