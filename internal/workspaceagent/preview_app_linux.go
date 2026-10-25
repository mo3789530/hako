//go:build linux

package workspaceagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mo3789530/hako/internal/previewprotocol"
	"github.com/mo3789530/hako/internal/workspaceprotocol"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

const (
	PreviewApplicationPort           = previewprotocol.ApplicationPort
	previewManifestPath              = "/workspace/src/.hako/preview.yaml"
	previewManifestMaxBytes          = 16 << 10
	previewApplicationStartupTimeout = 45 * time.Second
	previewApplicationProbeInterval  = 100 * time.Millisecond
)

var ErrPreviewApplication = errors.New("Preview application failed to start")

type previewManifest struct {
	Version int      `yaml:"version"`
	Command []string `yaml:"command"`
}

// PreviewApplicationManager starts the repository-declared application as the
// unprivileged Guest Agent user. It exposes one fixed loopback application port
// and inherits no environment variables or secrets from the Agent.
type PreviewApplicationManager struct {
	root string
	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan error
	hash [32]byte
}

func NewPreviewApplicationManager(workspaceRoot string) (*PreviewApplicationManager, error) {
	root, err := filepath.Abs(strings.TrimSpace(workspaceRoot))
	if err != nil || strings.TrimSpace(workspaceRoot) == "" || root == "/" {
		return nil, ErrPreviewApplication
	}
	return &PreviewApplicationManager{root: root}, nil
}

func (m *PreviewApplicationManager) Start(ctx context.Context, files *WorkspaceFiles) (uint16, error) {
	if m == nil || ctx == nil || ctx.Err() != nil || files == nil {
		return 0, ErrPreviewApplication
	}
	var manifestBytes bytes.Buffer
	count, err := files.Read(ctx, previewManifestPath, &manifestBytes)
	if err != nil || count < 1 || count > previewManifestMaxBytes {
		return 0, ErrPreviewApplication
	}
	manifest, err := decodePreviewManifest(manifestBytes.Bytes())
	if err != nil {
		return 0, ErrPreviewApplication
	}
	digest := sha256.Sum256(manifestBytes.Bytes())
	program, err := resolvePreviewProgram(filepath.Join(m.root, "src"), manifest.Command[0])
	if err != nil {
		return 0, ErrPreviewApplication
	}

	m.mu.Lock()
	if m.cmd != nil {
		select {
		case <-m.done:
			m.cmd, m.done = nil, nil
		case <-ctx.Done():
			m.mu.Unlock()
			return 0, ErrPreviewApplication
		default:
			if m.hash != digest {
				m.mu.Unlock()
				return 0, ErrPreviewApplication
			}
			done := m.done
			m.mu.Unlock()
			if err := waitPreviewApplication(ctx, done); err != nil {
				return 0, err
			}
			return PreviewApplicationPort, nil
		}
	}
	command := exec.Command(program, manifest.Command[1:]...)
	command.Dir = filepath.Join(m.root, "src")
	command.Env = []string{"HOME=" + m.root, "PATH=" + workspaceExecPath, "LANG=C.UTF-8",
		"PORT=3000", "HOST=127.0.0.1", "HOSTNAME=127.0.0.1"}
	command.Stdin, command.Stdout, command.Stderr = nil, io.Discard, io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := command.Start(); err != nil {
		m.mu.Unlock()
		return 0, ErrPreviewApplication
	}
	done := make(chan error, 1)
	m.cmd, m.done, m.hash = command, done, digest
	m.mu.Unlock()
	go func() { done <- command.Wait(); close(done) }()
	if err := waitPreviewApplication(ctx, done); err != nil {
		_ = m.Stop(context.Background())
		return 0, err
	}
	return PreviewApplicationPort, nil
}

func decodePreviewManifest(data []byte) (previewManifest, error) {
	var manifest previewManifest
	if len(data) == 0 || len(data) > previewManifestMaxBytes {
		return manifest, ErrPreviewApplication
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil || decoder.Decode(new(any)) != io.EOF || manifest.Version != 1 ||
		len(manifest.Command) == 0 || len(manifest.Command) > workspaceprotocol.MaxExecArgs {
		return previewManifest{}, ErrPreviewApplication
	}
	total := 0
	for _, argument := range manifest.Command {
		if argument == "" || strings.ContainsRune(argument, '\x00') || len(argument) > workspaceprotocol.MaxExecArgBytes {
			return previewManifest{}, ErrPreviewApplication
		}
		total += len(argument)
		if total > workspaceprotocol.MaxExecRequest {
			return previewManifest{}, ErrPreviewApplication
		}
	}
	return manifest, nil
}

func resolvePreviewProgram(sourceRoot, value string) (string, error) {
	if value == "" || filepath.IsAbs(value) || strings.ContainsRune(value, '\x00') {
		return "", ErrPreviewApplication
	}
	if !strings.ContainsRune(value, filepath.Separator) {
		return resolveWorkspaceExecProgram(sourceRoot, value)
	}
	program := filepath.Clean(filepath.Join(sourceRoot, value))
	relative, err := filepath.Rel(sourceRoot, program)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", ErrPreviewApplication
	}
	resolved, err := filepath.EvalSymlinks(program)
	if err != nil {
		return "", ErrPreviewApplication
	}
	relative, err = filepath.Rel(sourceRoot, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", ErrPreviewApplication
	}
	var info unix.Stat_t
	if err := unix.Stat(resolved, &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFREG || info.Mode&0111 == 0 {
		return "", ErrPreviewApplication
	}
	return resolved, nil
}

func waitPreviewApplication(ctx context.Context, done <-chan error) error {
	deadline := time.NewTimer(previewApplicationStartupTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(previewApplicationProbeInterval)
	defer ticker.Stop()
	dialer := net.Dialer{Timeout: 250 * time.Millisecond}
	for {
		connection, err := dialer.DialContext(ctx, "tcp", "127.0.0.1:3000")
		if err == nil {
			_ = connection.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ErrPreviewApplication
		case <-deadline.C:
			return ErrPreviewApplication
		case <-done:
			return ErrPreviewApplication
		case <-ticker.C:
		}
	}
}

func (m *PreviewApplicationManager) Stop(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		return ErrPreviewApplication
	}
	m.mu.Lock()
	command, done := m.cmd, m.done
	m.cmd, m.done = nil, nil
	m.mu.Unlock()
	if command == nil || command.Process == nil {
		return nil
	}
	if err := unix.Kill(-command.Process.Pid, unix.SIGTERM); err != nil && !errors.Is(err, unix.ESRCH) {
		return ErrPreviewApplication
	}
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		_ = unix.Kill(-command.Process.Pid, unix.SIGKILL)
		return ErrPreviewApplication
	case <-time.After(2 * time.Second):
		_ = unix.Kill(-command.Process.Pid, unix.SIGKILL)
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ErrPreviewApplication
		}
	}
}
