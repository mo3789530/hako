package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestValidWorkspaceFileCLIPath(t *testing.T) {
	for _, path := range []string{
		"/workspace/a.txt",
		"/workspace/nested dir/source.go",
		"/workspace/.hidden",
	} {
		if !validWorkspaceFileCLIPath(path) {
			t.Errorf("valid Workspace path rejected: %q", path)
		}
	}
	for _, path := range []string{
		"", "/workspace", "/workspace/", "/etc/passwd", "/workspace-old/file",
		"/workspace/../etc/passwd", "/workspace/a/../../etc/passwd", "/workspace/a\nfile",
	} {
		if validWorkspaceFileCLIPath(path) {
			t.Errorf("unsafe Workspace path accepted: %q", path)
		}
	}
}

func TestPrepareWorkspaceDownloadCreatesPrivateSiblingAndRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "download.txt")
	temp, file, err := prepareWorkspaceDownload(destination, true)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(temp) != dir || temp == destination {
		t.Fatalf("temporary download is not a sibling: %q", temp)
	}
	info, err := os.Stat(temp)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("temporary download permissions = %o, want 600", info.Mode().Perm())
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(temp); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, destination); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareWorkspaceDownload(destination, true); err == nil {
		t.Fatal("symlink destination was accepted")
	}
	contents, err := os.ReadFile(target)
	if err != nil || string(contents) != "preserve" {
		t.Fatalf("symlink target changed: %q, %v", contents, err)
	}
}

func TestPrepareWorkspaceDownloadNoopForUpload(t *testing.T) {
	temp, file, err := prepareWorkspaceDownload("ignored", false)
	if err != nil || temp != "" || file != nil {
		t.Fatalf("upload preparation = %q, %v, %v", temp, file, err)
	}
}

func TestValidCopyID(t *testing.T) {
	for _, id := range []string{"tenant_1", "ws-1", "a"} {
		if !validCopyID(id) {
			t.Errorf("valid ID rejected: %q", id)
		}
	}
	for _, id := range []string{"", "../ws", "tenant;id", "a/b", "space id"} {
		if validCopyID(id) {
			t.Errorf("unsafe ID accepted: %q", id)
		}
	}
}

func TestCopyWorkspacePathRejectsSymlinkUploadBeforeIssuingSession(t *testing.T) {
	previousGetter := cliCredentialGetter
	cliCredentialGetter = workspaceShellCredentials(fmt.Sprintf(`{"access_token":"copy-access","token_type":"Bearer","expires_in":3600,"stored_at":"%s"}`,
		time.Now().UTC().Format(time.RFC3339)))
	t.Cleanup(func() { cliCredentialGetter = previousGetter })
	t.Setenv("HAKO_API_URL", "https://api.example.test")
	t.Setenv("HAKO_GATEWAY_URL", "https://gateway.example.test")
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, source); err != nil {
		t.Fatal(err)
	}
	if err := copyWorkspacePath(context.Background(), "tenant_1", "ws_1", source, "/workspace/file.txt", false); err == nil {
		t.Fatal("symlink upload source was accepted")
	}
}
