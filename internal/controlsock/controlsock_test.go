package controlsock_test

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/farcloser/zerogravity/internal/controlsock"
)

// TestRelay stands an echo server in for the guest's agent: bytes written into
// the control socket come back through the relay, and closing the client's
// write side ends the exchange.
func TestRelay(t *testing.T) {
	t.Parallel()

	echo, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	defer func() { _ = echo.Close() }()

	go func() {
		for {
			conn, acceptErr := echo.Accept()
			if acceptErr != nil {
				return
			}

			go func() {
				_, _ = io.Copy(conn, conn)
				_ = conn.Close()
			}()
		}
	}()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// A stale socket file from a previous run sits at the path; Serve replaces it.
	path := filepath.Join(t.TempDir(), controlsock.File)
	if err = os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	dial := func(ctx context.Context) (net.Conn, error) {
		var dialer net.Dialer

		return dialer.DialContext(ctx, "tcp", echo.Addr().String())
	}

	served := make(chan error, 1)

	go func() { served <- controlsock.Serve(ctx, path, dial) }()

	deadline := time.Now().Add(5 * time.Second)

	var client net.Conn

	for {
		client, err = controlsock.Dial(ctx, path)
		if err == nil || time.Now().After(deadline) {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	// A wrong relay must fail, not hang the package until its timeout.
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err = client.Write([]byte("ping")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if half, ok := client.(interface{ CloseWrite() error }); ok {
		_ = half.CloseWrite()
	}

	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if string(got) != "ping" {
		t.Fatalf("relayed %q, want ping", got)
	}

	_ = client.Close()

	cancel()

	if err := <-served; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}
