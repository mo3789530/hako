//go:build linux

package workspacejournal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"golang.org/x/sys/unix"
)

const maxFileJournalBytes = 32 << 10
const maxFileJournalEntries = 100000

// FileStore is a Linux host-local Workspace journal. It uses a private
// directory, per-Workspace flock, atomic replacement, and fsync so an
// operation checkpoint is durable before the caller performs VM side effects.
// It is intended for a single host filesystem, not shared network storage.
type FileStore struct {
	root string
}

// OperationLocker provides a process-shared lock spanning a full Workspace
// operation, including provider side effects. Record CAS still uses its own
// short lock so callers can checkpoint while holding this operation lock.
type OperationLocker interface {
	LockOperation(context.Context, domain.TenantID, domain.WorkspaceID, domain.ResourcePlaneID) (func(), error)
}

func NewFileStore(root string) (*FileStore, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || strings.TrimSpace(root) != root {
		return nil, errors.New("Workspace journal directory must be an absolute clean path")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, errors.New("create Workspace journal directory")
	}
	// Persist the root directory entry when it was newly created. The records
	// below are only durable if their containing directory chain is durable too.
	if err := syncDirectory(filepath.Dir(root)); err != nil {
		return nil, err
	}
	requestedInfo, err := os.Lstat(root)
	if err != nil || !requestedInfo.IsDir() || requestedInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("Workspace journal directory cannot be a symlink")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || !filepath.IsAbs(resolved) {
		return nil, errors.New("resolve Workspace journal directory")
	}
	info, err := os.Lstat(resolved)
	stat, ok := func() (*syscall.Stat_t, bool) {
		if info == nil {
			return nil, false
		}
		value, valid := info.Sys().(*syscall.Stat_t)
		return value, valid
	}()
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || stat.Uid != uint32(os.Getuid()) {
		return nil, errors.New("Workspace journal directory must be owned by the current user and not be a symlink")
	}
	if err := os.Chmod(resolved, 0700); err != nil {
		return nil, errors.New("restrict Workspace journal directory permissions")
	}
	return &FileStore{root: resolved}, nil
}

func (s *FileStore) Create(ctx context.Context, journal Journal) (Snapshot, error) {
	if s == nil || ctx == nil || ctx.Err() != nil || journal.Revision != 1 {
		return Snapshot{}, ErrConflict
	}
	path, err := s.journalPath(journal.TenantID, journal.WorkspaceID, journal.ResourcePlaneID, true)
	if err != nil {
		return Snapshot{}, err
	}
	unlock, err := s.lock(ctx, path)
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	body, err := Encode(journal)
	if err != nil {
		return Snapshot{}, err
	}
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if errors.Is(err, unix.EEXIST) {
		return Snapshot{}, ErrConflict
	}
	if err != nil {
		return Snapshot{}, errors.New("create Workspace journal record")
	}
	file := os.NewFile(uintptr(fd), path)
	if err := writeSyncClose(file, body); err != nil {
		_ = os.Remove(path)
		return Snapshot{}, err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Journal: journal, ETag: digestETag(body)}, nil
}

func (s *FileStore) Get(ctx context.Context, tenantID domain.TenantID, workspaceID domain.WorkspaceID, resourcePlaneID domain.ResourcePlaneID) (Snapshot, error) {
	if s == nil || ctx == nil || ctx.Err() != nil {
		return Snapshot{}, ErrConflict
	}
	path, err := s.journalPath(tenantID, workspaceID, resourcePlaneID, false)
	if err != nil {
		return Snapshot{}, err
	}
	return s.readSnapshot(path, tenantID, workspaceID, resourcePlaneID)
}

func (s *FileStore) CompareAndSwap(ctx context.Context, current Snapshot, next Journal) (Snapshot, error) {
	if s == nil || ctx == nil || ctx.Err() != nil || ValidateUpdate(current, next) != nil {
		return Snapshot{}, ErrConflict
	}
	path, err := s.journalPath(next.TenantID, next.WorkspaceID, next.ResourcePlaneID, false)
	if err != nil {
		return Snapshot{}, err
	}
	unlock, err := s.lock(ctx, path)
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	stored, err := s.readSnapshot(path, next.TenantID, next.WorkspaceID, next.ResourcePlaneID)
	if err != nil {
		return Snapshot{}, err
	}
	if stored.ETag != current.ETag {
		return Snapshot{}, ErrConflict
	}
	body, err := Encode(next)
	if err != nil {
		return Snapshot{}, err
	}
	if err := atomicReplace(path, body); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Journal: next, ETag: digestETag(body)}, nil
}

// LockOperation serializes lifecycle operations for one Workspace across
// controller processes. Its lock file is separate from the short record lock,
// so the holder can safely call Create/Get/CompareAndSwap before unlocking.
func (s *FileStore) LockOperation(ctx context.Context, tenantID domain.TenantID, workspaceID domain.WorkspaceID,
	resourcePlaneID domain.ResourcePlaneID) (func(), error) {
	if s == nil || ctx == nil || ctx.Err() != nil {
		return nil, ErrConflict
	}
	path, err := s.journalPath(tenantID, workspaceID, resourcePlaneID, true)
	if err != nil {
		return nil, err
	}
	return s.lock(ctx, path+".operation")
}

