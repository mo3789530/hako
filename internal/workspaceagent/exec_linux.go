//go:build linux

package workspaceagent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mo3789530/hako/internal/workspaceprotocol"
)

const workspaceExecPath = "/usr/local/bin:/usr/bin:/bin"

// ExecManager runs a caller-selected process only after the caller obtained a
// Workspace Session with the explicit exec permission. It pins cwd to the
// Workspace and does not inherit the Agent environment.
type ExecManager struct {
	root string
}

func NewExecManager(workspaceRoot string) (*ExecManager, error) {
	root, err := filepath.Abs(strings.TrimSpace(workspaceRoot))
	if err != nil || root == "/" || strings.TrimSpace(workspaceRoot) == "" {
		return nil, workspaceprotocol.ErrInvalidExec
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, workspaceprotocol.ErrInvalidExec
	}
	return &ExecManager{root: root}, nil
}

func (m *ExecManager) Run(parent context.Context, request workspaceprotocol.ExecRequest) (workspaceprotocol.ExecResult, error) {
	if m == nil || parent == nil || parent.Err() != nil || workspaceprotocol.ValidateExecRequest(request) != nil {
		return workspaceprotocol.ExecResult{}, workspaceprotocol.ErrInvalidExec
	}
	timeout := time.Duration(request.TimeoutMillis) * time.Millisecond
	if timeout == 0 {
		timeout = workspaceprotocol.DefaultExecMillis * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	program, err := resolveWorkspaceExecProgram(m.root, request.Argv[0])
	if err != nil {
		return workspaceprotocol.ExecResult{ExitCode: -1, Code: "exec_failed"}, nil
	}
	command := exec.CommandContext(ctx, program, request.Argv[1:]...)
	command.Dir = m.root
	command.Env = []string{"HOME=" + m.root, "PATH=" + workspaceExecPath, "LANG=C.UTF-8", "TERM=dumb"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = 2 * time.Second
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := unix.Kill(-command.Process.Pid, unix.SIGKILL)
		if errors.Is(err, unix.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	stdout, stderr := &boundedExecBuffer{}, &boundedExecBuffer{}
	command.Stdout, command.Stderr = stdout, stderr
	runErr := command.Run()
	result := workspaceprotocol.ExecResult{
		Stdout: stdout.Bytes(), Stderr: stderr.Bytes(),
		StdoutTruncated: stdout.Truncated(), StderrTruncated: stderr.Truncated(),
		ExitCode: 0,
	}
	if ctx.Err() != nil && parent.Err() == nil {
		result.ExitCode, result.TimedOut, result.Code = -1, true, "timed_out"
		return result, nil
	}
	if runErr == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		if result.ExitCode < 0 {
			result.ExitCode = 255
		}
		return result, nil
	}
	result.ExitCode, result.Code = -1, "exec_failed"
	return result, nil
}

func resolveWorkspaceExecProgram(root, value string) (string, error) {
	if value == "" || strings.ContainsRune(value, '\x00') {
		return "", workspaceprotocol.ErrInvalidExec
	}
	if filepath.IsAbs(value) {
		info, err := os.Stat(value)
		if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
			return "", workspaceprotocol.ErrInvalidExec
		}
		return value, nil
	}
	if strings.ContainsRune(value, filepath.Separator) {
		program := filepath.Clean(filepath.Join(root, value))
		relative, err := filepath.Rel(root, program)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", workspaceprotocol.ErrInvalidExec
		}
		resolved, err := filepath.EvalSymlinks(program)
		if err != nil {
			return "", workspaceprotocol.ErrInvalidExec
		}
		relative, err = filepath.Rel(root, resolved)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", workspaceprotocol.ErrInvalidExec
		}
		program = resolved
		info, err := os.Stat(program)
		if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
			return "", workspaceprotocol.ErrInvalidExec
		}
		return program, nil
	}
	for _, directory := range strings.Split(workspaceExecPath, string(filepath.ListSeparator)) {
		program := filepath.Join(directory, value)
		info, err := os.Stat(program)
		if err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return program, nil
		}
	}
	return "", workspaceprotocol.ErrInvalidExec
}

type boundedExecBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	truncated bool
}

func (b *boundedExecBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := workspaceprotocol.MaxExecOutput - b.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			_, _ = b.buffer.Write(value[:remaining])
			b.truncated = true
		} else {
			_, _ = b.buffer.Write(value)
		}
	} else if len(value) != 0 {
		b.truncated = true
	}
	return len(value), nil
}

func (b *boundedExecBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buffer.Bytes()...)
}

func (b *boundedExecBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}
