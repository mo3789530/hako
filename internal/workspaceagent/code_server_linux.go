//go:build linux

package workspaceagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	WorkspaceCodeServerPath = "/usr/local/bin/code-server"
	WorkspaceCodeServerPort = 8081
	codeServerStartupLimit  = 15 * time.Second
)

var ErrCodeServerUnavailable = errors.New("Workspace code-server is unavailable")

// WorkspaceCodeServerProcess owns the image-pinned IDE child process. The
// process runs as the Workspace EFS identity and binds only guest loopback.
type WorkspaceCodeServerProcess struct {
	command *exec.Cmd
	done    chan struct{}
	mu      sync.Mutex
	err     error
}

func StartWorkspaceCodeServer(spec WorkspaceMountSpec) (*WorkspaceCodeServerProcess, error) {
	if !validWorkspaceMountSpec(spec) {
		return nil, ErrCodeServerUnavailable
	}
	return startWorkspaceCodeServer(WorkspaceCodeServerPath, spec.MountPath, spec.UID, spec.GID, waitWorkspaceCodeServerReady)
}

func startWorkspaceCodeServer(binary, root string, uid, gid uint32, waitReady func(*exec.Cmd, <-chan struct{}, time.Duration) error) (*WorkspaceCodeServerProcess, error) {
	binary = strings.TrimSpace(binary)
	info, statErr := os.Stat(binary)
	if !filepath.IsAbs(binary) || statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 ||
		!filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" || uid == 0 || gid == 0 || waitReady == nil {
		return nil, ErrCodeServerUnavailable
	}
	dataDir := filepath.Join(root, ".hako", "code-server", "data")
	extensionsDir := filepath.Join(root, ".hako", "code-server", "extensions")
	args := []string{"--bind-addr", fmt.Sprintf("127.0.0.1:%d", WorkspaceCodeServerPort),
		"--auth", "none", "--disable-telemetry", "--user-data-dir", dataDir, "--extensions-dir", extensionsDir, root}
	command := exec.Command(binary, args...)
	command.Dir = root
	command.Env = []string{"HOME=" + root, "PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "TERM=xterm-256color", "NO_COLOR=1"}
	command.Stdin, command.Stdout, command.Stderr = nil, io.Discard, io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	if os.Geteuid() == 0 {
		command.SysProcAttr.Credential = &syscall.Credential{Uid: uid, Gid: gid, Groups: []uint32{}}
	} else if uid != uint32(os.Geteuid()) || gid != uint32(os.Getegid()) {
		return nil, ErrCodeServerUnavailable
	}
	if err := command.Start(); err != nil {
		return nil, ErrCodeServerUnavailable
	}
	process := &WorkspaceCodeServerProcess{command: command, done: make(chan struct{})}
	go func() {
		err := command.Wait()
		process.mu.Lock()
		process.err = err
		process.mu.Unlock()
		close(process.done)
	}()
	if err := waitReady(command, process.done, codeServerStartupLimit); err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = process.Stop(ctx)
		cancel()
		return nil, ErrCodeServerUnavailable
	}
	return process, nil
}

func waitWorkspaceCodeServerReady(command *exec.Cmd, processDone <-chan struct{}, timeout time.Duration) error {
	if command == nil || command.Process == nil || timeout <= 0 {
		return ErrCodeServerUnavailable
	}
	client := &http.Client{Timeout: 300 * time.Millisecond, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", WorkspaceCodeServerPort))
		}, DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-processDone:
			return ErrCodeServerUnavailable
		case <-deadline.C:
			return ErrCodeServerUnavailable
		case <-ticker.C:
			request, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:8081/healthz", nil)
			response, err := client.Do(request)
			if err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
				_ = response.Body.Close()
				select {
				case <-processDone:
					return ErrCodeServerUnavailable
				default:
				}
				if response.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
	}
}

func (p *WorkspaceCodeServerProcess) Stop(ctx context.Context) error {
	if p == nil || ctx == nil || p.command == nil || p.command.Process == nil {
		return ErrCodeServerUnavailable
	}
	select {
	case <-p.done:
		return nil
	default:
	}
	_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGTERM)
	select {
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.err != nil && !errors.Is(p.err, os.ErrProcessDone) {
			return nil
		}
		return nil
	case <-ctx.Done():
		_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGKILL)
		<-p.done
		return ctx.Err()
	}
}
