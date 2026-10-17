package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceSCPArgsUploadAndDownloadStayOnWorkspaceHost(t *testing.T) {
	executable := "/opt/Hako's bin/hako"
	upload, err := workspaceSCPArgs(executable, "tenant_1", "ws_1", "./src file.txt", "/workspace/src file.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(upload) != 6 || upload[0] != "-o" || upload[2] != "-o" || upload[3] != "HostKeyAlias=hako-tenant_1-ws_1" {
		t.Fatalf("unexpected upload scp args: %#v", upload)
	}
	if !strings.Contains(upload[1], "ssh-proxy tenant_1 ws_1") || !strings.Contains(upload[1], "'\\''") || upload[4] != filepath.Clean("./src file.txt") && !filepath.IsAbs(upload[4]) || upload[5] != "hako@hako-workspace:/workspace/src file.txt" {
		t.Fatalf("upload args did not preserve safe ProxyCommand/path: %#v", upload)
	}
	for _, arg := range upload {
		if strings.Contains(arg, "StrictHostKeyChecking=no") || strings.Contains(arg, "UserKnownHostsFile=/dev/null") {
			t.Fatalf("scp disabled host-key verification: %#v", upload)
		}
	}

	download, err := workspaceSCPArgs("/usr/bin/hako", "tenant_1", "ws_1", "/workspace/out.txt", "./out.txt", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(download[4], "hako@hako-workspace:") || !filepath.IsAbs(download[5]) {
		t.Fatalf("download args have wrong direction: %#v", download)
	}
}

func TestWorkspaceSCPArgsRejectsTraversalAndUnsafeIdentifiers(t *testing.T) {
	for _, test := range []struct {
		name       string
		tenant     string
		workspace  string
		remotePath string
		localPath  string
		download   bool
	}{
		{name: "parent traversal", tenant: "tenant_1", workspace: "ws_1", remotePath: "/workspace/../etc/passwd", localPath: "local"},
		{name: "outside workspace", tenant: "tenant_1", workspace: "ws_1", remotePath: "/etc/passwd", localPath: "local"},
		{name: "workspace prefix lookalike", tenant: "tenant_1", workspace: "ws_1", remotePath: "/workspace-old/file", localPath: "local"},
		{name: "unsafe tenant", tenant: "tenant_1;id", workspace: "ws_1", remotePath: "/workspace/file", localPath: "local"},
		{name: "unsafe workspace", tenant: "tenant_1", workspace: "ws/1", remotePath: "/workspace/file", localPath: "local"},
		{name: "empty local", tenant: "tenant_1", workspace: "ws_1", remotePath: "/workspace/file", localPath: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := workspaceSCPArgs("/usr/bin/hako", test.tenant, test.workspace, test.localPath, test.remotePath, test.download); err == nil {
				t.Fatal("unsafe copy request was accepted")
			}
		})
	}
}

func TestSFTPRequiresOpenSSH9OrNewer(t *testing.T) {
	for _, test := range []struct {
		version string
		want    bool
	}{
		{version: "OpenSSH_8.9p1, OpenSSL 3.0.0", want: false},
		{version: "OpenSSH_9.0p1, OpenSSL 3.0.0", want: true},
		{version: "OpenSSH_10.2p1, OpenSSL 3.5.9", want: true},
		{version: "dropbear 2024.1", want: false},
	} {
		if got := supportsSFTPOpenSSH([]byte(test.version)); got != test.want {
			t.Errorf("supportsSFTPOpenSSH(%q)=%t, want %t", test.version, got, test.want)
		}
	}
}

func TestCopyWorkspacePathUsesInjectedSCPRunnerAfterLoginAndConfig(t *testing.T) {
	previousGetter, previousRunner, previousVersionCheck := cliCredentialGetter, runWorkspaceSCP, checkSFTPOpenSSH
	cliCredentialGetter = workspaceShellCredentials(`{"access_token":"copy-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	t.Cleanup(func() {
		cliCredentialGetter, runWorkspaceSCP, checkSFTPOpenSSH = previousGetter, previousRunner, previousVersionCheck
	})
	t.Setenv("HAKO_API_URL", "https://api.example.test")
	t.Setenv("HAKO_GATEWAY_URL", "https://gateway.example.test")
	called := false
	runWorkspaceSCP = func(ctx context.Context, args []string) error {
		called = true
		if ctx == nil || len(args) != 6 || args[5] != "hako@hako-workspace:/workspace/file.txt" {
			t.Fatalf("unexpected SCP runner args: %#v", args)
		}
		return nil
	}
	checkSFTPOpenSSH = func(context.Context) error { return nil }
	if err := copyWorkspacePath(context.Background(), "tenant_1", "ws_1", "local.txt", "/workspace/file.txt", false); err != nil {
		t.Fatalf("copyWorkspacePath: %v", err)
	}
	if !called {
		t.Fatal("SCP runner was not called")
	}
}

func TestCopyWorkspacePathPropagatesSCPFailure(t *testing.T) {
	previousGetter, previousRunner, previousVersionCheck := cliCredentialGetter, runWorkspaceSCP, checkSFTPOpenSSH
	cliCredentialGetter = workspaceShellCredentials(`{"access_token":"copy-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	t.Cleanup(func() {
		cliCredentialGetter, runWorkspaceSCP, checkSFTPOpenSSH = previousGetter, previousRunner, previousVersionCheck
	})
	t.Setenv("HAKO_API_URL", "https://api.example.test")
	t.Setenv("HAKO_GATEWAY_URL", "https://gateway.example.test")
	wantErr := errors.New("scp exited")
	runWorkspaceSCP = func(context.Context, []string) error { return wantErr }
	checkSFTPOpenSSH = func(context.Context) error { return nil }
	if err := copyWorkspacePath(context.Background(), "tenant_1", "ws_1", "local.txt", "/workspace/file.txt", false); !errors.Is(err, wantErr) {
		t.Fatalf("copy failure=%v, want wrapped runner error", err)
	}
}
