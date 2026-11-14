//go:build integration

package workloadsource

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPythonProposalAcceptedByGoInspector(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repository")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	cmd := exec.Command("python3", "-c", `import sys; sys.path.insert(0, sys.argv[1]); from hako_coding_agent.proposal import create_proposal; sys.stdout.buffer.write(create_proposal({"src/main.py": b"print(1)\n"}).data)`, filepath.Join(root, "coding-agent/src"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		t.Fatalf("generate Python proposal: %v: %s", err, stderr.String())
	}
	inspection, err := InspectAgentProposalArchive(data)
	if err != nil || len(inspection.Files) != 1 || inspection.Files[0].Path != "src/main.py" {
		t.Fatalf("Python proposal rejected: %+v, %v", inspection, err)
	}
}

func TestPythonLoadsGoSourceArchive(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "example.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".github", "settings"), []byte("hidden"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "binary"), []byte{0, 255}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "script.sh"), []byte("echo hello\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	archive, err := CreateArchive(context.Background(), dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	data, err := io.ReadAll(archive.Reader())
	if err != nil {
		t.Fatal(err)
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repository")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	cmd := exec.Command("python3", "-c", `import sys; sys.path.insert(0, sys.argv[1]); from hako_coding_agent.source import load_source; w = load_source(sys.stdin.buffer.read(), sys.argv[2]); assert w.read_file("example.py") == b"print(1)\n"; assert ".github/settings" not in w.list_files(); assert w.read_file("script.sh") == b"echo hello\n"`, filepath.Join(root, "coding-agent/src"), archive.SHA256())
	cmd.Stdin = bytes.NewReader(data)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Python source loader rejected Go archive: %v: %s", err, output)
	}
}
