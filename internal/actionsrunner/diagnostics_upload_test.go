package actionsrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPRunnerDiagnosticsUploaderUsesScopedBearerWithoutRedirects(t *testing.T) {
	logs := []byte("[runner diagnostic file]\njob finished\n")
	digest := sha256.Sum256(logs)
	endpoint := ""
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("a", 64) ||
			r.Header.Get("Content-Type") != "application/octet-stream" || r.Header.Get("Cache-Control") != "no-store" ||
			r.Header.Get("X-Hako-SHA256") != hex.EncodeToString(digest[:]) {
			t.Errorf("diagnostic request headers/method mismatch: %s %v", r.Method, r.Header)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != string(logs) {
			t.Errorf("diagnostic body=%q err=%v", body, err)
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	endpoint = runnerDiagnosticsUploadURL(server.URL, "integration_runner", 42, 1)
	uploader, err := NewHTTPRunnerDiagnosticsUploader(server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := uploader.Upload(context.Background(), endpoint, strings.Repeat("a", 64), logs, hex.EncodeToString(digest[:])); err != nil {
		t.Fatalf("Upload() failed: %v", err)
	}
	if calls != 1 || strings.Contains(endpoint, strings.Repeat("a", 64)) {
		t.Fatalf("diagnostic request leaked capability into URL or call count=%d", calls)
	}

	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	redirectUploader, err := NewHTTPRunnerDiagnosticsUploader(redirect.Client())
	if err != nil {
		t.Fatal(err)
	}
	redirectURL := runnerDiagnosticsUploadURL(redirect.URL, "integration_runner", 42, 1)
	if err := redirectUploader.Upload(context.Background(), redirectURL, strings.Repeat("a", 64), logs, hex.EncodeToString(digest[:])); err == nil {
		t.Fatal("diagnostics uploader followed or accepted a redirect")
	}
}

func TestRunnerDiagnosticsClaimsBindEndpointToExactJobAttempt(t *testing.T) {
	endpoint := runnerDiagnosticsUploadURL("https://api.hako.example", "integration_runner", 42, 2)
	if endpoint == "" || !validRunnerDiagnosticsClaims(endpoint, strings.Repeat("a", 64), "integration_runner", 42, 2) {
		t.Fatal("valid job-scoped diagnostics capability rejected")
	}
	for _, mutate := range []func(*string, *string){
		func(endpoint, _ *string) {
			*endpoint = "http://api.hako.example" + strings.TrimPrefix(*endpoint, "https://api.hako.example")
		},
		func(endpoint, _ *string) {
			*endpoint = runnerDiagnosticsUploadURL("https://api.hako.example", "integration_other", 42, 2)
		},
		func(_, token *string) { *token = "short" },
	} {
		changedURL, changedToken := endpoint, strings.Repeat("a", 64)
		mutate(&changedURL, &changedToken)
		if validRunnerDiagnosticsClaims(changedURL, changedToken, "integration_runner", 42, 2) {
			t.Fatalf("invalid diagnostics claim accepted: %q", changedURL)
		}
	}
}
