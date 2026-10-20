// Package agent is the host's typed client for the zerogravity agent in the
// guest, over Connect on a virtio socket: the dialer is the VM's, the wire is
// proto/agent.proto, and the revision is checked on the first call.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/farcloser/zerogravity/internal/agentrpc"
	"github.com/farcloser/zerogravity/internal/agentrpc/agentrpcconnect"
	"github.com/farcloser/zerogravity/internal/protocol"
)

// ErrAgent reports a guest agent that does not speak this host's protocol.
var ErrAgent = errors.New("guest agent")

// baseURL is inert: the transport's dialer ignores the address and hands back
// a vsock connection, but net/http wants a syntactically valid URL.
//
//revive:disable-next-line:unsecure-url-scheme
const baseURL = "http://vsock"

// Dialer opens a connection to the agent's vsock port.
type Dialer func(ctx context.Context) (net.Conn, error)

// Info is what Ready answers.
type Info struct {
	Version   string
	Hostname  string
	Addresses []string
	MacOS     string
}

// Configuration is what Configure applies.
type Configuration struct {
	User               string
	AuthorizedKeys     string
	Hostname           string
	DisablePasswordSSH bool
}

// Client talks to one guest's agent.
type Client struct {
	rpc  agentrpcconnect.AgentClient
	http *http.Client
}

// Dial connects through dial and checks the agent's revision with Ready.
func Dial(ctx context.Context, dial Dialer) (*Client, *Info, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx)
		},
		Proxy:              nil,
		ForceAttemptHTTP2:  false,
		DisableCompression: true,
	}

	httpClient := &http.Client{Transport: transport}
	client := &Client{
		rpc:  agentrpcconnect.NewAgentClient(connect.NewClient(connecthttp.NewTransport(httpClient, baseURL))),
		http: httpClient,
	}

	info, err := client.Ready(ctx)
	if err != nil {
		httpClient.CloseIdleConnections()

		return nil, nil, err
	}

	return client, info, nil
}

// Close drops the idle connections.
func (c *Client) Close() {
	c.http.CloseIdleConnections()
}

// Ready asks the agent who the guest is, and refuses another revision.
func (c *Client) Ready(ctx context.Context) (*Info, error) {
	resp, err := c.rpc.Ready(ctx, &agentrpc.ReadyRequest{})
	if err != nil {
		return nil, fmt.Errorf("%w: ready: %w", ErrAgent, err)
	}

	if resp.GetRevision() != protocol.Revision {
		return nil, fmt.Errorf("%w: speaks revision %q, this host requires %q; reinstall the agent",
			ErrAgent, resp.GetRevision(), protocol.Revision)
	}

	return &Info{
		Version:   resp.GetVersion(),
		Hostname:  resp.GetHostname(),
		Addresses: resp.GetAddresses(),
		MacOS:     resp.GetMacos(),
	}, nil
}

// Configure applies the configuration in the guest.
func (c *Client) Configure(ctx context.Context, configuration Configuration) error {
	_, err := c.rpc.Configure(ctx, &agentrpc.ConfigureRequest{
		User:               configuration.User,
		AuthorizedKeys:     configuration.AuthorizedKeys,
		Hostname:           configuration.Hostname,
		DisablePasswordSsh: configuration.DisablePasswordSSH,
	})
	if err != nil {
		return fmt.Errorf("%w: configure: %w", ErrAgent, err)
	}

	return nil
}

// ExecRequest is one command to run in the guest.
type ExecRequest struct {
	// Argv is the command; Argv[0] is resolved on the guest's PATH.
	Argv []string
	// Env is added to the command's environment, KEY=VALUE.
	Env []string
	// Stdin is handed to the command whole.
	Stdin []byte
	// User runs the command as that account; empty runs it as the agent, root.
	User string
}

// Exec runs the request in the guest, copying output as it comes, and returns
// the exit status.
func (c *Client) Exec(ctx context.Context, req ExecRequest, stdout, stderr io.Writer) (int, error) {
	stream, err := c.rpc.Exec(
		ctx,
		&agentrpc.ExecRequest{Argv: req.Argv, Env: req.Env, Stdin: req.Stdin, User: req.User},
	)
	if err != nil {
		return 0, fmt.Errorf("%w: exec: %w", ErrAgent, err)
	}

	defer func() { _ = stream.Close() }()

	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("%w: exec: the stream ended without an exit status", ErrAgent)
		}

		if err != nil {
			return 0, fmt.Errorf("%w: exec: %w", ErrAgent, err)
		}

		exit, done, err := deliver(msg, stdout, stderr)
		if err != nil || done {
			return exit, err
		}
	}
}

// deliver writes one stream message to its destination; done is true on the
// exit message, which comes last.
func deliver(msg *agentrpc.ExecResponse, stdout, stderr io.Writer) (int, bool, error) {
	switch event := msg.GetEvent().(type) {
	case *agentrpc.ExecResponse_Stdout:
		if event != nil {
			if _, err := stdout.Write(event.Stdout); err != nil {
				return 0, false, fmt.Errorf("exec: writing stdout: %w", err)
			}
		}
	case *agentrpc.ExecResponse_Stderr:
		if event != nil {
			if _, err := stderr.Write(event.Stderr); err != nil {
				return 0, false, fmt.Errorf("exec: writing stderr: %w", err)
			}
		}
	case *agentrpc.ExecResponse_Exit:
		if event != nil {
			return int(event.Exit), true, nil
		}
	}

	return 0, false, nil
}

// Shutdown asks the guest to shut down.
func (c *Client) Shutdown(ctx context.Context) error {
	if _, err := c.rpc.Shutdown(ctx, &agentrpc.ShutdownRequest{}); err != nil {
		return fmt.Errorf("%w: shutdown: %w", ErrAgent, err)
	}

	return nil
}
