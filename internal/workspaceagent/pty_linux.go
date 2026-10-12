//go:build linux

// Package workspaceagent contains the interactive, user-facing agent runtime.
// It is deliberately separate from workloadagent: a developer shell must not
// inherit Workload attempt tickets or the fixed CI process contract.
package workspaceagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var (
	ErrInvalidConfig = errors.New("invalid workspace shell configuration")
	ErrSessionLimit  = errors.New("workspace shell session limit reached")
	ErrSessionClosed = errors.New("workspace shell session is closed")
	ErrTerminalFrame = errors.New("workspace terminal frame exceeds the limit")
)

const (
	defaultSessionLimit = 4
	defaultSessionTTL   = 8 * time.Hour
	maxSessionTTL       = 24 * time.Hour
	maxTerminalRows     = 500
	maxTerminalColumns  = 500
	maxTerminalFrame    = 64 << 10
)

type ShellConfig struct {
	WorkspaceRoot string
	MaxSessions   int
	SessionTTL    time.Duration
	Now           func() time.Time
}

// ShellManager starts only the image-owned /bin/sh in an image-configured
// workspace root. Callers cannot provide a binary, argv, or environment.
type ShellManager struct {
	mu       sync.Mutex
	root     string
	max      int
	ttl      time.Duration
	now      func() time.Time
	sessions map[string]*ShellSession
	closed   bool
}

type ShellSession struct {
	mu      sync.Mutex
	id      string
	master  *os.File
	command *exec.Cmd
	started time.Time
	expires time.Time
	done    chan struct{}
	closed  bool
	waitErr error
}

type TerminalSize struct {
	Columns uint16
	Rows    uint16
}

func NewShellManager(config ShellConfig) (*ShellManager, error) {
	root, err := filepath.Abs(strings.TrimSpace(config.WorkspaceRoot))
	if err != nil || strings.TrimSpace(config.WorkspaceRoot) == "" || !filepath.IsAbs(root) {
		return nil, ErrInvalidConfig
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, ErrInvalidConfig
	}
	if config.MaxSessions == 0 {
		config.MaxSessions = defaultSessionLimit
	}
	if config.MaxSessions < 1 || config.MaxSessions > 64 {
		return nil, ErrInvalidConfig
	}
	if config.SessionTTL == 0 {
		config.SessionTTL = defaultSessionTTL
	}
	if config.SessionTTL <= 0 || config.SessionTTL > maxSessionTTL {
		return nil, ErrInvalidConfig
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &ShellManager{root: root, max: config.MaxSessions, ttl: config.SessionTTL,
		now: config.Now, sessions: make(map[string]*ShellSession)}, nil
}

func (m *ShellManager) Open(ctx context.Context, size TerminalSize) (*ShellSession, error) {
	if m == nil || ctx == nil || ctx.Err() != nil {
		return nil, ErrInvalidConfig
	}
	if err := validateTerminalSize(size); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrSessionClosed
	}
	m.removeExpiredLocked()
	if len(m.sessions) >= m.max {
		return nil, ErrSessionLimit
	}
	master, slave, err := openPTY()
	if err != nil {
		return nil, fmt.Errorf("open workspace PTY: %w", err)
	}
	if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: size.Columns, Row: size.Rows}); err != nil {
		master.Close()
		slave.Close()
		return nil, fmt.Errorf("set workspace PTY size: %w", err)
	}
	deadline := m.now().Add(m.ttl)
	command := exec.CommandContext(ctx, "/bin/sh", "-i")
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = 500 * time.Millisecond
	command.Dir = m.root
	command.Env = []string{"HOME=" + m.root, "PATH=/usr/local/bin:/usr/bin:/bin", "TERM=xterm-256color", "LANG=C.UTF-8"}
	command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0, Pdeathsig: syscall.SIGKILL}
	if err := command.Start(); err != nil {
		master.Close()
		slave.Close()
		return nil, fmt.Errorf("start workspace shell: %w", err)
	}
	_ = slave.Close()
	var rawID [24]byte
	if _, err := rand.Read(rawID[:]); err != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = master.Close()
		_ = command.Wait()
		return nil, errors.New("create workspace shell session id")
	}
	session := &ShellSession{id: hex.EncodeToString(rawID[:]), master: master, command: command,
		started: m.now(), expires: deadline, done: make(chan struct{})}
	m.sessions[session.id] = session
	go session.wait()
	go session.expireAt(deadline)
	return session, nil
}