// ListJournals returns a bounded, stable snapshot of host allocation records
// for startup reconciliation. Callers must re-read and CAS each record before
// making a provider change because entries can advance after enumeration.
func (s *FileStore) ListJournals(ctx context.Context, limit int) ([]Snapshot, error) {
	if s == nil || ctx == nil || ctx.Err() != nil || limit < 1 || limit > maxFileJournalEntries {
		return nil, errors.New("Workspace journal listing bounds are invalid")
	}
	var result []Snapshot
	err := filepath.WalkDir(s.root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == s.root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("Workspace journal contains a symlink")
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		relative, err := filepath.Rel(s.root, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if len(parts) != 3 || !idPattern.MatchString(parts[0]) || !idPattern.MatchString(parts[1]) ||
			!strings.HasSuffix(parts[2], ".json") || !idPattern.MatchString(strings.TrimSuffix(parts[2], ".json")) {
			return errors.New("Workspace journal directory contains an invalid record path")
		}
		if len(result) >= limit {
			return errors.New("Workspace journal listing exceeds configured limit")
		}
		snapshot, err := s.readSnapshot(path, domain.TenantID(parts[1]), domain.WorkspaceID(strings.TrimSuffix(parts[2], ".json")), domain.ResourcePlaneID(parts[0]))
		if err != nil {
			return err
		}
		result = append(result, snapshot)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list Workspace journals: %w", err)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i].Journal, result[j].Journal
		if a.ResourcePlaneID != b.ResourcePlaneID {
			return a.ResourcePlaneID < b.ResourcePlaneID
		}
		if a.TenantID != b.TenantID {
			return a.TenantID < b.TenantID
		}
		return a.WorkspaceID < b.WorkspaceID
	})
	return result, nil
}

func (s *FileStore) readSnapshot(path string, tenantID domain.TenantID, workspaceID domain.WorkspaceID, planeID domain.ResourcePlaneID) (Snapshot, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, errors.New("open Workspace journal record")
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Size < 1 || stat.Size > maxFileJournalBytes {
		return Snapshot{}, errors.New("Workspace journal file type or size is invalid")
	}
	body, err := io.ReadAll(io.LimitReader(file, maxFileJournalBytes+1))
	if err != nil || len(body) > maxFileJournalBytes {
		return Snapshot{}, errors.New("read Workspace journal record")
	}
	journal, err := Decode(body)
	if err != nil || journal.TenantID != tenantID || journal.WorkspaceID != workspaceID || journal.ResourcePlaneID != planeID {
		return Snapshot{}, errors.New("Workspace journal path does not match its record identity")
	}
	if stat.Mode&077 != 0 || stat.Uid != uint32(os.Getuid()) {
		return Snapshot{}, errors.New("Workspace journal record permissions or owner are invalid")
	}
	return Snapshot{Journal: journal, ETag: digestETag(body)}, nil
}

func (s *FileStore) journalPath(tenantID domain.TenantID, workspaceID domain.WorkspaceID, planeID domain.ResourcePlaneID, createDirs bool) (string, error) {
	if !idPattern.MatchString(string(tenantID)) || !idPattern.MatchString(string(workspaceID)) || !idPattern.MatchString(string(planeID)) {
		return "", errors.New("Workspace journal identity is invalid")
	}
	planeDir := filepath.Join(s.root, string(planeID))
	tenantDir := filepath.Join(planeDir, string(tenantID))
	if createDirs {
		for _, dir := range []string{planeDir, tenantDir} {
			created, err := mkdirPrivate(dir)
			if err != nil {
				return "", errors.New("create Workspace journal identity directory")
			}
			if created {
				if err := syncDirectory(filepath.Dir(dir)); err != nil {
					return "", err
				}
			}
		}
	}
	for _, dir := range []string{planeDir, tenantDir} {
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) && !createDirs {
			return filepath.Join(tenantDir, string(workspaceID)+".json"), nil
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("Workspace journal identity directory is invalid")
		}
		if err := os.Chmod(dir, 0700); err != nil {
			return "", errors.New("restrict Workspace journal identity directory")
		}
	}
	return filepath.Join(tenantDir, string(workspaceID)+".json"), nil
}

// mkdirPrivate creates one journal path component at a time so the parent
// directory can be synced immediately after each new identity directory.
func mkdirPrivate(path string) (bool, error) {
	err := os.Mkdir(path, 0700)
	created := err == nil
	if err != nil && !errors.Is(err, os.ErrExist) {
		return false, err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("Workspace journal identity directory is invalid")
	}
	if err := os.Chmod(path, 0700); err != nil {
		return false, err
	}
	return created, nil
}

func (s *FileStore) lock(ctx context.Context, path string) (func(), error) {
	lockPath := path + ".lock"
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, errors.New("open Workspace journal lock")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&077 != 0 || stat.Uid != uint32(os.Getuid()) {
		_ = unix.Close(fd)
		return nil, errors.New("Workspace journal lock file is invalid")
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			_ = unix.Close(fd)
			return nil, errors.New("lock Workspace journal record")
		}
		select {
		case <-ctx.Done():
			_ = unix.Close(fd)
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = unix.Close(fd) }, nil
}

func atomicReplace(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".workspace-journal-*.tmp")
	if err != nil {
		return errors.New("create Workspace journal replacement")
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return errors.New("restrict Workspace journal replacement")
	}
	if err := writeSyncClose(tmp, body); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return errors.New("replace Workspace journal record")
	}
	return syncDirectory(dir)
}

func writeSyncClose(file *os.File, body []byte) error {
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return errors.New("write Workspace journal record")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errors.New("sync Workspace journal record")
	}
	if err := file.Close(); err != nil {
		return errors.New("close Workspace journal record")
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return errors.New("open Workspace journal directory for sync")
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return errors.New("sync Workspace journal directory")
	}
	return nil
}

func digestETag(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

var _ Store = (*FileStore)(nil)
var _ OperationLocker = (*FileStore)(nil)
