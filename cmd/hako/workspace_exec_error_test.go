package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceExecExitErrorMessage(t *testing.T) {
	if got := (workspaceExecExitError{code: 17}).Error(); got != "Workspace command exited with status 17" {
		t.Fatalf("exit error = %q", got)
	}
}

func TestWorkspaceExecRejectsInvalidInputsBeforeCredentialLookup(t *testing.T) {
	previous := cliCredentialGetter
	cliCredentialGetter = workspaceShellCredentials(`{"access_token":"exec-test-token","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	t.Cleanup(func() { cliCredentialGetter = previous })
	t.Setenv("HAKO_API_URL", "")
	t.Setenv("HAKO_GATEWAY_URL", "")
	stdout, stderr := io.Discard, io.Discard
	if err := workspaceExec(nil, "tenant_exec", "ws_exec", []string{"true"}, stdout, stderr); err == nil {
		t.Fatal("nil context was accepted")
	}
	err := workspaceExecWith(context.Background(), "tenant_exec", "ws_exec", []string{"true"}, stdout, stderr, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "HAKO_API_URL and HAKO_GATEWAY_URL are required") {
		t.Fatalf("valid request should continue through auth to endpoint validation, got %v", err)
	}
}

func TestWorkspaceExecWithTimeoutFailsClosedAtConfigurationBoundaries(t *testing.T) {
	previous := cliCredentialGetter
	t.Cleanup(func() { cliCredentialGetter = previous })
	validCredential := `{"access_token":"exec-test-token","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	cliCredentialGetter = workspaceShellCredentials(validCredential)
	ctx := context.Background()
	argv := []string{"go", "test", "./..."}
	for name, call := range map[string]func() error{
		"nil context": func() error {
			return workspaceExecWithTimeout(nil, "tenant_exec", "ws_exec", argv, 0, io.Discard, io.Discard, nil, nil)
		},
		"nil stdout": func() error {
			return workspaceExecWithTimeout(ctx, "tenant_exec", "ws_exec", argv, 0, nil, io.Discard, nil, nil)
		},
		"nil stderr": func() error {
			return workspaceExecWithTimeout(ctx, "tenant_exec", "ws_exec", argv, 0, io.Discard, nil, nil, nil)
		},
		"invalid argv": func() error {
			return workspaceExecWithTimeout(ctx, "tenant_exec", "ws_exec", nil, 0, io.Discard, io.Discard, nil, nil)
		},
		"invalid tenant": func() error {
			return workspaceExecWithTimeout(ctx, "../tenant", "ws_exec", argv, 0, io.Discard, io.Discard, nil, nil)
		},
		"invalid workspace": func() error {
			return workspaceExecWithTimeout(ctx, "tenant_exec", "../workspace", argv, 0, io.Discard, io.Discard, nil, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Fatal("invalid exec request was accepted")
			}
		})
	}

	cliCredentialGetter = workspaceShellCredentials("")
	if err := workspaceExecWithTimeout(ctx, "tenant_exec", "ws_exec", argv, 0, io.Discard, io.Discard, nil, nil); err == nil {
		t.Fatal("missing CLI token was accepted")
	}
	cliCredentialGetter = workspaceShellCredentials(validCredential)
	t.Setenv("HAKO_API_URL", "")
	t.Setenv("HAKO_GATEWAY_URL", "")
	err := workspaceExecWithTimeout(ctx, "tenant_exec", "ws_exec", argv, 0, io.Discard, io.Discard, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "HAKO_API_URL and HAKO_GATEWAY_URL are required") {
		t.Fatalf("missing endpoint error = %v", err)
	}
}

func TestWorkspaceExecSessionFailureDoesNotDialGateway(t *testing.T) {
	previous := cliCredentialGetter
	cliCredentialGetter = workspaceShellCredentials(`{"access_token":"exec-test-token","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	t.Cleanup(func() { cliCredentialGetter = previous })
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "private database detail", http.StatusForbidden)
	}))
	defer apiServer.Close()
	t.Setenv("HAKO_API_URL", apiServer.URL)
	t.Setenv("HAKO_GATEWAY_URL", "https://gateway.example.test")
	err := workspaceExecWithTimeout(context.Background(), "tenant_exec", "ws_exec", []string{"go", "test"}, 0,
		io.Discard, io.Discard, apiServer.Client(), nil)
	if err == nil || strings.Contains(err.Error(), "private database detail") {
		t.Fatalf("exec session failure was not safely bounded: %v", err)
	}
}
