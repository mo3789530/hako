//go:build linux

package workspaceagent

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecodePreviewManifestIsStrictAndBounded(t *testing.T) {
	valid := []byte("version: 1\ncommand: [sh, -c, 'exec sleep 1']\n")
	if manifest, err := decodePreviewManifest(valid); err != nil || len(manifest.Command) != 3 || manifest.Command[0] != "sh" {
		t.Fatalf("valid Preview manifest=%+v error=%v", manifest, err)
	}
	for _, data := range [][]byte{
		[]byte("version: 2\ncommand: [sh]\n"),
		[]byte("version: 1\ncommand: []\n"),
		[]byte("version: 1\ncommand: [sh]\nunknown: true\n"),
		[]byte("version: 1\ncommand: [sh]\n---\nversion: 1\ncommand: [sh]\n"),
		[]byte(strings.Repeat("x", previewManifestMaxBytes+1)),
	} {
		if _, err := decodePreviewManifest(data); err == nil {
			t.Errorf("accepted invalid Preview manifest %q", data)
		}
	}
}

func TestPreviewApplicationManagerStartsAndStopsIsolatedManifestProcess(t *testing.T) {
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("/usr/bin/python3 is required for the local Preview process test")
	}
	root := t.TempDir()
	source := filepath.Join(root, "src")
	manifestDir := filepath.Join(source, ".hako")
	if err := os.MkdirAll(manifestDir, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := "version: 1\ncommand:\n  - python3\n  - -c\n  - |\n    import socket\n    s = socket.socket()\n    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)\n    s.bind(('127.0.0.1', 3000))\n    s.listen()\n    while True:\n      c, _ = s.accept()\n      c.close()\n"
	if err := os.WriteFile(filepath.Join(manifestDir, "preview.yaml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	files, err := newWorkspaceFilesAt(root, uint32(os.Getuid()), uint32(os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	manager, err := NewPreviewApplicationManager(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	port, err := manager.Start(ctx, files)
	if err != nil || port != PreviewApplicationPort {
		t.Fatalf("Preview Start port=%d error=%v", port, err)
	}
	if secondPort, err := manager.Start(ctx, files); err != nil || secondPort != port {
		t.Fatalf("idempotent Preview Start port=%d error=%v", secondPort, err)
	}
	connection, err := net.DialTimeout("tcp", "127.0.0.1:3000", time.Second)
	if err != nil {
		t.Fatalf("Preview app did not accept loopback connections: %v", err)
	}
	_ = connection.Close()
	if err := manager.Stop(ctx); err != nil {
		t.Fatalf("stop Preview app: %v", err)
	}
	if connection, err := net.DialTimeout("tcp", "127.0.0.1:3000", 100*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatal("Preview app remained alive after Stop")
	}
}

func TestResolvePreviewProgramRejectsEscapeAndAbsolutePath(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "program")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"../program", "escape", outside, ""} {
		if _, err := resolvePreviewProgram(root, value); err == nil {
			t.Errorf("resolved unsafe Preview executable %q", value)
		}
	}
}
