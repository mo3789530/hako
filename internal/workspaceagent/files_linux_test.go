//go:build linux

package workspaceagent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mo3789530/hako/internal/workloadsource"
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

func TestWorkspaceFilesInstallsVerifiedPreviewSourceAtomicallyAndIdempotently(t *testing.T) {
	workspace := t.TempDir()
	repository := t.TempDir()
	if err := os.Mkdir(filepath.Join(repository, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "nested", "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	archive, err := workloadsource.CreateArchive(context.Background(), repository, workloadsource.DefaultMaxArchiveBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	files, err := newWorkspaceFilesAt(workspace, uint32(os.Getuid()), uint32(os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	install := func() error {
		if _, err := archive.Reader().Seek(0, 0); err != nil {
			return err
		}
		return files.InstallSourceArchive(context.Background(), archive.Size(), archive.SHA256(), archive.Reader())
	}
	if err := install(); err != nil {
		t.Fatalf("install verified Preview source: %v", err)
	}
	if err := install(); err != nil {
		t.Fatalf("same-archive retry was not idempotent: %v", err)
	}
	installed, err := os.ReadFile(filepath.Join(workspace, "src", "nested", "main.go"))
	if err != nil || string(installed) != "package main\n" {
		t.Fatalf("installed source=%q error=%v", installed, err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "src", "nested", "main.go"), []byte("modified"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := install(); !errors.Is(err, ErrWorkspaceFile) {
		t.Fatalf("retry replaced or accepted modified source: %v", err)
	}
}

func TestWorkspaceFilesRejectsInvalidPreviewSourceArchiveWithoutInstalling(t *testing.T) {
	workspace := t.TempDir()
	files, err := newWorkspaceFilesAt(workspace, uint32(os.Getuid()), uint32(os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	if err := files.InstallSourceArchive(context.Background(), 4, strings.Repeat("0", 64), strings.NewReader("evil")); !errors.Is(err, ErrWorkspaceFile) {
		t.Fatalf("invalid digest-bound archive error=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(workspace, "src")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid source created destination: %v", err)
	}
}

func TestWorkspaceFilesSerializesConcurrentPreviewSourceInstallers(t *testing.T) {
	workspace := t.TempDir()
	repository := t.TempDir()
	if err := os.WriteFile(filepath.Join(repository, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	archive, err := workloadsource.CreateArchive(context.Background(), repository, workloadsource.DefaultMaxArchiveBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	archiveBytes, err := io.ReadAll(archive.Reader())
	if err != nil {
		t.Fatal(err)
	}
	files, err := newWorkspaceFilesAt(workspace, uint32(os.Getuid()), uint32(os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	var wait sync.WaitGroup
	errors := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errors <- files.InstallSourceArchive(context.Background(), int64(len(archiveBytes)), archive.SHA256(), bytes.NewReader(archiveBytes))
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent same-content source installation failed: %v", err)
		}
	}
	installed, err := os.ReadFile(filepath.Join(workspace, "src", "main.go"))
	if err != nil || string(installed) != "package main\n" {
		t.Fatalf("concurrently installed source=%q error=%v", installed, err)
	}
}
