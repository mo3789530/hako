//go:build linux

package workspaceagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/workspaceprotocol"
)

func TestExecManagerRunsInWorkspaceWithMinimalEnvironmentAndBoundedOutput(t *testing.T) {
	root := t.TempDir()
	manager, err := NewExecManager(root)
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Run(context.Background(), workspaceprotocol.ExecRequest{
		Argv: []string{"/bin/sh", "-c", "printf '%s:%s' \"$PWD\" \"${HAKO_TEST_SECRET-unset}\"; printf err >&2; exit 7"},
	})
	if err != nil || result.ExitCode != 7 || result.Code != "" || result.TimedOut ||
		string(result.Stdout) != root+":unset" || string(result.Stderr) != "err" {
		t.Fatalf("exec result=%+v err=%v", result, err)
	}

	result, err = manager.Run(context.Background(), workspaceprotocol.ExecRequest{Argv: []string{
		"/usr/bin/head", "-c", fmt.Sprint(workspaceprotocol.MaxExecOutput + 64), "/dev/zero",
	}})
	if err != nil || result.ExitCode != 0 || len(result.Stdout) != workspaceprotocol.MaxExecOutput || !result.StdoutTruncated {
		t.Fatalf("bounded output result len=%d truncated=%v exit=%d err=%v", len(result.Stdout), result.StdoutTruncated, result.ExitCode, err)
	}
}

func TestExecManagerTimesOutAndKillsProcessGroup(t *testing.T) {
	root := t.TempDir()
	manager, err := NewExecManager(root)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "child-survived-timeout")
	started := time.Now()
	result, err := manager.Run(context.Background(), workspaceprotocol.ExecRequest{
		Argv: []string{"/bin/sh", "-c", "(sleep 0.5; touch " + marker + ") & wait"}, TimeoutMillis: 100,
	})
	if err != nil || !result.TimedOut || result.ExitCode != -1 || result.Code != "timed_out" || time.Since(started) > 2*time.Second {
		t.Fatalf("timeout result=%+v elapsed=%s err=%v", result, time.Since(started), err)
	}
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child process survived group cancellation: stat err=%v", err)
	}
}

func TestExecManagerResolvesRelativeProgramUnderWorkspace(t *testing.T) {
	root := t.TempDir()
	program := filepath.Join(root, "run-local")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf local-ok\n"), 0700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewExecManager(root)
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Run(context.Background(), workspaceprotocol.ExecRequest{Argv: []string{"./run-local"}})
	if err != nil || result.ExitCode != 0 || string(result.Stdout) != "local-ok" {
		t.Fatalf("relative program result=%+v err=%v", result, err)
	}
	result, err = manager.Run(context.Background(), workspaceprotocol.ExecRequest{Argv: []string{"../outside"}})
	if err != nil || result.Code != "exec_failed" || result.ExitCode != -1 {
		t.Fatalf("workspace path traversal was accepted: result=%+v err=%v", result, err)
	}
}

func TestExecManagerRejectsInvalidRequestAndPreCanceledContext(t *testing.T) {
	manager, err := NewExecManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Run(context.Background(), workspaceprotocol.ExecRequest{}); err == nil {
		t.Fatal("empty command was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.Run(ctx, workspaceprotocol.ExecRequest{Argv: []string{"/bin/true"}}); err == nil {
		t.Fatal("pre-canceled exec was accepted")
	}
}
