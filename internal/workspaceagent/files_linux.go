//go:build linux

package workspaceagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

const MaxWorkspaceFileBytes int64 = 1 << 30

var ErrWorkspaceFile = errors.New("invalid or unavailable Workspace file")

// WorkspaceFiles confines upload/download operations to a pinned Workspace
// directory. Every path component is opened with O_NOFOLLOW relative to an
// already-open directory descriptor, so symlink replacement cannot escape
// the EFS Access Point.
type WorkspaceFiles struct {
	mu     sync.RWMutex
	rootFD int
	max    int64
}

func NewWorkspaceFiles(root string, uid, gid uint32) (*WorkspaceFiles, error) {
	if root != "/workspace" {
		return nil, ErrWorkspaceFile
	}
	return newWorkspaceFilesAt(root, uid, gid)
}

func newWorkspaceFilesAt(root string, uid, gid uint32) (*WorkspaceFiles, error) {
	if !path.IsAbs(root) || path.Clean(root) != root || uid == 0 || gid == 0 {
		return nil, ErrWorkspaceFile
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrWorkspaceFile
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uid || stat.Gid != gid {
		return nil, ErrWorkspaceFile
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrWorkspaceFile
	}
	return &WorkspaceFiles{rootFD: fd, max: MaxWorkspaceFileBytes}, nil
}

func (f *WorkspaceFiles) Close() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rootFD < 0 {
		return nil
	}
	err := unix.Close(f.rootFD)
	f.rootFD = -1
	return err
}

func (f *WorkspaceFiles) Read(ctx context.Context, workspacePath string, dst io.Writer) (int64, error) {
	if f == nil || ctx == nil || dst == nil {
		return 0, ErrWorkspaceFile
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.rootFD < 0 {
		return 0, ErrWorkspaceFile
	}
	parent, name, err := f.openParent(workspacePath)
	if err != nil {
		return 0, ErrWorkspaceFile
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return 0, ErrWorkspaceFile
	}
	file := os.NewFile(uintptr(fd), "workspace-file")
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Size < 0 || stat.Size > f.max {
		return 0, ErrWorkspaceFile
	}
	limited := &contextReader{ctx: ctx, reader: io.LimitReader(file, f.max+1)}
	written, err := io.Copy(dst, limited)
	if err != nil || written != stat.Size {
		return 0, ErrWorkspaceFile
	}
	return written, nil
}

func (f *WorkspaceFiles) Size(workspacePath string) (int64, error) {
	if f == nil {
		return 0, ErrWorkspaceFile
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.rootFD < 0 {
		return 0, ErrWorkspaceFile
	}
	parent, name, err := f.openParent(workspacePath)
	if err != nil {
		return 0, ErrWorkspaceFile
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return 0, ErrWorkspaceFile
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Size < 0 || stat.Size > f.max {
		return 0, ErrWorkspaceFile
	}
	return stat.Size, nil
}

func (f *WorkspaceFiles) Write(ctx context.Context, workspacePath string, size int64, src io.Reader) error {
	if f == nil || ctx == nil || src == nil || size < 0 || size > MaxWorkspaceFileBytes {
		return ErrWorkspaceFile
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.rootFD < 0 {
		return ErrWorkspaceFile
	}
	parent, name, err := f.openParent(workspacePath)
	if err != nil {
		return ErrWorkspaceFile
	}
	defer unix.Close(parent)
	var existing unix.Stat_t
	if err := unix.Fstatat(parent, name, &existing, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		if existing.Mode&unix.S_IFMT != unix.S_IFREG {
			return ErrWorkspaceFile
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return ErrWorkspaceFile
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return ErrWorkspaceFile
	}
	temp := ".hako-upload-" + hex.EncodeToString(random[:])
	fd, err := unix.Openat(parent, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return ErrWorkspaceFile
	}
	file := os.NewFile(uintptr(fd), "workspace-upload")
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = unix.Unlinkat(parent, temp, 0)
		}
	}()
	if err := file.Chmod(0600); err != nil {
		return ErrWorkspaceFile
	}
	reader := &contextReader{ctx: ctx, reader: io.LimitReader(src, size)}
	written, err := io.CopyN(file, reader, size)
	if err != nil || written != size {
		return ErrWorkspaceFile
	}
	if err := file.Sync(); err != nil {
		return ErrWorkspaceFile
	}
	if err := file.Close(); err != nil {
		return ErrWorkspaceFile
	}
	if err := unix.Renameat(parent, temp, parent, name); err != nil {
		return ErrWorkspaceFile
	}
	if err := unix.Fsync(parent); err != nil {
		return ErrWorkspaceFile
	}
	committed = true
	return nil
}

func (f *WorkspaceFiles) openParent(workspacePath string) (int, string, error) {
	if !validWorkspaceFilePath(workspacePath) {
		return -1, "", ErrWorkspaceFile
	}
	relative := strings.TrimPrefix(workspacePath, "/workspace/")
	components := strings.Split(relative, "/")
	if len(components) == 0 || components[len(components)-1] == "" {
		return -1, "", ErrWorkspaceFile
	}
	fd, err := unix.Dup(f.rootFD)
	if err != nil {
		return -1, "", ErrWorkspaceFile
	}
	for _, component := range components[:len(components)-1] {
		next, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		_ = unix.Close(fd)
		if openErr != nil {
			return -1, "", ErrWorkspaceFile
		}
		fd = next
	}
	return fd, components[len(components)-1], nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func validWorkspaceFilePath(value string) bool {
	return value != "/workspace" && path.IsAbs(value) && path.Clean(value) == value &&
		strings.HasPrefix(value, "/workspace/") && !strings.ContainsAny(value, "\\\x00\r\n")
}
