package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDownloadWorkloadArtifactCLIUsesAuthorizedTicketAndGateway(t *testing.T) {
	content := []byte("synthetic CLI artifact bytes")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/tenants/tenant_cli/workload-runs/run_cli/artifacts/artifact_cli/download-ticket":
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer cli-artifact-token" {
				t.Errorf("ticket request method/auth = %s/%q", r.Method, r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ticket": "short-lived-artifact-ticket", "expires_at": time.Now().Add(time.Minute),
				"size_bytes": len(content), "sha256": sha, "kind": "logs",
			})
		case "/v1/workload-artifacts/artifact_cli":
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer short-lived-artifact-ticket" {
				t.Errorf("Gateway request method/auth = %s/%q", r.Method, r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-Hako-Artifact-SHA256", sha)
			_, _ = w.Write(content)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)
	t.Setenv("HAKO_GATEWAY_URL", server.URL)
	previous := cliCredentialGetter
	cliCredentialGetter = workloadCLICredentials(fmt.Sprintf(`{"access_token":"cli-artifact-token","token_type":"Bearer","expires_in":3600,"stored_at":%q}`,
		time.Now().UTC().Format(time.RFC3339)))
	t.Cleanup(func() { cliCredentialGetter = previous })

	outputPath := filepath.Join(t.TempDir(), "artifact.log")
	var output bytes.Buffer
	if err := downloadWorkloadArtifactCLIWithOutput([]string{"tenant_cli", "run_cli", "artifact_cli", "--out", outputPath}, &output); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(outputPath)
	if err != nil || !bytes.Equal(actual, content) {
		t.Fatalf("artifact bytes=%q error=%v", actual, err)
	}
	info, err := os.Stat(outputPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("artifact mode=%v error=%v, want 0600", info.Mode().Perm(), err)
	}
	if !strings.Contains(output.String(), "artifact_cli saved to "+outputPath) {
		t.Fatalf("CLI success output = %q", output.String())
	}
}

func TestDownloadWorkloadArtifactVerifiesAndAtomicallyWritesPrivateFile(t *testing.T) {
	content := []byte("synthetic private artifact")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/tenants/tenant_cli/workload-runs/run_cli/artifacts/artifact_cli/download-ticket":
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer artifact-token" {
				t.Errorf("ticket request method/auth = %s/%q", r.Method, r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"ticket": "short-lived-artifact-ticket", "expires_at": time.Now().Add(time.Minute),
				"size_bytes": len(content), "sha256": sha, "kind": "logs"})
		case "/v1/workload-artifacts/artifact_cli":
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer short-lived-artifact-ticket" {
				t.Errorf("Gateway request method/auth = %s/%q", r.Method, r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-Hako-Artifact-SHA256", sha)
			_, _ = w.Write(content)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	outputPath := filepath.Join(t.TempDir(), "artifact.log")
	if err := downloadWorkloadArtifact(t.Context(), server.URL, server.URL, "artifact-token", "tenant_cli", "run_cli", "artifact_cli", outputPath, server.Client()); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(outputPath)
	if err != nil || string(actual) != string(content) {
		t.Fatalf("downloaded bytes=%q err=%v", actual, err)
	}
	info, err := os.Stat(outputPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("artifact mode=%v err=%v, want 0600", info.Mode().Perm(), err)
	}
}

func TestDownloadWorkloadArtifactRejectsDigestMismatchWithoutReplacingDestination(t *testing.T) {
	content := []byte("tampered content")
	want := []byte("expected content")
	digest := sha256.Sum256(want)
	sha := hex.EncodeToString(digest[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/tenants/tenant_cli/workload-runs/run_cli/artifacts/artifact_cli/download-ticket" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"ticket": "ticket", "expires_at": time.Now().Add(time.Minute),
				"size_bytes": len(want), "sha256": sha, "kind": "logs"})
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Hako-Artifact-SHA256", sha)
		_, _ = w.Write(content)
	}))
	defer server.Close()

	outputPath := filepath.Join(t.TempDir(), "artifact.bin")
	if err := os.WriteFile(outputPath, []byte("existing file"), 0600); err != nil {
		t.Fatal(err)
	}
	err := downloadWorkloadArtifact(t.Context(), server.URL, server.URL, "artifact-token", "tenant_cli", "run_cli", "artifact_cli", outputPath, server.Client())
	if err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("tampered artifact error = %v", err)
	}
	actual, err := os.ReadFile(outputPath)
	if err != nil || string(actual) != "existing file" {
		t.Fatalf("failed download replaced destination: %q err=%v", actual, err)
	}
}

func TestValidArtifactGatewayURLAllowsOnlyHTTPSOrLoopbackHTTP(t *testing.T) {
	for _, test := range []struct {
		url string
		ok  bool
	}{
		{"https://gateway.example.test", true},
		{"http://127.0.0.1:8443", true},
		{"http://[::1]:8443", true},
		{"http://gateway.example.test", false},
		{"https://user:pass@gateway.example.test", false},
		{"https://gateway.example.test/path", false},
	} {
		t.Run(test.url, func(t *testing.T) {
			if got := validArtifactGatewayURL(test.url); got != test.ok {
				t.Fatalf("validArtifactGatewayURL(%q)=%v, want %v", test.url, got, test.ok)
			}
		})
	}
}
