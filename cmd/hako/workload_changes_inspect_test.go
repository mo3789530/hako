package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectAgentChangesCLIListsPathsWithoutApplyingContent(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "changes.zip")
	writeInspectTestArchive(t, archivePath, "src/main.go", "package src\n// proposal content\n", 0644)
	var output bytes.Buffer
	if err := inspectAgentChangesCLI([]string{archivePath}, &output); err != nil {
		t.Fatalf("inspectAgentChangesCLI() error = %v", err)
	}
	result := output.String()
	if !strings.Contains(result, `"path":"src/main.go"`) || !strings.Contains(result, `"sha256":"`) ||
		strings.Contains(result, "proposal content") {
		t.Fatalf("inspection output exposed unexpected data: %q", result)
	}
}

func TestInspectAgentChangesCLIRejectsSymlinkAndUnsafeArchive(t *testing.T) {
	directory := t.TempDir()
	archivePath := filepath.Join(directory, "unsafe.zip")
	writeInspectTestArchive(t, archivePath, "../outside.go", "package outside\n", 0644)
	var output bytes.Buffer
	if err := inspectAgentChangesCLI([]string{archivePath}, &output); err == nil {
		t.Fatal("unsafe archive accepted")
	}
	symlink := filepath.Join(directory, "link.zip")
	if err := os.Symlink(archivePath, symlink); err != nil {
		t.Fatal(err)
	}
	if err := inspectAgentChangesCLI([]string{symlink}, &output); err == nil {
		t.Fatal("symlink archive accepted")
	}
}

func TestInspectAgentChangesCLIRequiresOneArchiveAndOutput(t *testing.T) {
	if err := inspectAgentChangesCLI(nil, &bytes.Buffer{}); err == nil {
		t.Fatal("missing archive accepted")
	}
	if err := inspectAgentChangesCLI([]string{"archive.zip", "extra"}, &bytes.Buffer{}); err == nil {
		t.Fatal("extra argument accepted")
	}
	if err := inspectAgentChangesCLI([]string{"archive.zip"}, nil); err == nil {
		t.Fatal("missing output writer accepted")
	}
}

func writeInspectTestArchive(t *testing.T, filename, entryName, contents string, mode os.FileMode) {
	t.Helper()
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	header := &zip.FileHeader{Name: entryName, Method: zip.Deflate}
	header.SetMode(mode)
	entry, err := writer.CreateHeader(header)
	if err == nil {
		_, err = entry.Write([]byte(contents))
	}
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
}
