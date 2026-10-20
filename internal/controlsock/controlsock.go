// Package controlsock is how a second zerogravity process reaches a running
// guest's agent. A virtio socket belongs to the process that owns the virtual
// machine, so `zerogravity run` listens on a unix socket in the VM directory
// and relays each connection to the agent's vsock port; `exec`, `ip` and `ssh`
// dial that socket from their own process.
package controlsock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
)

// File is the socket's name in the VM directory.
const File = "control.sock"

// Dialer opens a connection to the agent, the VM's vsock dialer.
type Dialer func(ctx context.Context) (net.Conn, error)

// Serve listens on path and relays every connection through dial until ctx
// ends. A stale socket file from a previous run is replaced: the lock the
// running process holds is what says a VM is running, not this file. The
// socket itself is created with the default umask; what keeps other users
// off it is the VM directory's 0700.
func Serve(ctx context.Context, path string, dial Dialer) error {
	_ = os.Remove(path)

	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		return fmt.Errorf("control socket %s: %w", path, err)
	}

	go func() {
		<-ctx.Done()

		_ = listener.Close()
		_ = os.Remove(path)
	}()

	for {
		client, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil //nolint:nilerr // a closed listener at shutdown is the normal end
			}

			return fmt.Errorf("control socket %s: %w", path, err)
		}

		go relay(ctx, client, dial)
	}
}

// relay copies both directions and closes both ends when either side is done;
// a half-close on one side is passed on so EOF-framed exchanges work.
func relay(ctx context.Context, client net.Conn, dial Dialer) {
	defer func() { _ = client.Close() }()

	guest, err := dial(ctx)
	if err != nil {
		return
	}

	defer func() { _ = guest.Close() }()

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		_, _ = io.Copy(guest, client)

		closeWrite(guest)
	}()

	go func() {
		defer wg.Done()

		_, _ = io.Copy(client, guest)

		closeWrite(client)
	}()

	wg.Wait()
}

func closeWrite(conn net.Conn) {
	if half, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = half.CloseWrite()

		return
	}

	_ = conn.Close()
}

// Dial connects to a running VM's control socket.
func Dial(ctx context.Context, path string) (net.Conn, error) {
	var dialer net.Dialer

	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("control socket %s: %w", path, err)
	}

	return conn, nil
}
