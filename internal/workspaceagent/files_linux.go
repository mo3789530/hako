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
	"time"

	"github.com/mo3789530/hako/internal/workloadsource"
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
	root   string
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
	return &WorkspaceFiles{rootFD: fd, root: root, max: MaxWorkspaceFileBytes}, nil
}

// InstallSourceArchive atomically installs a verified repository archive at
// the fixed Preview source path. An EFS-visible advisory lock serializes
// installers while allowing the OS to release it if the Guest process exits.
// The Preview Workspace is not published until this operation returns. A
// retry succeeds only when existing content has the same deterministic hash.
func (f *WorkspaceFiles) InstallSourceArchive(ctx context.Context, size int64, digest string, src io.Reader) error {
	decoded, digestErr := hex.DecodeString(digest)
	if f == nil || ctx == nil || src == nil || size < 1 || size > MaxWorkspaceFileBytes || digestErr != nil ||
		len(decoded) != 32 || hex.EncodeToString(decoded) != digest {
		return ErrWorkspaceFile
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.rootFD < 0 || ctx.Err() != nil {
		return ErrWorkspaceFile
	}
	if err := f.verifyExistingSource(ctx, digest); err == nil {
		return nil
	} else if !errors.Is(err, unix.ENOENT) {
		return ErrWorkspaceFile
	}
	lockFD, err := f.lockPreviewSource(ctx)
	if err != nil {
		return ErrWorkspaceFile
	}
	defer func() {
		_ = unix.Flock(lockFD, unix.LOCK_UN)
		_ = unix.Close(lockFD)
	}()
	if err := f.verifyExistingSource(ctx, digest); err == nil {
		return nil
	} else if !errors.Is(err, unix.ENOENT) {
		return ErrWorkspaceFile
	}
	temporary, err := os.MkdirTemp(f.root, ".hako-preview-source-")
	if err != nil {
		return ErrWorkspaceFile
	}
	defer os.RemoveAll(temporary)
	if err := os.Chmod(temporary, 0700); err != nil {
		return ErrWorkspaceFile
	}
	content := path.Join(temporary, "content")
	if err := workloadsource.ExtractArchive(ctx, io.LimitReader(src, size), size, digest, content, f.max); err != nil {
		return ErrWorkspaceFile
	}
	if err := unix.Renameat(unix.AT_FDCWD, content, f.rootFD, "src"); err != nil {
		return ErrWorkspaceFile
	}
	if err := unix.Fsync(f.rootFD); err != nil {
		return ErrWorkspaceFile
	}
	return nil
}

func (f *WorkspaceFiles) lockPreviewSource(ctx context.Context) (int, error) {
	fd, err := unix.Openat(f.rootFD, ".hako-preview-source-install.lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return -1, ErrWorkspaceFile
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return -1, ErrWorkspaceFile
	}
	for {
		if ctx.Err() != nil {
			_ = unix.Close(fd)
			return -1, ErrWorkspaceFile
		}
		if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err == nil {
			return fd, nil
		} else if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = unix.Close(fd)
			return -1, ErrWorkspaceFile
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = unix.Close(fd)
			return -1, ErrWorkspaceFile
		case <-timer.C:
		}
	}
}

func (f *WorkspaceFiles) verifyExistingSource(ctx context.Context, digest string) error {
	fd, err := unix.Openat(f.rootFD, "src", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	_ = unix.Close(fd)
	archive, err := workloadsource.CreateArchive(ctx, path.Join(f.root, "src"), f.max)
	if err != nil {
		return ErrWorkspaceFile
	}
	defer archive.Close()
	if archive.SHA256() != digest {
		return ErrWorkspaceFile
	}
	return nil
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
