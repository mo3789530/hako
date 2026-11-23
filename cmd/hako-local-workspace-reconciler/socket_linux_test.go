//go:build linux

package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestListenPrivateSocketCreatesOwnedMode0600Socket(t *testing.T) {
	dir := privateSocketTestDir(t)
	path := filepath.Join(dir, "controller.sock")
	listener, err := listenPrivateSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closePrivateSocket(listener, path)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("controller socket metadata = mode %v, err %v", info.Mode(), err)
	}
	connection, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("connect to private controller socket: %v", err)
	}
	_ = connection.Close()
}

func TestListenPrivateSocketRejectsActiveAndForeignPaths(t *testing.T) {
	dir := privateSocketTestDir(t)
	path := filepath.Join(dir, "controller.sock")
	listener, err := listenPrivateSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := listenPrivateSocket(path); err == nil {
		t.Fatal("active socket was replaced")
	}
	closePrivateSocket(listener, path)

	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenPrivateSocket(regular); err == nil {
		t.Fatal("regular file path was replaced by a socket")
	}
	if data, err := os.ReadFile(regular); err != nil || string(data) != "keep" {
		t.Fatalf("regular file changed: data=%q err=%v", data, err)
	}

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := listenPrivateSocket(filepath.Join(dir, "insecure.sock")); err == nil {
		t.Fatal("socket in a group/world-accessible directory was accepted")
	}
}

func TestListenPrivateSocketReplacesOnlyStaleSocket(t *testing.T) {
	path := filepath.Join(privateSocketTestDir(t), "stale.sock")
	listener, err := listenPrivateSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	newListener, err := listenPrivateSocket(path)
	if err != nil {
		t.Fatalf("replace stale owned socket: %v", err)
	}
	closePrivateSocket(newListener, path)
}

func privateSocketTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
