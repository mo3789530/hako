//go:build linux

package workspaceagent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceFilesReadWriteAndAtomicallyReplaceWithinRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	files, err := newWorkspaceFilesAt(root, uint32(os.Getuid()), uint32(os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	if err := files.Write(context.Background(), "/workspace/src/main.go", int64(len("package main\n")), strings.NewReader("package main\n")); err != nil {
		t.Fatalf("write Workspace file: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "src", "main.go"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("uploaded mode=%v error=%v; want private 0600", info, err)
	}
	var downloaded bytes.Buffer
	count, err := files.Read(context.Background(), "/workspace/src/main.go", &downloaded)
	if err != nil || count != int64(len("package main\n")) || downloaded.String() != "package main\n" {
		t.Fatalf("read Workspace file: count=%d body=%q err=%v", count, downloaded.String(), err)
	}
	if err := files.Write(context.Background(), "/workspace/src/main.go", 3, strings.NewReader("new")); err != nil {
		t.Fatalf("replace Workspace file: %v", err)
	}
	downloaded.Reset()
	if _, err := files.Read(context.Background(), "/workspace/src/main.go", &downloaded); err != nil || downloaded.String() != "new" {
		t.Fatalf("replacement content=%q err=%v", downloaded.String(), err)
	}
}

func TestWorkspaceFilesRejectsTraversalSymlinksAndNonRegularFiles(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(root, "dir-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	files, err := newWorkspaceFilesAt(root, uint32(os.Getuid()), uint32(os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	for _, value := range []string{"", "/workspace", "/workspace/../secret", "/workspace/a/../secret", "/workspace//secret", "/workspace/a\\b", "/workspace/dir\nname"} {
		if err := files.Write(context.Background(), value, 1, strings.NewReader("x")); err == nil {
			t.Errorf("accepted unsafe path %q", value)
		}
	}
	for _, value := range []string{"/workspace/escape", "/workspace/dir-link/secret", "/workspace/directory"} {
		var output bytes.Buffer
		if _, err := files.Read(context.Background(), value, &output); !errors.Is(err, ErrWorkspaceFile) {
			t.Errorf("Read(%q) error=%v, want ErrWorkspaceFile", value, err)
		}
	}
	if err := files.Write(context.Background(), "/workspace/escape", 3, strings.NewReader("bad")); !errors.Is(err, ErrWorkspaceFile) {
		t.Fatalf("Write over symlink error=%v, want ErrWorkspaceFile", err)
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "outside" {
		t.Fatalf("outside symlink target changed: %q error=%v", data, err)
	}
}

func TestWorkspaceFilesEnforcesSizeAndCancellation(t *testing.T) {
	root := t.TempDir()
	files, err := newWorkspaceFilesAt(root, uint32(os.Getuid()), uint32(os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	if err := files.Write(context.Background(), "/workspace/too-large", MaxWorkspaceFileBytes+1, strings.NewReader("x")); !errors.Is(err, ErrWorkspaceFile) {
		t.Fatalf("oversized upload error=%v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := files.Write(canceled, "/workspace/canceled", 1, strings.NewReader("x")); !errors.Is(err, ErrWorkspaceFile) {
		t.Fatalf("canceled upload error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "canceled")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled upload left a file: %v", err)
	}
}
