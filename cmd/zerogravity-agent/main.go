//go:build darwin

// Command zerogravity-agent runs inside a macOS guest as a launchd daemon and
// serves the four calls of proto/agent.proto to the host over a virtio socket:
// Ready, Configure, Exec, Shutdown. Nothing else; the share does files, ssh
// does the rest.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/farcloser/zerogravity/internal/agentrpc/agentrpcconnect"
	"github.com/farcloser/zerogravity/internal/agentserver"
	"github.com/farcloser/zerogravity/internal/guestvsock"
	"github.com/farcloser/zerogravity/internal/protocol"
)

// version is set by the release build.
var version = "0.0.1-dev"

const (
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 5 * time.Second
)

func main() {
	port := flag.Uint("port", uint(protocol.Port), "vsock port to serve on")
	showVersion := flag.Bool("version", false, "print version and exit")

	flag.Parse()

	if *showVersion {
		fmt.Fprintln(os.Stdout, "zerogravity-agent "+version)

		return
	}

	if err := run(uint32(*port)); err != nil { // #nosec G115 -- a vsock port fits
		slog.Error("agent", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(port uint32) error {
	listener, err := guestvsock.Listen(port)
	if err != nil {
		return err
	}

	rpc := connect.NewServer()
	agentrpcconnect.RegisterAgentHandler(rpc, &agentserver.Server{Version: version})

	mux := http.NewServeMux()
	connecthttp.Mount(mux, rpc)

	httpServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	errCh := make(chan error, 1)

	go func() { errCh <- httpServer.Serve(listener) }()

	slog.Info("agent serving", slog.Uint64("port", uint64(port)), slog.String("revision", protocol.Revision))

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutting down: %w", err)
		}

		return nil
	}
}
