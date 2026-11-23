//go:build linux

package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func listenPrivateSocket(path string) (net.Listener, error) {
	if !cleanAbsolute(path) || len(path) >= 104 {
		return nil, errors.New("controller socket path is invalid")
	}
	parent := filepath.Dir(path)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil || resolvedParent != parent {
		return nil, errors.New("controller socket parent must be an existing canonical directory")
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 || parentInfo.Mode().Perm()&0077 != 0 {
		return nil, errors.New("controller socket parent must be private")
	}
	parentStat, ok := parentInfo.Sys().(*syscall.Stat_t)
	if !ok || parentStat.Uid != uint32(os.Getuid()) {
		return nil, errors.New("controller socket parent must be owned by the current user")
	}
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("refusing to replace a non-socket controller path")
		}
		stat, valid := info.Sys().(*syscall.Stat_t)
		if !valid || stat.Uid != uint32(os.Getuid()) {
			return nil, errors.New("refusing to replace a foreign controller socket")
		}
		conn, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return nil, errors.New("local Workspace controller socket is already active")
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, syscall.ENOENT) {
			return nil, errors.New("cannot determine whether local Workspace controller socket is stale")
		}
		if err := os.Remove(path); err != nil {
			return nil, errors.New("remove stale local Workspace controller socket")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, errors.New("inspect local Workspace controller socket")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, errors.New("create local Workspace controller socket")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, errors.New("protect local Workspace controller socket")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, errors.New("verify private local Workspace controller socket")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, errors.New("verify local Workspace controller socket owner")
	}
	return listener, nil
}

func closePrivateSocket(listener net.Listener, path string) {
	if listener != nil {
		_ = listener.Close()
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid == uint32(os.Getuid()) {
			_ = os.Remove(path)
		}
	}
}
