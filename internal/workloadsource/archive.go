package workloadsource

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/workloadjournal"
)

const (
	DefaultMaxArchiveBytes = 1 << 30
	copyBufferSize         = 32 << 10
	maxArchiveEntries      = 100000
)

var ErrArchive = errors.New("Workload source archive is invalid or exceeds its limit")

var sourceDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// MaterializedSource identifies the content-addressed source archive and its
// Tenant/Run/attempt-scoped object prefix.
type MaterializedSource struct {
	Digest         string
	ArtifactPrefix string
}

type SourceArchive struct {
	Body io.ReadCloser
	Size int64
}

// SourceArchiveReader fetches only the content-addressed archive bound to the
// supplied immutable attempt and exact artifact prefix.
type SourceArchiveReader interface {
	OpenSourceArchive(context.Context, workloadjournal.Command, string, string) (SourceArchive, error)
}

// Archive is a deterministic tar stream in a private temporary file. Close
// removes the file. Its digest covers the exact bytes that must be staged and
// later verified by the MicroVM Agent.
type Archive struct {
	file   *os.File
	size   int64
	digest string
}

func (a *Archive) Reader() io.ReadSeeker {
	if a == nil {
		return nil
	}
	return a.file
}

func (a *Archive) Size() int64 {
	if a == nil {
		return 0
	}
	return a.size
}

func (a *Archive) SHA256() string {
	if a == nil {
		return ""
	}
	return a.digest
}

func (a *Archive) Close() error {
	if a == nil || a.file == nil {
		return nil
	}
	name := a.file.Name()
	closeErr := a.file.Close()
	removeErr := os.Remove(name)
	a.file = nil
	if closeErr != nil || removeErr != nil {
		return ErrArchive
	}
	return nil
}

// CreateArchive packages a trusted checkout without .git metadata. It rejects
// symlinks and special files so extraction never follows repository-controlled
// filesystem links. File order and metadata are normalized for stable retries.
func CreateArchive(ctx context.Context, root string, maxBytes int64) (*Archive, error) {
	if ctx == nil || strings.TrimSpace(root) == "" {
		return nil, ErrArchive
	}
	if maxBytes <= 0 || maxBytes > DefaultMaxArchiveBytes {
		maxBytes = DefaultMaxArchiveBytes
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, ErrArchive
	}
	rootInfo, err := os.Lstat(absoluteRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, ErrArchive
	}
	file, err := os.CreateTemp("", "hako-source-*.tar")
	if err != nil {
		return nil, ErrArchive
	}
	name := file.Name()
	failed := func() (*Archive, error) {
		_ = file.Close()
		_ = os.Remove(name)
		return nil, ErrArchive
	}
	if err := file.Chmod(0o600); err != nil {
		return failed()
	}
	hasher := sha256.New()
	writer := &boundedHashWriter{writer: io.MultiWriter(file, hasher), max: maxBytes}
	tarWriter := tar.NewWriter(writer)
	var totalFileBytes int64
	entries := 0
	walkErr := filepath.WalkDir(absoluteRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || ctx.Err() != nil {
			return ErrArchive
		}
		relative, err := filepath.Rel(absoluteRoot, path)
		if err != nil {
			return ErrArchive
		}
		if relative == "." {
			return nil
		}
		if entry.Name() == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entries++
		if entries > maxArchiveEntries {
			return ErrArchive
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return ErrArchive
		}
		name := filepath.ToSlash(relative)
		if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "../") || name == ".." || strings.ContainsRune(name, '\x00') {
			return ErrArchive
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return ErrArchive
		}
		header.Name = name
		header.Linkname = ""
		header.Uid, header.Gid = 0, 0
		header.Uname, header.Gname = "", ""
		header.ModTime = time.Unix(0, 0).UTC()
		header.AccessTime, header.ChangeTime = time.Time{}, time.Time{}
		header.PAXRecords = nil
		header.Format = tar.FormatPAX
		if info.IsDir() {
			header.Name += "/"
			header.Mode = 0o755
			header.Size = 0
		} else {
			if info.Size() < 0 || info.Size() > maxBytes-totalFileBytes {
				return ErrArchive
			}
			if info.Mode().Perm()&0o111 != 0 {
				header.Mode = 0o755
			} else {
				header.Mode = 0o644
			}
			header.Size = info.Size()
			totalFileBytes += info.Size()
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			return ErrArchive
		}
		if info.IsDir() {
			return nil
		}
		input, err := os.Open(path)
		if err != nil {
			return ErrArchive
		}
		openedInfo, statErr := input.Stat()
		if statErr != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) || openedInfo.Size() != info.Size() {
			_ = input.Close()
			return ErrArchive
		}
		_, copyErr := copyContext(ctx, tarWriter, input)
		closeErr := input.Close()
		if copyErr != nil || closeErr != nil {
			return ErrArchive
		}
		return nil
	})
	if walkErr != nil || ctx.Err() != nil || tarWriter.Close() != nil || writer.err != nil {
		return failed()
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return failed()
	}
	return &Archive{file: file, size: writer.size, digest: hex.EncodeToString(hasher.Sum(nil))}, nil
}

