package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/api"
)

func TestParseWorkspaceWaitArgs(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantState string
		wantError bool
	}{
		{name: "defaults", args: []string{"tenant_1", "ws_1"}, wantState: "running"},
		{name: "explicit state and bounds", args: []string{"tenant_1", "ws_1", "--state", "suspended", "--timeout", "5m", "--interval", "1s"}, wantState: "suspended"},
		{name: "deleted state", args: []string{"tenant_1", "ws_1", "--state=deleted"}, wantState: "deleted"},
		{name: "invalid state", args: []string{"tenant_1", "ws_1", "--state", "provisioning"}, wantError: true},
		{name: "unbounded timeout", args: []string{"tenant_1", "ws_1", "--timeout", "31m"}, wantError: true},
		{name: "fast polling", args: []string{"tenant_1", "ws_1", "--interval", "10ms"}, wantError: true},
		{name: "unexpected positional argument", args: []string{"tenant_1", "ws_1", "extra"}, wantError: true},
		{name: "missing identity", args: []string{"tenant_1"}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseWorkspaceWaitArgs(test.args)
			if (err != nil) != test.wantError {
				t.Fatalf("parseWorkspaceWaitArgs(%q) error=%v, wantError=%t", test.args, err, test.wantError)
			}
			if err == nil && (got.target != test.wantState || got.timeout <= 0 || got.interval <= 0) {
				t.Fatalf("parsed options = %+v", got)
			}
		})
	}
}

func TestWorkspaceWaitCLIRoutesAuthenticatedTenantScopedAPIRequest(t *testing.T) {
	previousGetter := cliCredentialGetter
	cliCredentialGetter = workspaceShellCredentials(`{"access_token":"wait-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	t.Cleanup(func() { cliCredentialGetter = previousGetter })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tenants/tenant_wait/workspaces/ws_wait" {
			t.Errorf("Workspace GET = %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer wait-access" {
			t.Errorf("Authorization header = %q", r.Header.Get("Authorization"))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"workspace":{"id":"ws_wait","tenant_id":"tenant_wait"},"status":{"desired_state":"running","observed_state":"running"}}`)
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)

	var output bytes.Buffer
	err := workspaceWaitCLI(context.Background(), []string{"tenant_wait", "ws_wait", "--timeout", "1s"}, &output)
	if err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "Workspace ws_wait: desired=running observed=running") {
		t.Fatalf("wait output = %q", got)
	}
}

func TestWorkspaceWaitCLIFailsClosedForMissingConfiguration(t *testing.T) {
	t.Run("missing token", func(t *testing.T) {
		previousGetter := cliCredentialGetter
		cliCredentialGetter = workspaceShellCredentials("")
		t.Cleanup(func() { cliCredentialGetter = previousGetter })
		t.Setenv("HAKO_API_URL", "https://api.example")
		if err := workspaceWaitCLI(context.Background(), []string{"tenant_wait", "ws_wait"}, io.Discard); err == nil {
			t.Fatal("missing access token must fail")
		}
	})
	t.Run("missing API URL", func(t *testing.T) {
		previousGetter := cliCredentialGetter
		cliCredentialGetter = workspaceShellCredentials(`{"access_token":"wait-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
		t.Cleanup(func() { cliCredentialGetter = previousGetter })
		t.Setenv("HAKO_API_URL", " ")
		if err := workspaceWaitCLI(context.Background(), []string{"tenant_wait", "ws_wait"}, io.Discard); err == nil {
			t.Fatal("missing API URL must fail")
		}
	})
}

func TestWaitForWorkspaceStateReportsProgressAndReturnsOnTarget(t *testing.T) {
	options := workspaceWaitOptions{tenantID: "tenant_1", workspaceID: "ws_1", target: "running", timeout: time.Minute, interval: time.Second}
	states := []string{"pending", "provisioning", "running"}
	reads := 0
	var output bytes.Buffer
	get := func(context.Context, string, string) (api.WorkspaceView, error) {
		state := states[reads]
		reads++
		return api.WorkspaceView{Workspace: api.WorkspaceSummary{ID: "ws_1"}, Status: api.WorkspaceStatus{DesiredState: "running", ObservedState: state}}, nil
	}
	sleepCalls := 0
	sleep := func(context.Context, time.Duration) error { sleepCalls++; return nil }
	if err := waitForWorkspaceState(context.Background(), options, get, &output, sleep); err != nil {
		t.Fatal(err)
	}
	if reads != 3 || sleepCalls != 2 {
		t.Fatalf("GET calls=%d sleep calls=%d", reads, sleepCalls)
	}
	for _, state := range states {
		if !strings.Contains(output.String(), "observed="+state) {
			t.Errorf("progress output %q does not include %s", output.String(), state)
		}
	}
}

func TestWaitForWorkspaceStateFailsOnTerminalFailureOrUnexpectedDelete(t *testing.T) {
	for _, state := range []string{"failed", "deleted"} {
		t.Run(state, func(t *testing.T) {
			options := workspaceWaitOptions{tenantID: "tenant_1", workspaceID: "ws_1", target: "running", timeout: time.Minute, interval: time.Second}
			get := func(context.Context, string, string) (api.WorkspaceView, error) {
				return api.WorkspaceView{Workspace: api.WorkspaceSummary{ID: "ws_1"}, Status: api.WorkspaceStatus{ObservedState: state}}, nil
			}
			var output bytes.Buffer
			err := waitForWorkspaceState(context.Background(), options, get, &output, nil)
			if err == nil || !strings.Contains(err.Error(), state) {
				t.Fatalf("wait error=%v, want failure mentioning %s", err, state)
			}
		})
	}
}

func TestWaitForWorkspaceStateHandlesTimeoutCancellationAndAPIError(t *testing.T) {
	options := workspaceWaitOptions{tenantID: "tenant_1", workspaceID: "ws_1", target: "running", timeout: time.Second, interval: time.Second}
	pending := func(context.Context, string, string) (api.WorkspaceView, error) {
		return api.WorkspaceView{Workspace: api.WorkspaceSummary{ID: "ws_1"}, Status: api.WorkspaceStatus{ObservedState: "pending"}}, nil
	}
	var output bytes.Buffer
	err := waitForWorkspaceState(context.Background(), options, pending, &output, func(context.Context, time.Duration) error { return context.DeadlineExceeded })
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "last observed=pending") {
		t.Fatalf("timeout error=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = waitForWorkspaceState(ctx, options, pending, &output, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error=%v", err)
	}
	apiErr := errors.New("unauthorized")
	err = waitForWorkspaceState(context.Background(), options, func(context.Context, string, string) (api.WorkspaceView, error) { return api.WorkspaceView{}, apiErr }, &output, nil)
	if !errors.Is(err, apiErr) {
		t.Fatalf("API error=%v", err)
	}
}

func TestLastObservedStateUsesUnknownForEmptyState(t *testing.T) {
	if got := lastObservedState(""); got != "unknown" {
		t.Fatalf("lastObservedState(empty) = %q, want unknown", got)
	}
	if got := lastObservedState("provisioning"); got != "provisioning" {
		t.Fatalf("lastObservedState(value) = %q, want provisioning", got)
	}
}
