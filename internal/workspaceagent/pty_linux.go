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
	maxBufferedTerminal = 1 << 20
)

type ShellConfig struct {
	WorkspaceRoot string
	RunAsUID      uint32
	RunAsGID      uint32
	MaxSessions   int
	SessionTTL    time.Duration
	Now           func() time.Time
}

// ShellManager starts only the image-owned /bin/sh in an image-configured
// workspace root. Callers cannot provide a binary, argv, or environment.
type ShellManager struct {
	mu       sync.Mutex
	root     string
	uid      uint32
	gid      uint32
	max      int
	ttl      time.Duration
	now      func() time.Time
	sessions map[string]*ShellSession
	closed   bool
}

type ShellSession struct {
	mu               sync.Mutex
	id               string
	master           *os.File
	command          *exec.Cmd
	started          time.Time
	expires          time.Time
	done             chan struct{}
	closed           bool
	waitErr          error
	owner            string
	attached         bool
	attachGeneration uint64
	output           []byte
	outputBase       uint64
	outputCursor     uint64
	outputNotify     chan struct{}
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
	if config.RunAsUID == 0 && config.RunAsGID == 0 {
		if os.Geteuid() == 0 {
			return nil, ErrInvalidConfig
		}
		config.RunAsUID = uint32(os.Geteuid())
		config.RunAsGID = uint32(os.Getegid())
	}
	if config.RunAsUID == 0 || config.RunAsGID == 0 ||
		(os.Geteuid() != 0 && (config.RunAsUID != uint32(os.Geteuid()) || config.RunAsGID != uint32(os.Getegid()))) {
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
	return &ShellManager{root: root, uid: config.RunAsUID, gid: config.RunAsGID, max: config.MaxSessions, ttl: config.SessionTTL,
		now: config.Now, sessions: make(map[string]*ShellSession)}, nil
}

func (m *ShellManager) Open(ctx context.Context, size TerminalSize) (*ShellSession, error) {
	return m.open(ctx, size, "", true)
}

// OpenOwned starts an authenticated shell that is independent of its current
// WebSocket connection and can be resumed by the same Hako user.
func (m *ShellManager) OpenOwned(ctx context.Context, size TerminalSize, owner string) (*ShellSession, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, ErrInvalidConfig
	}
	return m.open(ctx, size, owner, false)
}

func (m *ShellManager) open(ctx context.Context, size TerminalSize, owner string, bindContext bool) (*ShellSession, error) {
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
	commandContext := context.Background()
	if bindContext {
		commandContext = ctx
	}
	command := exec.CommandContext(commandContext, "/bin/sh", "-i")
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
	if uint32(os.Geteuid()) != m.uid || uint32(os.Getegid()) != m.gid {
		command.SysProcAttr.Credential = &syscall.Credential{Uid: m.uid, Gid: m.gid, Groups: []uint32{}}
	}
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
		started: m.now(), expires: deadline, done: make(chan struct{}), owner: owner,
		attached: owner != "", outputNotify: make(chan struct{})}
	if owner != "" {
		session.attachGeneration = 1
	}
	m.sessions[session.id] = session
	go session.wait()
	go session.captureOutput()
	go session.expireAt(deadline)
	return session, nil
}

