//go:build linux

package main

import (
	"errors"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func openWorkspaceTestPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("pseudo terminals are unavailable: %v", err)
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		_ = master.Close()
		t.Skipf("cannot unlock pseudo terminal: %v", err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		_ = master.Close()
		t.Skipf("cannot read pseudo terminal number: %v", err)
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(number), os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = master.Close()
		t.Skipf("cannot open pseudo terminal slave: %v", err)
	}
	t.Cleanup(func() {
		_ = slave.Close()
		_ = master.Close()
	})
	return master, slave
}

func TestEnableWorkspaceTerminalUsesRestoresAndResizesPTY(t *testing.T) {
	_, terminal := openWorkspaceTestPTY(t)
	if err := unix.IoctlSetWinsize(int(terminal.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: 100, Row: 40}); err != nil {
		t.Skipf("cannot set pseudo terminal size: %v", err)
	}
	original, err := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
	if err != nil {
		t.Skipf("cannot inspect pseudo terminal settings: %v", err)
	}
	resized := make(chan [2]uint16, 2)
	restore, err := enableWorkspaceTerminal(terminal, terminal, func(columns, rows uint16) error {
		resized <- [2]uint16{columns, rows}
		return nil
	})
	if err != nil {
		t.Fatalf("enableWorkspaceTerminal() error = %v", err)
	}
	select {
	case size := <-resized:
		if size != [2]uint16{100, 40} {
			t.Fatalf("initial resize = %v, want [100 40]", size)
		}
	case <-time.After(time.Second):
		restore()
		t.Fatal("initial terminal size was not sent")
	}
	raw, err := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
	if err != nil {
		restore()
		t.Fatal(err)
	}
	if raw.Lflag&(unix.ECHO|unix.ICANON|unix.IEXTEN) != 0 || raw.Oflag&unix.OPOST != 0 || raw.Cflag&unix.CS8 == 0 {
		restore()
		t.Fatalf("terminal was not placed in character mode: %+v", raw)
	}
	if err := unix.IoctlSetWinsize(int(terminal.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: 132, Row: 51}); err != nil {
		restore()
		t.Fatal(err)
	}
	if err := unix.Kill(os.Getpid(), unix.SIGWINCH); err != nil {
		restore()
		t.Fatal(err)
	}
	select {
	case size := <-resized:
		if size != [2]uint16{132, 51} {
			restore()
			t.Fatalf("resized terminal size = %v, want [132 51]", size)
		}
	case <-time.After(time.Second):
		restore()
		t.Fatal("SIGWINCH did not update the remote terminal size")
	}
	restore()
	restored, err := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, original) {
		t.Fatalf("terminal settings after restore = %+v, want %+v", restored, original)
	}
}

func TestEnableWorkspaceTerminalRestoresWhenInitialResizeFails(t *testing.T) {
	_, terminal := openWorkspaceTestPTY(t)
	if err := unix.IoctlSetWinsize(int(terminal.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: 80, Row: 24}); err != nil {
		t.Skipf("cannot set pseudo terminal size: %v", err)
	}
	original, err := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
	if err != nil {
		t.Skipf("cannot inspect pseudo terminal settings: %v", err)
	}
	wantErr := errors.New("resize rejected")
	restore, err := enableWorkspaceTerminal(terminal, terminal, func(uint16, uint16) error { return wantErr })
	if !errors.Is(err, wantErr) || restore != nil {
		t.Fatalf("enableWorkspaceTerminal() returned restore=%t, error=%v; want resize error and nil restore", restore != nil, err)
	}
	restored, err := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, original) {
		t.Fatalf("terminal settings after rejected resize = %+v, want %+v", restored, original)
	}
}

func TestEnableWorkspaceTerminalLeavesNonTerminalAndInvalidSizeUntouched(t *testing.T) {
	if restore, err := enableWorkspaceTerminal(nil, nil, nil); err != nil || restore == nil {
		t.Fatalf("non-terminal enable returned restore=%t, error=%v; want no-op restore", restore != nil, err)
	}
	_, terminal := openWorkspaceTestPTY(t)
	for _, size := range []unix.Winsize{{Col: 0, Row: 24}, {Col: 80, Row: 0}, {Col: 501, Row: 24}} {
		if err := unix.IoctlSetWinsize(int(terminal.Fd()), unix.TIOCSWINSZ, &size); err != nil {
			t.Skipf("cannot set pseudo terminal size: %v", err)
		}
		before, err := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		restore, err := enableWorkspaceTerminal(terminal, terminal, nil)
		if err != nil || restore == nil {
			t.Fatalf("invalid-size enable returned restore=%t, error=%v; want no-op restore", restore != nil, err)
		}
		restore()
		after, err := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("invalid window size changed terminal settings: before=%+v after=%+v", before, after)
		}
	}
}
