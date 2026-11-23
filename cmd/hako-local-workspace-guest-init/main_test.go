//go:build linux

package main

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestGuestInitFailureLogLineRedactsWrappedCause(t *testing.T) {
	secret := errors.New("launch /usr/local/bin/agent: permission denied; token=never-log")
	line := guestInitFailureLogLine(guestInitFailure("agent_start", secret))
	if !strings.Contains(line, `failure_class=guest_init stage="agent_start"`) {
		t.Fatalf("log line omitted bounded stage: %q", line)
	}
	for _, forbidden := range []string{"permission denied", "/usr/local/bin", "token=never-log"} {
		if strings.Contains(line, forbidden) {
			t.Fatalf("log line leaked %q: %q", forbidden, line)
		}
	}
}

func TestGuestInitFailureLogLineUsesSafeAgentStartupSubstage(t *testing.T) {
	line := guestInitFailureLogLine(guestInitFailure("agent_start", errors.New("start Workspace SSH server: /private/path/key: sensitive detail")))
	if !strings.Contains(line, `stage="agent_start_ssh_process"`) || strings.Contains(line, "private") || strings.Contains(line, "sensitive") {
		t.Fatalf("unsafe or missing startup substage: %q", line)
	}
}

func TestGuestInitFailureLogLineUsesUnknownForUnclassifiedError(t *testing.T) {
	line := guestInitFailureLogLine(errors.New("database password detail"))
	if line != `Hako local Workspace Guest stopped (failure_class=guest_init stage="unknown")` {
		t.Fatalf("unexpected fallback log line: %q", line)
	}
}

func TestMountGuestPseudoFilesystemsIncludesPrivateDevpts(t *testing.T) {
	var directories []string
	type mountCall struct {
		source, target, filesystem string
		flags                      uintptr
		data                       string
	}
	var mounts []mountCall
	err := mountGuestPseudoFilesystemsWith(func(path string, mode os.FileMode) error {
		if mode != 0755 {
			t.Fatalf("mountpoint %s mode=%o, want 0755", path, mode)
		}
		directories = append(directories, path)
		return nil
	}, func(source, target, filesystem string, flags uintptr, data string) error {
		mounts = append(mounts, mountCall{source: source, target: target, filesystem: filesystem, flags: flags, data: data})
		return nil
	}, func(string) error { return nil }, func(string, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/dev", "/proc", "/sys", localWorkspacePath, "/dev/pts"}; !reflect.DeepEqual(directories, want) {
		t.Fatalf("mountpoints=%v, want %v", directories, want)
	}
	wantMounts := []mountCall{
		{source: "devtmpfs", target: "/dev", filesystem: "devtmpfs", flags: syscall.MS_NOSUID, data: "mode=0755"},
		{source: "devpts", target: "/dev/pts", filesystem: "devpts", flags: syscall.MS_NOSUID | syscall.MS_NOEXEC, data: "newinstance,ptmxmode=0666,mode=0620,gid=5"},
		{source: "proc", target: "/proc", filesystem: "proc", flags: syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC},
		{source: "sysfs", target: "/sys", filesystem: "sysfs", flags: syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC},
	}
	if !reflect.DeepEqual(mounts, wantMounts) {
		t.Fatalf("mount calls=%+v, want %+v", mounts, wantMounts)
	}
}

func TestMountGuestPseudoFilesystemsStopsAfterDevptsFailure(t *testing.T) {
	var mounts []string
	wantErr := errors.New("devpts unavailable")
	err := mountGuestPseudoFilesystemsWith(func(string, os.FileMode) error { return nil },
		func(_ string, target, _ string, _ uintptr, _ string) error {
			mounts = append(mounts, target)
			if target == "/dev/pts" {
				return wantErr
			}
			return nil
		}, func(string) error { return nil }, func(string, string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "mount Guest devpts") {
		t.Fatalf("expected sanitized devpts mount failure, got %v", err)
	}
	if want := []string{"/dev", "/dev/pts"}; !reflect.DeepEqual(mounts, want) {
		t.Fatalf("mounts after devpts failure=%v, want %v", mounts, want)
	}
}

func TestConfigureGuestPTYDeviceLinksPrivateDevptsInstance(t *testing.T) {
	var removed, target, link string
	err := configureGuestPTYDevice(func(path string) error {
		removed = path
		return nil
	}, func(oldPath, newPath string) error {
		link, target = oldPath, newPath
		return nil
	})
	if err != nil || removed != "/dev/ptmx" || link != "pts/ptmx" || target != "/dev/ptmx" {
		t.Fatalf("PTY device setup removed=%q symlink=%q -> %q err=%v", removed, target, link, err)
	}
}

func TestConfigureGuestPTYDeviceAllowsMissingDevtmpfsNodeAndRejectsSymlinkFailure(t *testing.T) {
	err := configureGuestPTYDevice(func(string) error { return os.ErrNotExist }, func(string, string) error { return nil })
	if err != nil {
		t.Fatalf("missing devtmpfs PTY node should be replaceable: %v", err)
	}
	err = configureGuestPTYDevice(func(string) error { return nil }, func(string, string) error { return errors.New("raw path detail") })
	if err == nil || err.Error() != "link Guest PTY multiplexer to its private devpts instance" {
		t.Fatalf("PTY link failure was not sanitized: %v", err)
	}
}
