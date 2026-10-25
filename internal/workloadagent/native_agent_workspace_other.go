//go:build !linux

package workloadagent

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func readNativeAgentWorkspaceFile(root, rel string, maxBytes int) ([]byte, os.FileMode, error) {
	name, info, err := resolveNativeAgentWorkspacePath(root, rel, false)
	if err != nil {
		return nil, 0, err
	}
	if info.Size() > int64(maxBytes) {
		return nil, 0, ErrNativeAgentExecution
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, 0, ErrNativeAgentExecution
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil || len(data) > maxBytes {
		clear(data)
		return nil, 0, ErrNativeAgentExecution
	}
	return data, info.Mode(), nil
}

func writeNativeAgentWorkspaceFile(root, rel string, content []byte, mode os.FileMode) error {
	name, _, err := resolveNativeAgentWorkspacePath(root, rel, true)
	if err != nil {
		return ErrNativeAgentExecution
	}
	tmp, err := os.CreateTemp(filepath.Dir(name), ".hako-agent-*")
	if err != nil {
		return ErrNativeAgentExecution
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode.Perm() & 0755); err != nil {
		_ = tmp.Close()
		return ErrNativeAgentExecution
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return ErrNativeAgentExecution
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return ErrNativeAgentExecution
	}
	if err := tmp.Close(); err != nil || os.Rename(tmpName, name) != nil {
		return ErrNativeAgentExecution
	}
	return nil
}

func resolveNativeAgentWorkspacePath(root, rel string, allowNew bool) (string, os.FileInfo, error) {
	if root == "" || !filepath.IsLocal(rel) || strings.ContainsAny(rel, "\\\r\n\x00:") {
		return "", nil, ErrNativeAgentExecution
	}
	current := root
	parts := strings.Split(filepath.FromSlash(rel), string(filepath.Separator))
	var final os.FileInfo
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && allowNew && i == len(parts)-1 {
			final = nil
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (i < len(parts)-1 && !info.IsDir()) || (i == len(parts)-1 && !info.Mode().IsRegular()) {
			return "", nil, ErrNativeAgentExecution
		}
		final = info
	}
	return current, final, nil
}
