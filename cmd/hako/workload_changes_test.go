package main

import (
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

	"github.com/mo3789530/hako/internal/domain"
)

func TestDownloadAgentChangesCLIResolvesRunArtifactAndDownloadsVerifiedBytes(t *testing.T) {
	content := []byte("untrusted proposal zip bytes")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" && r.URL.Path != "/v1/workload-artifacts/artifact_changes" {
			t.Errorf("missing Authorization for %s %s", r.Method, r.URL.Path)
		}
		switch r.URL.Path {
		case "/v1/tenants/tenant_changes/workload-runs/run_changes":
			if r.Method != http.MethodGet {
				t.Errorf("Run lookup method = %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(domain.WorkloadRun{ID: "run_changes", TenantID: "tenant_changes", Kind: domain.WorkloadAgent,
				State: domain.WorkloadSucceeded, Artifacts: []domain.WorkloadArtifactSummary{{ID: "artifact_changes", Kind: "agent_changes",
					SizeBytes: int64(len(content)), SHA256: sha, CreatedAt: time.Now().UTC()}}})
		case "/v1/tenants/tenant_changes/workload-runs/run_changes/artifacts/artifact_changes/download-ticket":
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer changes-access" {
				t.Errorf("ticket request = %s auth=%q", r.Method, r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"ticket": "short-ticket", "expires_at": time.Now().Add(time.Minute),
				"size_bytes": len(content), "sha256": sha, "kind": "agent_changes"})
		case "/v1/workload-artifacts/artifact_changes":
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer short-ticket" {
				t.Errorf("Gateway request = %s auth=%q", r.Method, r.Header.Get("Authorization"))
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

	output := filepath.Join(t.TempDir(), "changes.zip")
	if err := downloadAgentChangesCLI([]string{"tenant_changes", "run_changes", "--out", output}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil || string(got) != string(content) {
		t.Fatalf("proposal bytes=%q err=%v", got, err)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("proposal mode=%v err=%v, want 0600", info.Mode().Perm(), err)
	}
}

func TestAgentChangesArtifactRequiresSingleUnredactedSuccessfulAgentArtifact(t *testing.T) {
	valid := domain.WorkloadRun{Kind: domain.WorkloadAgent, State: domain.WorkloadSucceeded,
		Artifacts: []domain.WorkloadArtifactSummary{{ID: "artifact_changes", Kind: "agent_changes", SizeBytes: 1, SHA256: strings.Repeat("a", 64)}}}
	if artifact, err := agentChangesArtifact(valid); err != nil || artifact.ID != "artifact_changes" {
		t.Fatalf("valid proposal = %+v, %v", artifact, err)
	}
	for name, run := range map[string]domain.WorkloadRun{
		"not agent":      {Kind: domain.WorkloadJob, State: domain.WorkloadSucceeded, Artifacts: valid.Artifacts},
		"not successful": {Kind: domain.WorkloadAgent, State: domain.WorkloadFailed, Artifacts: valid.Artifacts},
		"missing":        {Kind: domain.WorkloadAgent, State: domain.WorkloadSucceeded},
		"redacted":       {Kind: domain.WorkloadAgent, State: domain.WorkloadSucceeded, Artifacts: []domain.WorkloadArtifactSummary{{ID: "artifact_changes", Kind: "agent_changes", SizeBytes: 1, SHA256: strings.Repeat("a", 64), Redacted: true}}},
		"oversized":      {Kind: domain.WorkloadAgent, State: domain.WorkloadSucceeded, Artifacts: []domain.WorkloadArtifactSummary{{ID: "artifact_changes", Kind: "agent_changes", SizeBytes: 8<<20 + 1, SHA256: strings.Repeat("a", 64)}}},
		"ambiguous":      {Kind: domain.WorkloadAgent, State: domain.WorkloadSucceeded, Artifacts: append(append([]domain.WorkloadArtifactSummary(nil), valid.Artifacts...), valid.Artifacts...)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := agentChangesArtifact(run); err == nil {
				t.Fatal("unsafe or unavailable proposal was accepted")
			}
		})
	}
}

func TestDownloadWorkloadArtifactExpectedRejectsTicketMismatchWithoutReplacingFile(t *testing.T) {
	content := []byte("proposal")
	actualDigest := sha256.Sum256(content)
	actualSHA := hex.EncodeToString(actualDigest[:])
	expected := domain.WorkloadArtifactSummary{
		ID: "artifact_changes", Kind: "agent_changes", SizeBytes: int64(len(content)), SHA256: strings.Repeat("a", 64),
	}
	gatewayReached := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/tenants/tenant_changes/workload-runs/run_changes/artifacts/artifact_changes/download-ticket" {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"ticket": "mismatched-ticket", "expires_at": time.Now().Add(time.Minute),
				"size_bytes": len(content), "sha256": actualSHA, "kind": "agent_changes"})
			return
		}
		gatewayReached = true
		t.Errorf("Gateway must not receive request after metadata mismatch: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer server.Close()

	output := filepath.Join(t.TempDir(), "changes.zip")
	if err := os.WriteFile(output, []byte("keep existing file"), 0600); err != nil {
		t.Fatal(err)
	}
	err := downloadWorkloadArtifactExpected(t.Context(), server.URL, server.URL, "access",
		"tenant_changes", "run_changes", "artifact_changes", output, server.Client(), &expected)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("download error = %v, want ticket metadata mismatch", err)
	}
	if gatewayReached {
		t.Fatal("Gateway was contacted with a ticket that did not match Run metadata")
	}
	got, readErr := os.ReadFile(output)
	if readErr != nil || string(got) != "keep existing file" {
		t.Fatalf("existing destination = %q, error = %v", got, readErr)
	}
}
