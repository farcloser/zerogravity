package agent_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/farcloser/zerogravity/internal/agentrpc"
	"github.com/farcloser/zerogravity/internal/agentrpc/agentrpcconnect"
	"github.com/farcloser/zerogravity/internal/protocol"
	"github.com/farcloser/zerogravity/pkg/agent"
)

// fakeAgent answers Ready with whatever revision it is given.
type fakeAgent struct {
	agentrpcconnect.UnimplementedAgentHandler

	revision string
}

func (f fakeAgent) Ready(context.Context, *agentrpc.ReadyRequest) (*agentrpc.ReadyResponse, error) {
	return &agentrpc.ReadyResponse{Revision: f.revision, Version: "fake", Hostname: "guest"}, nil
}

// dialer serves the fake over plain TCP in place of the guest's vsock: the
// client only ever sees a net.Conn.
func dialer(t *testing.T, revision string) agent.Dialer {
	t.Helper()

	rpc := connect.NewServer()
	agentrpcconnect.RegisterAgentHandler(rpc, fakeAgent{revision: revision})

	mux := http.NewServeMux()
	connecthttp.Mount(mux, rpc)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return func(ctx context.Context) (net.Conn, error) {
		var nd net.Dialer

		return nd.DialContext(ctx, "tcp", strings.TrimPrefix(server.URL, "http://"))
	}
}

func TestDialRefusesAnotherRevision(t *testing.T) {
	t.Parallel()

	_, _, err := agent.Dial(t.Context(), dialer(t, "not-"+protocol.Revision))
	if !errors.Is(err, agent.ErrAgent) {
		t.Fatalf("Dial against another revision = %v, want ErrAgent", err)
	}

	if !strings.Contains(err.Error(), "not-"+protocol.Revision) || !strings.Contains(err.Error(), protocol.Revision) {
		t.Fatalf("the error names neither revision: %v", err)
	}
}

func TestDialAcceptsTheRevision(t *testing.T) {
	t.Parallel()

	client, info, err := agent.Dial(t.Context(), dialer(t, protocol.Revision))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	defer client.Close()

	if info.Version != "fake" || info.Hostname != "guest" {
		t.Fatalf("Info = %+v, want the fake's answers", info)
	}
}
