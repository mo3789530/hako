package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

func TestDownloadAgentChangesCLIUsesRunMetadataAndAuthorizedGateway(t *testing.T) {
	content := []byte("synthetic Agent proposal archive")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	artifact := domain.WorkloadArtifactSummary{ID: "artifact_changes", Kind: "agent_changes", SizeBytes: int64(len(content)), SHA256: sha}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/tenants/tenant_cli/workload-runs/run_changes":
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer changes-access" {
				t.Errorf("Run request method/auth = %s/%q", r.Method, r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.WorkloadRun{ID: "run_changes", TenantID: "tenant_cli", Kind: domain.WorkloadAgent,
				State: domain.WorkloadSucceeded, Artifacts: []domain.WorkloadArtifactSummary{artifact}})
		case "/v1/tenants/tenant_cli/workload-runs/run_changes/artifacts/artifact_changes/download-ticket":
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer changes-access" {
				t.Errorf("ticket request method/auth = %s/%q", r.Method, r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"ticket": "short-lived-changes-ticket", "expires_at": time.Now().Add(time.Minute),
				"size_bytes": artifact.SizeBytes, "sha256": artifact.SHA256, "kind": artifact.Kind})
		case "/v1/workload-artifacts/artifact_changes":
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer short-lived-changes-ticket" {
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
	cliCredentialGetter = workloadCLICredentials(fmt.Sprintf(`{"access_token":"changes-access","token_type":"Bearer","expires_in":3600,"stored_at":%q}`,
		time.Now().UTC().Format(time.RFC3339)))
	t.Cleanup(func() { cliCredentialGetter = previous })

	outputPath := filepath.Join(t.TempDir(), "proposal.zip")
	var output bytes.Buffer
	if err := downloadAgentChangesCLIWithOutput([]string{"tenant_cli", "run_changes", "--out", outputPath}, &output); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(outputPath)
	if err != nil || !bytes.Equal(actual, content) {
		t.Fatalf("proposal bytes=%q error=%v", actual, err)
	}
	if !strings.Contains(output.String(), "artifact_changes saved to "+outputPath) || !strings.Contains(output.String(), "untrusted source") {
		t.Fatalf("proposal CLI output = %q", output.String())
	}
}

func TestDownloadAgentChangesCLIRejectsUnsuccessfulOrMismatchedRunsBeforeTicket(t *testing.T) {
	for _, test := range []struct {
		name string
		run  domain.WorkloadRun
	}{
		{name: "wrong tenant", run: domain.WorkloadRun{ID: "run_changes", TenantID: "tenant_other", Kind: domain.WorkloadAgent, State: domain.WorkloadSucceeded}},
		{name: "not successful", run: domain.WorkloadRun{ID: "run_changes", TenantID: "tenant_cli", Kind: domain.WorkloadAgent, State: domain.WorkloadFailed}},
		{name: "no proposal", run: domain.WorkloadRun{ID: "run_changes", TenantID: "tenant_cli", Kind: domain.WorkloadAgent, State: domain.WorkloadSucceeded}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ticketRequested := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/tenants/tenant_cli/workload-runs/run_changes" {
					ticketRequested = true
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(test.run)
			}))
			defer server.Close()
			t.Setenv("HAKO_API_URL", server.URL)
			t.Setenv("HAKO_GATEWAY_URL", server.URL)
			previous := cliCredentialGetter
			cliCredentialGetter = workloadCLICredentials(fmt.Sprintf(`{"access_token":"changes-access","token_type":"Bearer","expires_in":3600,"stored_at":%q}`,
				time.Now().UTC().Format(time.RFC3339)))
			t.Cleanup(func() { cliCredentialGetter = previous })
			err := downloadAgentChangesCLIWithOutput([]string{"tenant_cli", "run_changes", "--out", filepath.Join(t.TempDir(), "proposal.zip")}, io.Discard)
			if err == nil {
				t.Fatal("invalid Agent Run was accepted")
			}
			if ticketRequested {
				t.Fatal("invalid Agent Run requested an artifact ticket")
			}
		})
	}
}

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

func TestDownloadWorkloadArtifactCLIFailsBeforeNetworkOnInvalidConfiguration(t *testing.T) {
	previous := cliCredentialGetter
	t.Cleanup(func() { cliCredentialGetter = previous })
	args := []string{"tenant_cli", "run_cli", "artifact_cli", "--out", filepath.Join(t.TempDir(), "artifact.log")}
	for _, test := range []struct {
		name       string
		output     io.Writer
		args       []string
		credential string
		apiURL     string
		gatewayURL string
		want       string
	}{
		{name: "nil output", args: args, want: "output is unavailable"},
		{name: "invalid arguments", output: io.Discard, args: []string{"tenant_cli"}, want: "usage:"},
		{name: "missing credentials", output: io.Discard, args: args, credential: "", want: "saved Hako login is invalid"},
		{name: "missing API URL", output: io.Discard, args: args, credential: fmt.Sprintf(`{"access_token":"test-token","token_type":"Bearer","expires_in":3600,"stored_at":%q}`, time.Now().UTC().Format(time.RFC3339)), want: "HAKO_API_URL is required"},
		{name: "missing Gateway URL", output: io.Discard, args: args, credential: fmt.Sprintf(`{"access_token":"test-token","token_type":"Bearer","expires_in":3600,"stored_at":%q}`, time.Now().UTC().Format(time.RFC3339)), apiURL: "https://api.example.test", want: "HAKO_GATEWAY_URL is required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HAKO_API_URL", test.apiURL)
			t.Setenv("HAKO_GATEWAY_URL", test.gatewayURL)
			cliCredentialGetter = workloadCLICredentials(test.credential)
			err := downloadWorkloadArtifactCLIWithOutput(test.args, test.output)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("download error = %v, want substring %q", err, test.want)
			}
		})
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

