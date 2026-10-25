//go:build linux

package workloadagent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func readNativeAgentWorkspaceFile(root, rel string, maxBytes int) ([]byte, os.FileMode, error) {
	parent, name, err := openNativeAgentParent(root, rel)
	if err != nil {
		return nil, 0, err
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, 0, os.ErrNotExist
	}
	if err != nil {
		return nil, 0, ErrNativeAgentExecution
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > int64(maxBytes) {
		return nil, 0, ErrNativeAgentExecution
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil || len(data) > maxBytes {
		clear(data)
		return nil, 0, ErrNativeAgentExecution
	}
	return data, info.Mode(), nil
}

func writeNativeAgentWorkspaceFile(root, rel string, content []byte, mode os.FileMode) error {
	parent, name, err := openNativeAgentParent(root, rel)
	if err != nil {
		return ErrNativeAgentExecution
	}
	defer unix.Close(parent)
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return ErrNativeAgentExecution
	}
	tempName := ".hako-agent-" + hex.EncodeToString(random[:])
	fd, err := unix.Openat(parent, tempName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return ErrNativeAgentExecution
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = unix.Unlinkat(parent, tempName, 0)
		}
	}()
	file := os.NewFile(uintptr(fd), tempName)
	if err := file.Chmod(mode.Perm() & 0755); err != nil {
		_ = file.Close()
		return ErrNativeAgentExecution
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return ErrNativeAgentExecution
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return ErrNativeAgentExecution
	}
	if err := file.Close(); err != nil {
		return ErrNativeAgentExecution
	}
	if err := unix.Renameat(parent, tempName, parent, name); err != nil {
		return ErrNativeAgentExecution
	}
	removeTemp = false
	if err := unix.Fsync(parent); err != nil {
		return ErrNativeAgentExecution
	}
	return nil
}

func openNativeAgentParent(root, rel string) (int, string, error) {
	if root == "" || rel == "" || strings.ContainsAny(rel, "\\\r\n\x00:") || strings.HasPrefix(rel, "/") {
		return -1, "", ErrNativeAgentExecution
	}
	parts := strings.Split(rel, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return -1, "", ErrNativeAgentExecution
		}
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return -1, "", ErrNativeAgentExecution
	}
	fd, err := openNativeAgentRoot(absolute)
	if err != nil {
		return -1, "", ErrNativeAgentExecution
	}
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		_ = unix.Close(fd)
		if openErr != nil {
			return -1, "", ErrNativeAgentExecution
		}
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}

func openNativeAgentRoot(absolute string) (int, error) {
	if !filepath.IsAbs(absolute) {
		return -1, ErrNativeAgentExecution
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(absolute, "/"), "/") {
		if part == "" {
			continue
		}
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		_ = unix.Close(fd)
		if openErr != nil {
			return -1, openErr
		}
		fd = next
	}
	return fd, nil
}
