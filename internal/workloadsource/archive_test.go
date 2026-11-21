package workloadsource

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateArchiveIsDeterministicExcludesGitAndRoundTrips(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	for _, root := range []string{first, second} {
		if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("credential = must-not-ship"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Unix(1, 0)
	if err := os.Chtimes(filepath.Join(first, "src", "main.go"), old, old); err != nil {
		t.Fatal(err)
	}
	firstArchive, err := CreateArchive(context.Background(), first, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer firstArchive.Close()
	secondArchive, err := CreateArchive(context.Background(), second, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer secondArchive.Close()
	if firstArchive.SHA256() != secondArchive.SHA256() || firstArchive.Size() != secondArchive.Size() {
		t.Fatalf("archive is not stable across checkout timestamps: first=%s/%d second=%s/%d", firstArchive.SHA256(), firstArchive.Size(), secondArchive.SHA256(), secondArchive.Size())
	}
	if _, err := firstArchive.Reader().Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "checkout")
	if err := ExtractArchive(context.Background(), firstArchive.Reader(), firstArchive.Size(), firstArchive.SHA256(), destination, 0); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(destination, "src", "main.go"))
	if err != nil || string(content) != "package main\n" {
		t.Fatalf("round-trip content = %q err=%v", content, err)
	}
	if info, err := os.Stat(filepath.Join(destination, "run.sh")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("executable bit was not preserved: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(destination, ".git")); !os.IsNotExist(err) {
		t.Fatal(".git metadata was included in source archive")
	}
}

func TestCreateArchiveRejectsSymlinksSpecialFilesAndSizeLimit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateArchive(context.Background(), root, 8); err == nil {
		t.Fatal("archive size limit was ignored")
	}
	linkRoot := t.TempDir()
	if err := os.Symlink("/etc/passwd", filepath.Join(linkRoot, "escape")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	if _, err := CreateArchive(context.Background(), linkRoot, 0); err == nil {
		t.Fatal("source symlink was archived")
	}
	if _, err := CreateArchive(nil, root, 0); err == nil {
		t.Fatal("nil context was accepted")
	}
}

func TestExtractArchiveRejectsDigestTraversalSymlinkAndExistingDestination(t *testing.T) {
	for _, tc := range []struct {
		name   string
		entry  tar.Header
		body   string
		digest string
	}{
		{name: "traversal", entry: tar.Header{Name: "../escape", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}, body: "x"},
		{name: "absolute", entry: tar.Header{Name: "/tmp/escape", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}, body: "x"},
		{name: "symlink", entry: tar.Header{Name: "link", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := makeTar(t, tc.entry, tc.body)
			digest := digestBytes(archive)
			if err := ExtractArchive(context.Background(), bytes.NewReader(archive), int64(len(archive)), digest, filepath.Join(t.TempDir(), "out"), 0); err == nil {
				t.Fatal("unsafe archive was extracted")
			}
		})
	}
	valid := makeTar(t, tar.Header{Name: "main.go", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}, "x")
	if err := ExtractArchive(context.Background(), bytes.NewReader(valid), int64(len(valid)), strings.Repeat("0", 64), filepath.Join(t.TempDir(), "digest-mismatch"), 0); err == nil {
		t.Fatal("wrong digest was accepted")
	}
	root := t.TempDir()
	destination := filepath.Join(root, "existing")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "keep"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ExtractArchive(context.Background(), bytes.NewReader(valid), int64(len(valid)), digestBytes(valid), destination, 0); err == nil {
		t.Fatal("existing destination was accepted")
	}
	if content, err := os.ReadFile(filepath.Join(destination, "keep")); err != nil || string(content) != "keep" {
		t.Fatalf("existing destination was modified: %q err=%v", content, err)
	}
}

func makeTar(t *testing.T, header tar.Header, body string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if err := writer.WriteHeader(&header); err != nil {
		t.Fatal(err)
	}
	if body != "" {
		if _, err := io.WriteString(writer, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
