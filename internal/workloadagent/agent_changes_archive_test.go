package workloadagent

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func TestInspectAgentChangeArchiveReturnsSortedMetadataOnly(t *testing.T) {
	archive := buildAgentChangeArchive(t, map[string]string{"z.go": "package z\n", "internal/a.go": "package a\n"}, nil)
	summary, err := InspectAgentChangeArchive(archive)
	if err != nil {
		t.Fatalf("InspectAgentChangeArchive() error = %v", err)
	}
	digest := sha256.Sum256(archive)
	if summary.SHA256 != hex.EncodeToString(digest[:]) || len(summary.Files) != 2 ||
		summary.Files[0] != (AgentChangeArchiveFile{Path: "internal/a.go", SizeBytes: int64(len("package a\n"))}) ||
		summary.Files[1] != (AgentChangeArchiveFile{Path: "z.go", SizeBytes: int64(len("package z\n"))}) {
		t.Fatalf("archive summary = %+v", summary)
	}
}

func TestInspectAgentChangeArchiveRejectsUnsafeEntries(t *testing.T) {
	tests := []struct {
		name string
		file string
		body string
		mode os.FileMode
	}{
		{name: "parent traversal", file: "../outside.go", body: "package outside\n"},
		{name: "absolute path", file: "/tmp/outside.go", body: "package outside\n"},
		{name: "backslash path", file: `nested\\outside.go`, body: "package outside\n"},
		{name: "workflow file", file: ".github/workflows/ci.yml", body: "name: ci\n"},
		{name: "Hako policy", file: ".hako/factory.yaml", body: "version: 1\n"},
		{name: "secret filename", file: ".env.production", body: "SAFE=1\n"},
		{name: "private key content", file: "internal/key.go", body: "-----BEGIN PRIVATE KEY-----\n"},
		{name: "symlink", file: "link.go", body: "target.go", mode: os.ModeSymlink | 0777},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := buildAgentChangeArchive(t, map[string]string{test.file: test.body}, map[string]os.FileMode{test.file: test.mode})
			if _, err := InspectAgentChangeArchive(archive); err == nil {
				t.Fatal("unsafe Agent proposal archive accepted")
			}
		})
	}
}

func TestInspectAgentChangeArchiveRejectsMalformedDuplicateAndOversizedFiles(t *testing.T) {
	tests := []struct {
		name    string
		archive []byte
	}{
		{name: "not a zip", archive: []byte("not-a-zip")},
		{name: "empty", archive: buildAgentChangeArchive(t, nil, nil)},
		{name: "oversized file", archive: buildAgentChangeArchive(t, map[string]string{"large.go": strings.Repeat("a", defaultNativeAgentFileBytes+1)}, nil)},
		{name: "oversized archive", archive: bytes.Repeat([]byte("x"), maxNativeAgentChangeArchive+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := InspectAgentChangeArchive(test.archive); err == nil {
				t.Fatal("invalid Agent proposal archive accepted")
			}
		})
	}
}

func TestInspectAgentChangeArchiveRejectsDuplicateEntries(t *testing.T) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, content := range []string{"first", "second"} {
		entry, err := writer.Create("same.go")
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
	if _, err := InspectAgentChangeArchive(buffer.Bytes()); err == nil {
		t.Fatal("duplicate ZIP paths accepted")
	}
}

func buildAgentChangeArchive(t *testing.T, files map[string]string, modes map[string]os.FileMode) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range files {
		mode := modes[name]
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if mode == 0 {
			mode = 0644
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
