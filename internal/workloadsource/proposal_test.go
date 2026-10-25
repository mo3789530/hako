package workloadsource

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyAgentProposalArchiveProducesDeterministicVerifierSource(t *testing.T) {
	baseRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(baseRoot, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseRoot, "src", "main.go"), []byte("package main\nconst answer = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseRoot, "go.mod"), []byte("module example\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := CreateArchive(context.Background(), baseRoot, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	baseBytes, err := io.ReadAll(base.Reader())
	if err != nil {
		t.Fatal(err)
	}
	proposal := makeProposalZip(t, map[string]string{"src/main.go": "package main\nconst answer = 42\n", "src/new.go": "package main\n"}, nil)
	first, err := ApplyAgentProposalArchive(context.Background(), bytes.NewReader(baseBytes), int64(len(baseBytes)), base.SHA256(), proposal, 0)
	if err != nil {
		t.Fatalf("ApplyAgentProposalArchive() error = %v", err)
	}
	defer first.Close()
	second, err := ApplyAgentProposalArchive(context.Background(), bytes.NewReader(baseBytes), int64(len(baseBytes)), base.SHA256(), proposal, 0)
	if err != nil {
		t.Fatalf("second ApplyAgentProposalArchive() error = %v", err)
	}
	defer second.Close()
	if first.ProposalSHA256 != digestBytes(proposal) || first.BaseSourceSHA256 != base.SHA256() || first.Archive.SHA256() != second.Archive.SHA256() {
		t.Fatalf("proposal source identity is incomplete or nondeterministic: first=%+v second=%+v", first, second)
	}
	output := filepath.Join(t.TempDir(), "verified-source")
	if err := ExtractArchive(context.Background(), first.Archive.Reader(), first.Archive.Size(), first.Archive.SHA256(), output, 0); err != nil {
		t.Fatalf("extract verifier source: %v", err)
	}
	updated, err := os.ReadFile(filepath.Join(output, "src", "main.go"))
	if err != nil || string(updated) != "package main\nconst answer = 42\n" {
		t.Fatalf("updated source = %q, error=%v", updated, err)
	}
	created, err := os.ReadFile(filepath.Join(output, "src", "new.go"))
	if err != nil || string(created) != "package main\n" {
		t.Fatalf("new source = %q, error=%v", created, err)
	}
}

func TestApplyAgentProposalArchiveRejectsUnsafeProposalsAndBaseIdentity(t *testing.T) {
	baseRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseRoot, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := CreateArchive(context.Background(), baseRoot, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	baseBytes, err := io.ReadAll(base.Reader())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		proposal []byte
		baseHash string
	}{
		{name: "traversal", proposal: makeProposalZip(t, map[string]string{"../outside.go": "package outside\n"}, nil), baseHash: base.SHA256()},
		{name: "absolute", proposal: makeProposalZip(t, map[string]string{"/tmp/outside.go": "package outside\n"}, nil), baseHash: base.SHA256()},
		{name: "protected policy", proposal: makeProposalZip(t, map[string]string{".hako/factory.yaml": "version: 1\n"}, nil), baseHash: base.SHA256()},
		{name: "workflow", proposal: makeProposalZip(t, map[string]string{".github/workflows/ci.yml": "name: ci\n"}, nil), baseHash: base.SHA256()},
		{name: "secret filename", proposal: makeProposalZip(t, map[string]string{".env": "TOKEN=value\n"}, nil), baseHash: base.SHA256()},
		{name: "private key content", proposal: makeProposalZip(t, map[string]string{"key.txt": "-----BEGIN PRIVATE KEY-----\n"}, nil), baseHash: base.SHA256()},
		{name: "symlink", proposal: makeProposalZip(t, map[string]string{"link.go": "main.go"}, map[string]os.FileMode{"link.go": os.ModeSymlink | 0o777}), baseHash: base.SHA256()},
		{name: "wrong base digest", proposal: makeProposalZip(t, map[string]string{"main.go": "package changed\n"}, nil), baseHash: strings.Repeat("0", 64)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ApplyAgentProposalArchive(context.Background(), bytes.NewReader(baseBytes), int64(len(baseBytes)), test.baseHash, test.proposal, 0); err == nil {
				t.Fatal("invalid proposal/base accepted")
			}
		})
	}
	if _, err := ApplyAgentProposalArchive(nil, bytes.NewReader(baseBytes), int64(len(baseBytes)), base.SHA256(), []byte("zip"), 0); err == nil {
		t.Fatal("nil context accepted")
	}
}

func TestApplyAgentProposalArchiveRejectsDuplicateAndOversizedEntries(t *testing.T) {
	baseRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseRoot, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := CreateArchive(context.Background(), baseRoot, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	baseBytes, err := io.ReadAll(base.Reader())
	if err != nil {
		t.Fatal(err)
	}
	duplicate := makeDuplicateProposalZip(t)
	oversized := makeProposalZip(t, map[string]string{"large.go": strings.Repeat("x", maxProposalFileBytes+1)}, nil)
	for name, proposal := range map[string][]byte{"duplicate": duplicate, "oversized_file": oversized, "oversized_archive": bytes.Repeat([]byte("x"), maxProposalArchiveBytes+1)} {
		t.Run(name, func(t *testing.T) {
			if _, err := ApplyAgentProposalArchive(context.Background(), bytes.NewReader(baseBytes), int64(len(baseBytes)), base.SHA256(), proposal, 0); err == nil {
				t.Fatal("invalid proposal accepted")
			}
		})
	}
}

func makeProposalZip(t *testing.T, files map[string]string, modes map[string]os.FileMode) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range files {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		mode := modes[name]
		if mode == 0 {
			mode = 0o644
		}
		header.SetMode(mode)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func makeDuplicateProposalZip(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, content := range []string{"first", "second"} {
		entry, err := writer.Create("same.go")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