func TestDownloadWorkloadArtifactRejectsGatewayMetadataAndStatusFailures(t *testing.T) {
	content := []byte("artifact body")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	for _, test := range []struct {
		name        string
		status      int
		contentType string
		responseSHA string
		contentLen  int
		want        string
	}{
		{name: "gateway denied", status: http.StatusForbidden, want: "rejected"},
		{name: "wrong media type", status: http.StatusOK, contentType: "text/plain", responseSHA: sha, contentLen: len(content), want: "invalid Workload artifact metadata"},
		{name: "wrong digest header", status: http.StatusOK, contentType: "application/octet-stream", responseSHA: strings.Repeat("0", 64), contentLen: len(content), want: "invalid Workload artifact metadata"},
		{name: "wrong content length", status: http.StatusOK, contentType: "application/octet-stream", responseSHA: sha, contentLen: len(content) + 1, want: "invalid Workload artifact metadata"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/tenants/tenant_cli/workload-runs/run_cli/artifacts/artifact_cli/download-ticket" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]any{"ticket": "ticket", "expires_at": time.Now().Add(time.Minute),
						"size_bytes": len(content), "sha256": sha, "kind": "logs"})
					return
				}
				w.Header().Set("Content-Type", test.contentType)
				w.Header().Set("X-Hako-Artifact-SHA256", test.responseSHA)
				w.Header().Set("Content-Length", fmt.Sprint(test.contentLen))
				w.WriteHeader(test.status)
				if test.status == http.StatusOK {
					_, _ = w.Write(content)
				}
			}))
			defer server.Close()

			outputPath := filepath.Join(t.TempDir(), "artifact.bin")
			if err := os.WriteFile(outputPath, []byte("previous file"), 0600); err != nil {
				t.Fatal(err)
			}
			err := downloadWorkloadArtifact(t.Context(), server.URL, server.URL, "access", "tenant_cli", "run_cli", "artifact_cli", outputPath, server.Client())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("download error = %v, want %q", err, test.want)
			}
			got, readErr := os.ReadFile(outputPath)
			if readErr != nil || string(got) != "previous file" {
				t.Fatalf("failed response replaced destination: %q, %v", got, readErr)
			}
			entries, readErr := os.ReadDir(filepath.Dir(outputPath))
			if readErr != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(outputPath) {
				t.Fatalf("temporary download file leaked: entries=%v err=%v", entries, readErr)
			}
		})
	}
}

func TestDownloadWorkloadArtifactRejectsInvalidConfigurationBeforeRequest(t *testing.T) {
	for _, test := range []struct {
		name       string
		ctx        context.Context
		gatewayURL string
		outputPath string
	}{
		{name: "nil context", gatewayURL: "https://gateway.example.test", outputPath: "artifact.bin"},
		{name: "non-loopback http", ctx: context.Background(), gatewayURL: "http://gateway.example.test", outputPath: "artifact.bin"},
		{name: "blank output", ctx: context.Background(), gatewayURL: "https://gateway.example.test", outputPath: " "},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := downloadWorkloadArtifact(test.ctx, "not a URL", test.gatewayURL, "access", "tenant", "run", "artifact", test.outputPath, nil); err == nil || !strings.Contains(err.Error(), "configuration is invalid") {
				t.Fatalf("invalid configuration error = %v", err)
			}
		})
	}
}

func TestDownloadWorkloadArtifactHandlesTicketAndFilesystemFailures(t *testing.T) {
	content := []byte("artifact bytes")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	for _, test := range []struct {
		name    string
		prepare func(string) (string, error)
		deny    bool
		want    string
	}{
		{name: "ticket denied", prepare: func(path string) (string, error) {
			output := filepath.Join(path, "artifact.bin")
			return output, os.WriteFile(output, []byte("keep"), 0600)
		}, deny: true, want: "HTTP 403"},
		{name: "temporary file create fails", prepare: func(path string) (string, error) {
			return filepath.Join(path, "missing", "artifact.bin"), nil
		}, want: "create temporary artifact file"},
		{name: "install rename fails", prepare: func(path string) (string, error) {
			output := filepath.Join(path, "destination")
			return output, os.Mkdir(output, 0700)
		}, want: "install verified artifact file"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			outputPath, err := test.prepare(root)
			if err != nil {
				t.Fatal(err)
			}
			gatewayReached := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/tenants/tenant_cli/workload-runs/run_cli/artifacts/artifact_cli/download-ticket" {
					if test.deny {
						http.Error(w, "denied", http.StatusForbidden)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]any{"ticket": "ticket", "expires_at": time.Now().Add(time.Minute),
						"size_bytes": len(content), "sha256": sha, "kind": "logs"})
					return
				}
				gatewayReached = true
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("X-Hako-Artifact-SHA256", sha)
				w.Header().Set("Content-Length", fmt.Sprint(len(content)))
				_, _ = w.Write(content)
			}))
			defer server.Close()

			err = downloadWorkloadArtifact(t.Context(), server.URL, server.URL, "access", "tenant_cli", "run_cli", "artifact_cli", outputPath, server.Client())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("download error = %v, want %q", err, test.want)
			}
			if test.deny && gatewayReached {
				t.Fatal("Gateway was contacted after ticket denial")
			}
			if !test.deny && !gatewayReached {
				t.Fatal("Gateway was not contacted for a valid ticket")
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".hako-artifact-") {
					t.Fatalf("temporary artifact file leaked: %s", entry.Name())
				}
			}
			if test.deny {
				got, err := os.ReadFile(outputPath)
				if err != nil || string(got) != "keep" {
					t.Fatalf("ticket denial changed destination: %q err=%v", got, err)
				}
			}
		})
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