// AttachOwned allows only the authenticated owner to resume a detached shell.
func (m *ShellManager) AttachOwned(id, owner string) (*ShellSession, error) {
	if m == nil || id == "" || strings.TrimSpace(owner) == "" {
		return nil, ErrSessionClosed
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[id]
	if !ok || sessionFinished(session) || !m.now().Before(session.expires) {
		return nil, ErrSessionClosed
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.owner != owner || session.attached || session.closed {
		return nil, ErrSessionClosed
	}
	session.attached = true
	session.attachGeneration++
	return session, nil
}

func (s *ShellSession) Attachment() *ShellAttachment {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	generation := s.attachGeneration
	s.mu.Unlock()
	return &ShellAttachment{session: s, generation: generation}
}

// Detach ends the stream attachment without terminating its shell process.
func (s *ShellSession) Detach() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.attached = false
	s.signalOutputLocked()
	s.mu.Unlock()
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
	return s.read(p, 0, false)
}

func (s *ShellSession) read(p []byte, generation uint64, bound bool) (int, error) {
	if s == nil {
		return 0, ErrSessionClosed
	}
	for {
		s.mu.Lock()
		if s.owner != "" && (!s.attached || bound && generation != s.attachGeneration) {
			s.mu.Unlock()
			return 0, io.EOF
		}
		if s.outputCursor < s.outputBase {
			s.outputCursor = s.outputBase
		}
		available := uint64(len(s.output)) - (s.outputCursor - s.outputBase)
		if available > 0 {
			n := copy(p, s.output[s.outputCursor-s.outputBase:])
			s.outputCursor += uint64(n)
			s.mu.Unlock()
			return n, nil
		}
		if s.closed {
			s.mu.Unlock()
			return 0, io.EOF
		}
		notify := s.outputNotify
		s.mu.Unlock()
		<-notify
	}
}

func (s *ShellSession) Write(p []byte) (int, error) {
	return s.write(p, 0, false)
}

func (s *ShellSession) write(p []byte, generation uint64, bound bool) (int, error) {
	if s == nil {
		return 0, ErrSessionClosed
	}
	if len(p) > maxTerminalFrame {
		return 0, ErrTerminalFrame
	}
	s.mu.Lock()
	closed, master := s.closed, s.master
	attached := s.owner == "" || s.attached && (!bound || generation == s.attachGeneration)
	s.mu.Unlock()
	if closed || !attached {
		return 0, ErrSessionClosed
	}
	return master.Write(p)
}

func (s *ShellSession) Resize(size TerminalSize) error {
	return s.resize(size, 0, false)
}

func (s *ShellSession) resize(size TerminalSize, generation uint64, bound bool) error {
	if s == nil {
		return ErrSessionClosed
	}
	if err := validateTerminalSize(size); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.owner != "" && (!s.attached || bound && generation != s.attachGeneration) {
		return ErrSessionClosed
	}
	return unix.IoctlSetWinsize(int(s.master.Fd()), unix.TIOCSWINSZ,
		&unix.Winsize{Col: size.Columns, Row: size.Rows})
}

type ShellAttachment struct {
	session    *ShellSession
	generation uint64
}

func (a *ShellAttachment) Read(p []byte) (int, error) {
	if a == nil || a.session == nil {
		return 0, ErrSessionClosed
	}
	return a.session.read(p, a.generation, true)
}

func (a *ShellAttachment) Write(p []byte) (int, error) {
	if a == nil || a.session == nil {
		return 0, ErrSessionClosed
	}
	return a.session.write(p, a.generation, true)
}

func (a *ShellAttachment) Resize(size TerminalSize) error {
	if a == nil || a.session == nil {
		return ErrSessionClosed
	}
	return a.session.resize(size, a.generation, true)
}

func (a *ShellAttachment) Close() error {
	if a != nil && a.session != nil {
		a.session.detachGeneration(a.generation)
	}
	return nil
}

func (s *ShellSession) detachGeneration(generation uint64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.owner != "" && s.attachGeneration == generation {
		s.attached = false
		s.signalOutputLocked()
	}
	s.mu.Unlock()
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
	s.signalOutputLocked()
	s.mu.Unlock()
	close(s.done)
}

func (s *ShellSession) captureOutput() {
	buffer := make([]byte, maxTerminalFrame)
	for {
		n, err := s.master.Read(buffer)
		s.mu.Lock()
		if n > 0 {
			s.output = append(s.output, buffer[:n]...)
			if len(s.output) > maxBufferedTerminal {
				drop := len(s.output) - maxBufferedTerminal
				s.output = append([]byte(nil), s.output[drop:]...)
				s.outputBase += uint64(drop)
			}
			s.signalOutputLocked()
		}
		s.mu.Unlock()
		if err != nil || n == 0 {
			return
		}
	}
}

func (s *ShellSession) signalOutputLocked() {
	close(s.outputNotify)
	s.outputNotify = make(chan struct{})
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
