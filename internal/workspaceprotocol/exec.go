package workspaceprotocol

import (
	"errors"
	"strings"
)

const (
	MaxExecArgs       = 64
	MaxExecArgBytes   = 4096
	MaxExecRequest    = 32 << 10
	MaxExecOutput     = 256 << 10
	DefaultExecMillis = 60_000
	MaxExecMillis     = 5 * 60_000
)

var ErrInvalidExec = errors.New("invalid Workspace exec request or result")

// ExecRequest is sent once over a one-use, signed Exec session. It has no
// environment, working-directory, user, or executable-path override.
type ExecRequest struct {
	Argv          []string `json:"argv"`
	TimeoutMillis int      `json:"timeout_ms,omitempty"`
}

// ExecResult keeps stdout and stderr separate and explicitly marks bounded
// truncation. ExitCode is -1 only when the process could not start or timed out.
type ExecResult struct {
	Stdout          []byte `json:"stdout"`
	Stderr          []byte `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
	TimedOut        bool   `json:"timed_out"`
	Code            string `json:"code,omitempty"`
}

func ValidateExecRequest(request ExecRequest) error {
	if len(request.Argv) == 0 || len(request.Argv) > MaxExecArgs {
		return ErrInvalidExec
	}
	total := 0
	for _, arg := range request.Argv {
		if arg == "" || strings.ContainsRune(arg, '\x00') || len(arg) > MaxExecArgBytes {
			return ErrInvalidExec
		}
		total += len(arg)
		if total > MaxExecRequest {
			return ErrInvalidExec
		}
	}
	if request.TimeoutMillis != 0 && (request.TimeoutMillis < 100 || request.TimeoutMillis > MaxExecMillis) {
		return ErrInvalidExec
	}
	return nil
}

func ValidateExecResult(result ExecResult) error {
	validCode := result.Code == "" || result.Code == "exec_failed" || result.Code == "timed_out" ||
		result.Code == "executable_unavailable" || result.Code == "exec_permission_denied" || result.Code == "exec_not_found"
	validCode = validCode || result.Code == "exec_format_invalid" || result.Code == "exec_filesystem_read_only" ||
		result.Code == "exec_resource_unavailable" || result.Code == "exec_binary_unavailable" || result.Code == "exec_runtime_failed"
	if len(result.Stdout) > MaxExecOutput || len(result.Stderr) > MaxExecOutput ||
		(result.ExitCode < -1 || result.ExitCode > 255) ||
		(result.TimedOut && result.ExitCode != -1) ||
		!validCode {
		return ErrInvalidExec
	}
	return nil
}
