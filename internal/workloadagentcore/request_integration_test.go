//go:build integration

package workloadagentcore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/mo3789530/hako/internal/workloadjournal"
)

func TestPythonAgentCoreRuntimeAcceptsGoInvocationRequest(t *testing.T) {
	archive := []byte("go-authored source archive")
	digest := sha256.Sum256(archive)
	body, err := EncodeInvocationRequest(InvocationRequest{
		Command: workloadjournal.Command{TenantID: "tenant_go", WorkloadRunID: "run_go", AttemptID: "attempt_go",
			Generation: 3, ResourcePlaneID: "rp_go", IdempotencyToken: "token_go"},
		RepositoryID: 19, CommitSHA: "0123456789abcdef0123456789abcdef01234567",
		SourceSHA256: hex.EncodeToString(digest[:]), Task: "verify Go/Python invocation contract", SourceArchive: archive,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repository")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	cmd := exec.Command("python3", "-c", `import sys; sys.path.insert(0, sys.argv[1]); from hako_coding_agent.runtime import parse_invocation; attempt, source, task = parse_invocation(sys.stdin.buffer.read()); assert attempt.tenant_id == "tenant_go" and attempt.run_id == "run_go" and attempt.attempt_id == "attempt_go" and attempt.resource_plane_id == "rp_go" and attempt.generation == 3 and attempt.repository_id == 19 and attempt.commit_sha == "0123456789abcdef0123456789abcdef01234567" and task == "verify Go/Python invocation contract" and source == b"go-authored source archive"`, filepath.Join(root, "coding-agent/src"))
	cmd.Stdin = bytes.NewReader(body)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Python AgentCore Runtime rejected Go request: %v: %s", err, output)
	}
}
