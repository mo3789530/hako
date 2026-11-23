//go:build linux

package workspaceagent

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const WorkspaceAgentVsockPort uint32 = 7000

// ListenWorkspaceAgentVsock creates the host-private Guest listener used by a
// local KVM Gateway. The caller must attach a vsock device to the VM;
// the listener itself still serves the normal Workspace Agent HTTP/WebSocket
// protocol, including its signed Agent-ticket checks.
func ListenWorkspaceAgentVsock(port uint32) (net.Listener, error) {
	if port == 0 {
		return nil, errors.New("Workspace Agent vsock port must be nonzero")
	}
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("create Workspace Agent vsock socket")
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		_ = unix.Close(fd)
		return nil, errors.New("bind Workspace Agent vsock socket")
	}
	if err := unix.Listen(fd, 64); err != nil {
		_ = unix.Close(fd)
		return nil, errors.New("listen on Workspace Agent vsock socket")
	}
	return &workspaceVsockListener{fd: fd, addr: workspaceVsockAddr{cid: unix.VMADDR_CID_ANY, port: port}}, nil
}

type workspaceVsockListener struct {
	mu     sync.Mutex
	fd     int
	addr   workspaceVsockAddr
	closed bool
}

func (l *workspaceVsockListener) Accept() (net.Conn, error) {
	if l == nil {
		return nil, net.ErrClosed
	}
	for {
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return nil, net.ErrClosed
		}
		fd := l.fd
		ready := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		_, err := unix.Poll(ready, 100)
		if err == unix.EINTR || (err == nil && ready[0].Revents == 0) {
			l.mu.Unlock()
			continue
		}
		if err != nil {
			closed := l.closed
			l.mu.Unlock()
			if closed {
				return nil, net.ErrClosed
			}
			return nil, &net.OpError{Op: "accept", Net: "vsock", Err: err}
		}
		connFD, remote, err := unix.Accept4(fd, unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK)
		l.mu.Unlock()
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK || err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, &net.OpError{Op: "accept", Net: "vsock", Err: err}
		}
		remoteVM, ok := remote.(*unix.SockaddrVM)
		if !ok {
			_ = unix.Close(connFD)
			return nil, errors.New("Workspace Agent received a non-vsock peer")
		}
		file := os.NewFile(uintptr(connFD), "workspace-agent-vsock")
		if file == nil {
			_ = unix.Close(connFD)
			return nil, errors.New("wrap Workspace Agent vsock connection")
		}
		return &workspaceVsockConn{file: file, local: l.addr,
			remote: workspaceVsockAddr{cid: remoteVM.CID, port: remoteVM.Port}}, nil
	}
}

func (l *workspaceVsockListener) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	err := unix.Close(l.fd)
	l.fd = -1
	if err != nil {
		return &net.OpError{Op: "close", Net: "vsock", Err: err}
	}
	return nil
}

func (l *workspaceVsockListener) Addr() net.Addr {
	if l == nil {
		return workspaceVsockAddr{}
	}
	return l.addr
}

type workspaceVsockAddr struct {
	cid  uint32
	port uint32
}

func (a workspaceVsockAddr) Network() string { return "vsock" }
func (a workspaceVsockAddr) String() string  { return fmt.Sprintf("%d:%d", a.cid, a.port) }

type workspaceVsockConn struct {
	file   *os.File
	local  net.Addr
	remote net.Addr
}

func (c *workspaceVsockConn) Read(p []byte) (int, error)         { return c.file.Read(p) }
func (c *workspaceVsockConn) Write(p []byte) (int, error)        { return c.file.Write(p) }
func (c *workspaceVsockConn) Close() error                       { return c.file.Close() }
func (c *workspaceVsockConn) LocalAddr() net.Addr                { return c.local }
func (c *workspaceVsockConn) RemoteAddr() net.Addr               { return c.remote }
func (c *workspaceVsockConn) SetDeadline(t time.Time) error      { return c.file.SetDeadline(t) }
func (c *workspaceVsockConn) SetReadDeadline(t time.Time) error  { return c.file.SetReadDeadline(t) }
func (c *workspaceVsockConn) SetWriteDeadline(t time.Time) error { return c.file.SetWriteDeadline(t) }

var _ net.Listener = (*workspaceVsockListener)(nil)
var _ net.Conn = (*workspaceVsockConn)(nil)
