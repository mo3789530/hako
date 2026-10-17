package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/auth"
)

type workspaceCopyRunner func(context.Context, []string) error

var runWorkspaceSCP workspaceCopyRunner = func(ctx context.Context, args []string) error {
	command := exec.CommandContext(ctx, "scp", args...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}

var checkSFTPOpenSSH = requireSFTPOpenSSH

var openSSHVersionPattern = regexp.MustCompile(`OpenSSH_([0-9]+)\.([0-9]+)`)

// copyWorkspacePath transfers one local path to or from the Workspace through
// OpenSSH scp and the authenticated Hako ProxyCommand. It never disables SSH
// host-key verification and never accepts a caller-selected remote host.
func copyWorkspacePath(ctx context.Context, tenantID, workspaceID, source, destination string, download bool) error {
	if ctx == nil {
		return errors.New("copy context is required")
	}
	if _, err := auth.LoadAccessToken(cliCredentialGetter, time.Now()); err != nil {
		return err
	}
	if os.Getenv("HAKO_API_URL") == "" {
		return errors.New("HAKO_API_URL is required")
	}
	if os.Getenv("HAKO_GATEWAY_URL") == "" {
		return errors.New("HAKO_GATEWAY_URL is required")
	}
	if checkSFTPOpenSSH == nil {
		return errors.New("OpenSSH version checker is unavailable")
	}
	if err := checkSFTPOpenSSH(ctx); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return errors.New("cannot resolve the Hako executable for SSH ProxyCommand")
	}
	args, err := workspaceSCPArgs(executable, tenantID, workspaceID, source, destination, download)
	if err != nil {
		return err
	}
	if runWorkspaceSCP == nil {
		return errors.New("SCP runner is unavailable")
	}
	if err := runWorkspaceSCP(ctx, args); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("SCP transfer failed: %w", err)
	}
	return nil
}

func requireSFTPOpenSSH(ctx context.Context) error {
	if ctx == nil {
		return errors.New("copy context is required")
	}
	output, err := exec.CommandContext(ctx, "ssh", "-V").CombinedOutput()
	if err != nil {
		return errors.New("hako cp requires OpenSSH 9.0 or newer with SFTP-based scp")
	}
	if !supportsSFTPOpenSSH(output) {
		return errors.New("cannot verify OpenSSH version; hako cp requires OpenSSH 9.0 or newer")
	}
	return nil
}

func supportsSFTPOpenSSH(output []byte) bool {
	version := openSSHVersionPattern.FindSubmatch(output)
	if len(version) != 3 {
		return false
	}
	major := 0
	for _, digit := range version[1] {
		major = major*10 + int(digit-'0')
	}
	return major >= 9
}

func workspaceSCPArgs(executable, tenantID, workspaceID, source, destination string, download bool) ([]string, error) {
	if !validCopyID(tenantID) || !validCopyID(workspaceID) || strings.TrimSpace(executable) == "" {
		return nil, errors.New("valid Tenant and Workspace IDs are required")
	}
	remotePath, localPath := destination, source
	if download {
		remotePath, localPath = source, destination
	}
	if !validWorkspacePath(remotePath) || strings.TrimSpace(localPath) == "" || strings.ContainsRune(localPath, '\x00') {
		return nil, errors.New("Workspace path must be an absolute path inside /workspace and local path must be non-empty")
	}
	localPath, err := filepath.Abs(localPath)
	if err != nil {
		return nil, errors.New("local path is invalid")
	}
	remote := "hako@hako-workspace:" + remotePath
	proxyCommand := "ProxyCommand=" + shellQuote(executable) + " ssh-proxy " + tenantID + " " + workspaceID
	hostKeyAlias := "hako-" + tenantID + "-" + workspaceID
	args := []string{"-o", proxyCommand, "-o", "HostKeyAlias=" + hostKeyAlias}
	if download {
		return append(args, remote, localPath), nil
	}
	return append(args, localPath, remote), nil
}

func validWorkspacePath(value string) bool {
	if value == "" || !path.IsAbs(value) || path.Clean(value) != value || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	return value == "/workspace" || strings.HasPrefix(value, "/workspace/")
}

func validCopyID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
