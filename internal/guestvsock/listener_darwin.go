//go:build darwin

// Package guestvsock listens on a virtio socket port inside a macOS guest. XNU
// has AF_VSOCK since macOS 11 and golang.org/x/sys/unix carries its sockaddr,
// but the net package knows nothing of the family, so the listener and its
// connections are built here over raw descriptors wrapped in os.File, which
// gives them the runtime's poller and so deadlines.
package guestvsock

import (
	"errors"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// ErrClosed reports a listener used after Close.
var ErrClosed = errors.New("vsock listener closed")

const backlog = 16

// Listener accepts connections from the host on one vsock port.
type Listener struct {
	file *os.File
	port uint32
}

// Listen binds the port on every context id, which in a guest means the host.
func Listen(port uint32) (*Listener, error) {
	socket, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}

	if err = unix.SetNonblock(socket, true); err != nil {
		_ = unix.Close(socket)

		return nil, fmt.Errorf("vsock nonblock: %w", err)
	}

	if err = unix.Bind(socket, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		_ = unix.Close(socket)

		return nil, fmt.Errorf("vsock bind port %d: %w", port, err)
	}

	if err = unix.Listen(socket, backlog); err != nil {
		_ = unix.Close(socket)

		return nil, fmt.Errorf("vsock listen port %d: %w", port, err)
	}

	return &Listener{file: os.NewFile(uintptr(socket), fmt.Sprintf("vsock:%d", port)), port: port}, nil
}

// Accept waits for the next connection from the host.
func (l *Listener) Accept() (net.Conn, error) {
	raw, err := l.file.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("vsock accept: %w", err)
	}

	var (
		connFd int
		accErr error
	)

	err = raw.Read(func(fd uintptr) bool {
		connFd, _, accErr = unix.Accept(int(fd))
		if errors.Is(accErr, unix.EAGAIN) || errors.Is(accErr, unix.EWOULDBLOCK) {
			return false
		}

		return true
	})
	if err != nil {
		if errors.Is(err, os.ErrClosed) {
			return nil, ErrClosed
		}

		return nil, fmt.Errorf("vsock accept: %w", err)
	}

	if accErr != nil {
		return nil, fmt.Errorf("vsock accept: %w", accErr)
	}

	if err := unix.SetNonblock(connFd, true); err != nil {
		_ = unix.Close(connFd)

		return nil, fmt.Errorf("vsock accept: nonblock: %w", err)
	}

	return &conn{File: os.NewFile(uintptr(connFd), "vsock"), port: l.port}, nil
}

// Close stops accepting; Accept returns ErrClosed.
func (l *Listener) Close() error {
	if err := l.file.Close(); err != nil {
		return fmt.Errorf("vsock close: %w", err)
	}

	return nil
}

// Addr names the port.
func (l *Listener) Addr() net.Addr { return addr{port: l.port} }

type conn struct {
	*os.File

	port uint32
}

func (c *conn) LocalAddr() net.Addr { return addr{port: c.port} }
func (*conn) RemoteAddr() net.Addr  { return addr{port: 0} }

type addr struct{ port uint32 }

func (addr) Network() string  { return "vsock" }
func (a addr) String() string { return fmt.Sprintf("vsock:%d", a.port) }
