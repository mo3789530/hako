//go:build linux

package workspaceagent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShellManagerRunsFixedInteractiveShellThroughPTY(t *testing.T) {
	manager := newTestShellManager(t, 1, time.Minute)
	session, err := manager.Open(context.Background(), TerminalSize{Columns: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if session.ID() == "" || session.ExpiresAt().IsZero() {
		t.Fatal("session identity or expiry missing")
	}
	if _, ok := manager.Get(session.ID()); !ok {
		t.Fatal("new session was not discoverable")
	}
	if err := session.Resize(TerminalSize{Columns: 120, Rows: 40}); err != nil {
		t.Fatalf("resize: %v", err)
	}
	if _, err := session.Write(make([]byte, maxTerminalFrame+1)); !errors.Is(err, ErrTerminalFrame) {
		t.Fatalf("oversized terminal frame error = %v", err)
	}
	if _, err := io.WriteString(session, "printf 'hako-pty-ready\\n'\nexit\n"); err != nil {
		t.Fatalf("write PTY: %v", err)
	}
	output := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		var got strings.Builder
		for {
			n, readErr := session.Read(buf)
			if n > 0 {
				got.Write(buf[:n])
			}
			if strings.Contains(got.String(), "hako-pty-ready") || readErr != nil {
				output <- got.String()
				return
			}
		}
	}()
	select {
	case got := <-output:
		if !strings.Contains(got, "hako-pty-ready") {
			t.Fatalf("PTY output = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for PTY output")
	}
	if err := session.Wait(); err != nil {
		t.Fatalf("shell exit: %v", err)
	}
	if _, ok := manager.Get(session.ID()); ok {
		t.Fatal("completed session remained available for reconnect")
	}
}

func TestShellManagerEnforcesSessionLimitAndClose(t *testing.T) {
	manager := newTestShellManager(t, 1, time.Minute)
	session, err := manager.Open(context.Background(), TerminalSize{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Open(context.Background(), TerminalSize{Columns: 80, Rows: 24}); !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("second Open error = %v, want session limit", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Wait(); err == nil {
		t.Fatal("closing manager left interactive shell running")
	}
	if _, err := manager.Open(context.Background(), TerminalSize{Columns: 80, Rows: 24}); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("Open after close = %v, want closed", err)
	}
}

func TestShellManagerContextCancellationKillsSession(t *testing.T) {
	manager := newTestShellManager(t, 1, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	session, err := manager.Open(ctx, TerminalSize{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(session, "while :; do sleep 1; done\n"); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-session.done:
	case <-time.After(3 * time.Second):
		t.Fatal("context cancellation did not stop shell process group")
	}
	_ = manager.Close()
}

func TestShellManagerRejectsInvalidConfigAndTerminalSize(t *testing.T) {
	temp := t.TempDir()
	if _, err := NewShellManager(ShellConfig{WorkspaceRoot: filepath.Join(temp, "missing")}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("missing root error = %v", err)
	}
	if _, err := NewShellManager(ShellConfig{WorkspaceRoot: temp, MaxSessions: 65}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("oversized session limit error = %v", err)
	}
	manager := newTestShellManager(t, 1, time.Minute)
	defer manager.Close()
	if _, err := manager.Open(context.Background(), TerminalSize{Columns: 0, Rows: 24}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid terminal size error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.Open(canceled, TerminalSize{Columns: 80, Rows: 24}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("canceled context error = %v", err)
	}
}

func TestShellRunsWithWorkspaceRootAndMinimalEnvironment(t *testing.T) {
	root := t.TempDir()
	manager, err := NewShellManager(ShellConfig{WorkspaceRoot: root, MaxSessions: 1, SessionTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	session, err := manager.Open(context.Background(), TerminalSize{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := io.WriteString(session, "pwd; env; exit\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	var output strings.Builder
	deadline := time.After(3 * time.Second)
	for !strings.Contains(output.String(), "TERM=xterm-256color") {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for shell environment: %q", output.String())
		default:
		}
		n, readErr := session.Read(buf)
		if n > 0 {
			output.Write(buf[:n])
		}
		if readErr != nil && readErr != io.EOF {
			t.Fatal(readErr)
		}
		if readErr != nil {
			break
		}
	}
	if !strings.Contains(output.String(), root) {
		t.Fatalf("shell did not start in workspace root: %q", output.String())
	}
	if strings.Contains(output.String(), "AWS_") || strings.Contains(output.String(), "HOME=/root") {
		t.Fatalf("shell inherited a sensitive or host environment value: %q", output.String())
	}
}

func newTestShellManager(t *testing.T, max int, ttl time.Duration) *ShellManager {
	t.Helper()
	root := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewShellManager(ShellConfig{WorkspaceRoot: root, MaxSessions: max, SessionTTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}