func (m *ShellManager) Get(id string) (*ShellSession, bool) {
	if m == nil || id == "" {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[id]
	if !ok || sessionFinished(session) || !m.now().Before(session.expires) {
		if ok {
			delete(m.sessions, id)
			_ = session.Close()
		}
		return nil, false
	}
	return session, true
}

func (m *ShellManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.closed = true
	sessions := make([]*ShellSession, 0, len(m.sessions))
	for id, session := range m.sessions {
		delete(m.sessions, id)
		sessions = append(sessions, session)
	}
	m.mu.Unlock()
	var first error
	for _, session := range sessions {
		if err := session.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (m *ShellManager) removeExpiredLocked() {
	now := m.now()
	for id, session := range m.sessions {
		if sessionFinished(session) || !now.Before(session.expires) {
			delete(m.sessions, id)
			_ = session.Close()
		}
	}
}

func (s *ShellSession) ID() string {
	if s == nil {
		return ""
	}
	return s.id
}
func (s *ShellSession) StartedAt() time.Time {
	if s == nil {
		return time.Time{}
	}
	return s.started
}
func (s *ShellSession) ExpiresAt() time.Time {
	if s == nil {
		return time.Time{}
	}
	return s.expires
}

func (s *ShellSession) Read(p []byte) (int, error) {
	if s == nil {
		return 0, ErrSessionClosed
	}
	s.mu.Lock()
	closed, master := s.closed, s.master
	s.mu.Unlock()
	if closed {
		return 0, ErrSessionClosed
	}
	return master.Read(p)
}

func (s *ShellSession) Write(p []byte) (int, error) {
	if s == nil {
		return 0, ErrSessionClosed
	}
	if len(p) > maxTerminalFrame {
		return 0, ErrTerminalFrame
	}
	s.mu.Lock()
	closed, master := s.closed, s.master
	s.mu.Unlock()
	if closed {
		return 0, ErrSessionClosed
	}
	return master.Write(p)
}

func (s *ShellSession) Resize(size TerminalSize) error {
	if s == nil {
		return ErrSessionClosed
	}
	if err := validateTerminalSize(size); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSessionClosed
	}
	return unix.IoctlSetWinsize(int(s.master.Fd()), unix.TIOCSWINSZ,
		&unix.Winsize{Col: size.Columns, Row: size.Rows})
}

func (s *ShellSession) Wait() error {
	if s == nil {
		return ErrSessionClosed
	}
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waitErr
}

func (s *ShellSession) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	master, command := s.master, s.command
	s.mu.Unlock()
	_ = master.Close()
	if command.Process != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		select {
		case <-s.done:
		case <-time.After(500 * time.Millisecond):
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			<-s.done
		}
	}
	return nil
}

func (s *ShellSession) wait() {
	err := s.command.Wait()
	s.mu.Lock()
	s.waitErr = err
	s.closed = true
	_ = s.master.Close()
	s.mu.Unlock()
	close(s.done)
}

func (s *ShellSession) expireAt(deadline time.Time) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-s.done:
	case <-timer.C:
		_ = s.Close()
	}
}

func sessionFinished(session *ShellSession) bool {
	select {
	case <-session.done:
		return true
	default:
		return false
	}
}

func validateTerminalSize(size TerminalSize) error {
	if size.Columns < 1 || size.Columns > maxTerminalColumns || size.Rows < 1 || size.Rows > maxTerminalRows {
		return ErrInvalidConfig
	}
	return nil
}

func openPTY() (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, nil, err
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

var _ io.ReadWriteCloser = (*ShellSession)(nil)
