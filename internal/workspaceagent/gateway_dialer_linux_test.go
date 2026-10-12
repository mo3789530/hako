//go:build linux

package workspaceagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacesession"
)

func TestGatewayDialerRejectsClaimsOutsideWorkspaceBinding(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	shells, err := NewShellManager(ShellConfig{WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer shells.Close()
	dialer := &GatewayDialer{WorkspaceID: domain.WorkspaceID("ws_bound"), Shells: shells}
	claims := workspacesession.Claims{WorkspaceID: domain.WorkspaceID("ws_foreign")}
	if _, err := dialer.OpenShell(context.Background(), claims); !errors.Is(err, ErrInteractiveChannelUnavailable) {
		t.Fatalf("foreign shell claim error=%v", err)
	}
	if _, err := dialer.OpenPort(context.Background(), claims, 5432); !errors.Is(err, ErrInteractiveChannelUnavailable) {
		t.Fatalf("foreign port claim error=%v", err)
	}
}
