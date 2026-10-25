package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWorkspaceShellReconnectCancelsRetryAfterRevokingOldSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	postAttempted := make(chan struct{})
	var posts, revokes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			revokes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			posts.Add(1)
			http.Error(w, "synthetic temporary failure", http.StatusServiceUnavailable)
			close(postAttempted)
		default:
			t.Errorf("unexpected API request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	shell := &workspaceShellReconnect{ctx: ctx, apiURL: server.URL, accessToken: "synthetic-access",
		tenantID: "tenant_cli", workspaceID: "ws_cli", apiHTTPClient: server.Client(), apiSession: strings.Repeat("a", 48)}
	go func() {
		<-postAttempted
		cancel()
	}()
	err := shell.reconnect(nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("reconnect error = %v, want context cancellation", err)
	}
	if posts.Load() != 1 || revokes.Load() != 1 {
		t.Fatalf("reconnect requests: session creates=%d revokes=%d, want 1 each", posts.Load(), revokes.Load())
	}
	if shell.apiSession != "" || shell.current != nil {
		t.Fatalf("failed reconnect retained prior session/stream: session=%q current=%p", shell.apiSession, shell.current)
	}
}

func TestWorkspaceShellReconnectIgnoresStaleStreamAndCompletedInput(t *testing.T) {
	current := &workspaceShellStream{}
	failed := &workspaceShellStream{}
	shell := &workspaceShellReconnect{ctx: context.Background(), current: current}
	if err := shell.reconnect(failed); err != nil {
		t.Fatalf("stale stream reconnect = %v", err)
	}
	if shell.current != current {
		t.Fatal("stale stream reconnect replaced the current shell")
	}

	shell.inputDone = true
	if err := shell.reconnect(current); !errors.Is(err, io.EOF) {
		t.Fatalf("reconnect after input completion = %v, want EOF", err)
	}
}
