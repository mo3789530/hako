//go:build linux

package main

import (
	"io"
	"os"
	"os/signal"

	"golang.org/x/sys/unix"
)

// enableWorkspaceTerminal switches a real local terminal to character mode,
// synchronizes its size with the remote PTY, and restores the exact original
// settings on every exit path. Non-terminal readers/writers remain untouched.
func enableWorkspaceTerminal(stdin io.Reader, stdout io.Writer, resize func(uint16, uint16) error) (func(), error) {
	in, ok := stdin.(*os.File)
	if !ok || in == nil {
		return func() {}, nil
	}
	out, ok := stdout.(*os.File)
	if !ok || out == nil {
		return func() {}, nil
	}

	original, err := unix.IoctlGetTermios(int(in.Fd()), unix.TCGETS)
	if err != nil {
		return func() {}, nil
	}
	window, err := unix.IoctlGetWinsize(int(out.Fd()), unix.TIOCGWINSZ)
	if err != nil || window.Col == 0 || window.Row == 0 || window.Col > 500 || window.Row > 500 {
		return func() {}, nil
	}

	raw := *original
	raw.Iflag &^= unix.BRKINT | unix.ICRNL | unix.INPCK | unix.ISTRIP | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Cflag |= unix.CS8
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(int(in.Fd()), unix.TCSETS, &raw); err != nil {
		return nil, err
	}
	restore := func() { _ = unix.IoctlSetTermios(int(in.Fd()), unix.TCSETS, original) }
	if resize != nil {
		if err := resize(window.Col, window.Row); err != nil {
			restore()
			return nil, err
		}
	}

	changed := make(chan os.Signal, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	signal.Notify(changed, unix.SIGWINCH)
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-changed:
				size, err := unix.IoctlGetWinsize(int(out.Fd()), unix.TIOCGWINSZ)
				if err == nil && size.Col > 0 && size.Row > 0 && size.Col <= 500 && size.Row <= 500 && resize != nil {
					_ = resize(size.Col, size.Row)
				}
			}
		}
	}()
	return func() {
		signal.Stop(changed)
		close(stop)
		<-done
		restore()
	}, nil
}
