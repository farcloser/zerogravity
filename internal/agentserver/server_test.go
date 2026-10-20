//go:build darwin

package agentserver_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"

	"github.com/farcloser/zerogravity/internal/agentrpc"
	"github.com/farcloser/zerogravity/internal/agentrpc/agentrpcconnect"
	"github.com/farcloser/zerogravity/internal/agentserver"
	"github.com/farcloser/zerogravity/internal/protocol"
)

// client drives the server in-process: the same handlers the guest serves over
// vsock, with no transport in between.
func client(t *testing.T) agentrpcconnect.AgentClient {
	t.Helper()

	server := connect.NewServer()
	agentrpcconnect.RegisterAgentHandler(server, &agentserver.Server{Version: "test"})

	return agentrpcconnect.NewAgentClient(connect.NewClient(connectinprocess.New(server)))
}

func TestReadyReportsTheRevision(t *testing.T) {
	t.Parallel()

	resp, err := client(t).Ready(t.Context(), &agentrpc.ReadyRequest{})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}

	if resp.GetRevision() != protocol.Revision {
		t.Fatalf("Revision = %q, want %q", resp.GetRevision(), protocol.Revision)
	}

	if resp.GetVersion() != "test" || resp.GetHostname() == "" || resp.GetMacos() == "" {
		t.Fatalf("Ready = %+v, want version, hostname and macos filled", resp)
	}
}

func TestExecStreamsOutputThenExit(t *testing.T) {
	t.Parallel()

	stream, err := client(t).Exec(t.Context(), &agentrpc.ExecRequest{
		Argv:  []string{"/bin/sh", "-c", "cat; echo out; echo err 1>&2; exit 3"},
		Stdin: []byte("in\n"),
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	defer func() { _ = stream.Close() }()

	var (
		stdout, stderr bytes.Buffer
		exit           int32 = -1
		exits          int
	)

	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("Receive: %v", err)
		}

		switch event := msg.GetEvent().(type) {
		case *agentrpc.ExecResponse_Stdout:
			if event != nil {
				stdout.Write(event.Stdout)
			}
		case *agentrpc.ExecResponse_Stderr:
			if event != nil {
				stderr.Write(event.Stderr)
			}
		case *agentrpc.ExecResponse_Exit:
			if event != nil {
				exit = event.Exit
				exits++
			}
		}
	}

	if stdout.String() != "in\nout\n" {
		t.Fatalf("stdout = %q, want stdin echoed then out", stdout.String())
	}

	if stderr.String() != "err\n" {
		t.Fatalf("stderr = %q, want err", stderr.String())
	}

	if exit != 3 || exits != 1 {
		t.Fatalf("exit = %d sent %d times, want 3 once", exit, exits)
	}
}

func TestExecRefusesEmptyArgv(t *testing.T) {
	t.Parallel()

	stream, err := client(t).Exec(t.Context(), &agentrpc.ExecRequest{})
	if err == nil {
		_, err = stream.Receive()
		_ = stream.Close()
	}

	if err == nil {
		t.Fatal("Exec with no argv succeeded")
	}

	if coded, ok := errors.AsType[*connect.Error](err); !ok || coded.Code() != connect.CodeInvalidArgument {
		t.Fatalf("Exec with no argv = %v, want CodeInvalidArgument", err)
	}
}

func TestExecReportsAnUnstartableCommand(t *testing.T) {
	t.Parallel()

	stream, err := client(t).Exec(t.Context(), &agentrpc.ExecRequest{Argv: []string{"/nonexistent/zerogravity-test"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	defer func() { _ = stream.Close() }()

	exit := int32(-1)

	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("Receive: %v", err)
		}

		if event, ok := msg.GetEvent().(*agentrpc.ExecResponse_Exit); ok && event != nil {
			exit = event.Exit
		}
	}

	if exit != 127 {
		t.Fatalf("exit = %d, want 127 for a command that cannot start", exit)
	}
}