type boundedHashWriter struct {
	writer io.Writer
	max    int64
	size   int64
	err    error
}

func (w *boundedHashWriter) Write(data []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if int64(len(data)) > w.max-w.size {
		w.err = ErrArchive
		return 0, w.err
	}
	n, err := w.writer.Write(data)
	w.size += int64(n)
	if err != nil || n != len(data) {
		w.err = ErrArchive
		return n, w.err
	}
	return n, nil
}

func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buffer := make([]byte, copyBufferSize)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buffer)
		if n > 0 {
			written, writeErr := dst.Write(buffer[:n])
			total += int64(written)
			if writeErr != nil || written != n {
				return total, fmt.Errorf("write archive file: %w", ErrArchive)
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, ErrArchive
		}
	}
}

// ExtractArchive verifies the complete archive digest before writing any
// repository file. The destination must not already exist; extracted entries
// are regular files/directories only, with ownership and timestamps discarded.
func ExtractArchive(ctx context.Context, source io.Reader, archiveSize int64, expectedDigest, destination string, maxBytes int64) error {
	if ctx == nil || source == nil || archiveSize < 1 || !sourceDigestPattern.MatchString(expectedDigest) || strings.TrimSpace(destination) == "" {
		return ErrArchive
	}
	if maxBytes <= 0 || maxBytes > DefaultMaxArchiveBytes {
		maxBytes = DefaultMaxArchiveBytes
	}
	if archiveSize > maxBytes {
		return ErrArchive
	}
	verified, err := os.CreateTemp("", "hako-source-verified-*.tar")
	if err != nil {
		return ErrArchive
	}
	verifiedName := verified.Name()
	defer func() {
		_ = verified.Close()
		_ = os.Remove(verifiedName)
	}()
	if verified.Chmod(0o600) != nil {
		return ErrArchive
	}
	hasher := sha256.New()
	size, err := copyContext(ctx, io.MultiWriter(verified, hasher), io.LimitReader(source, maxBytes+1))
	if err != nil || size != archiveSize || size > maxBytes || subtle.ConstantTimeCompare([]byte(hex.EncodeToString(hasher.Sum(nil))), []byte(expectedDigest)) != 1 {
		return ErrArchive
	}
	var extra [1]byte
	if n, readErr := source.Read(extra[:]); n != 0 || (readErr != nil && readErr != io.EOF) {
		return ErrArchive
	}
	if _, err := verified.Seek(0, io.SeekStart); err != nil {
		return ErrArchive
	}
	absDestination, err := filepath.Abs(destination)
	if err != nil {
		return ErrArchive
	}
	if err := os.Mkdir(absDestination, 0o700); err != nil {
		return ErrArchive
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(absDestination)
		}
	}()
	archive := tar.NewReader(verified)
	seen := make(map[string]struct{})
	var total int64
	entries := 0
	for {
		if err := ctx.Err(); err != nil {
			return ErrArchive
		}
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ErrArchive
		}
		entries++
		if entries > maxArchiveEntries {
			return ErrArchive
		}
		name := strings.TrimSuffix(header.Name, "/")
		if !validArchivePath(name) {
			return ErrArchive
		}
		if _, exists := seen[name]; exists {
			return ErrArchive
		}
		seen[name] = struct{}{}
		target := filepath.Join(absDestination, filepath.FromSlash(name))
		if !withinRoot(absDestination, target) {
			return ErrArchive
		}
		if header.Typeflag == tar.TypeDir {
			if header.Size != 0 || os.MkdirAll(target, 0o755) != nil {
				return ErrArchive
			}
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA || header.Size < 0 || header.Size > maxBytes-total {
			return ErrArchive
		}
		total += header.Size
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return ErrArchive
		}
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, archiveMode(header.Mode))
		if err != nil {
			return ErrArchive
		}
		_, copyErr := copyExactContext(ctx, file, archive, header.Size)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return ErrArchive
		}
		if err := os.Chmod(target, archiveMode(header.Mode)); err != nil {
			return ErrArchive
		}
	}
	cleanup = false
	return nil
}

func validArchivePath(value string) bool {
	if value == "" || len(value) > 4096 || strings.ContainsRune(value, '\\') || strings.ContainsRune(value, '\x00') || strings.HasPrefix(value, "/") ||
		path.Clean(value) != value || value == "." || value == ".." {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

func withinRoot(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func archiveMode(mode int64) os.FileMode {
	if mode&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

func copyExactContext(ctx context.Context, destination io.Writer, source io.Reader, size int64) (int64, error) {
	reader := &contextReader{ctx: ctx, reader: io.LimitReader(source, size)}
	n, err := io.Copy(destination, reader)
	if err != nil || n != size {
		return n, ErrArchive
	}
	return n, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

var _ io.Closer = (*Archive)(nil)
