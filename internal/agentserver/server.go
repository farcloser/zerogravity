//go:build darwin

// Package agentserver is the guest side of proto/agent.proto: Ready, Configure,
// Exec and Shutdown, as the launchd daemon serves them to the host over vsock.
// It knows nothing of the transport, so a test drives it in-process.
package agentserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"connectrpc.com/connect/v2"
	"github.com/forkcloser/xos"

	"github.com/farcloser/zerogravity/internal/agentrpc"
	"github.com/farcloser/zerogravity/internal/agentrpc/agentrpcconnect"
	"github.com/farcloser/zerogravity/internal/hostos"
	"github.com/farcloser/zerogravity/internal/protocol"
)

// ErrConfigure reports a Configure request the agent could not apply.
var ErrConfigure = errors.New("configure")

const (
	sshDirMode   = 0o700
	sshKeysMode  = 0o600
	sshdConfMode = 0o644
	sshdDirMode  = 0o755
	// Sorted before Apple's 100-macos.conf: sshd reads the directory in glob
	// order and the first setting of a key wins.
	sshdConfDir    = "/etc/ssh/sshd_config.d"
	sshdConfFile   = "00-zerogravity.conf"
	sshdConf       = "PasswordAuthentication no\nKbdInteractiveAuthentication no\n"
	sshdService    = "system/com.openssh.sshd"
	outputChunk    = 32 * 1024
	exitOnBadStart = 127
)

// Server serves proto/agent.proto for one guest: the agent process registers it
// with Connect and mounts it on the vsock listener.
type Server struct {
	Version string
}

// Ready answers who the guest is: revision, version, hostname, addresses, macOS.
func (s *Server) Ready(_ context.Context, _ *agentrpc.ReadyRequest) (*agentrpc.ReadyResponse, error) {
	hostname, _ := os.Hostname()
	macos, _ := hostos.Version()

	return &agentrpc.ReadyResponse{
		Revision:  protocol.Revision,
		Version:   s.Version,
		Hostname:  hostname,
		Addresses: ipv4Addresses(),
		Macos:     macos,
	}, nil
}

func ipv4Addresses() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	var addresses []string

	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To4() != nil {
				addresses = append(addresses, ipNet.IP.String())
			}
		}
	}

	return addresses
}

// Configure applies what the host asked for this boot: authorized keys for the
// user, the hostname, password ssh off. Each part is optional; a refusal names
// the part.
func (*Server) Configure(ctx context.Context, msg *agentrpc.ConfigureRequest) (*agentrpc.ConfigureResponse, error) {
	if msg.GetAuthorizedKeys() != "" {
		if err := writeAuthorizedKeys(msg.GetUser(), msg.GetAuthorizedKeys()); err != nil {
			return nil, failed(err)
		}
	}

	if msg.GetHostname() != "" {
		for _, key := range []string{"HostName", "LocalHostName", "ComputerName"} {
			set := exec.CommandContext(ctx, "scutil", "--set", key, msg.GetHostname()) // #nosec G204 -- the call

			out, err := set.CombinedOutput()
			if err != nil {
				return nil, failed(
					fmt.Errorf("%w: scutil --set %s: %w: %s", ErrConfigure, key, err, bytes.TrimSpace(out)),
				)
			}
		}
	}

	if msg.GetDisablePasswordSsh() {
		if err := disablePasswordSSH(ctx); err != nil {
			return nil, failed(err)
		}
	}

	return &agentrpc.ConfigureResponse{}, nil
}

// failed is a Configure refusal the host can read: the code and the text both
// travel; a plain error would arrive as "unknown".
func failed(err error) error {
	return connect.NewError(connect.CodeFailedPrecondition, err.Error()).WithCause(err)
}

// writeAuthorizedKeys replaces the account's authorized_keys, owned by the
// account with the modes sshd insists on.
func writeAuthorizedKeys(username, keys string) error {
	owner, err := lookup(username)
	if err != nil {
		return err
	}

	sshDir := filepath.Join(owner.HomeDir, ".ssh")
	if err = os.MkdirAll(sshDir, sshDirMode); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrConfigure, sshDir, err)
	}

	if err = os.Chown(sshDir, int(owner.uid), int(owner.gid)); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrConfigure, sshDir, err)
	}

	file := filepath.Join(sshDir, "authorized_keys")
	if err = xos.WriteFile(file, []byte(keys), sshKeysMode); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrConfigure, file, err)
	}

	if err = os.Chown(file, int(owner.uid), int(owner.gid)); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrConfigure, file, err)
	}

	return nil
}

// account is a guest user with its ids as the kernel wants them.
type account struct {
	*user.User

	uid uint32
	gid uint32
}

func lookup(username string) (*account, error) {
	if username == "" {
		return nil, fmt.Errorf("%w: no user named", ErrConfigure)
	}

	found, err := user.Lookup(username)
	if err != nil {
		return nil, fmt.Errorf("%w: user %q: %w", ErrConfigure, username, err)
	}

	uid, err := strconv.ParseUint(found.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("%w: user %q uid %q: %w", ErrConfigure, username, found.Uid, err)
	}

	gid, err := strconv.ParseUint(found.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("%w: user %q gid %q: %w", ErrConfigure, username, found.Gid, err)
	}

	return &account{User: found, uid: uint32(uid), gid: uint32(gid)}, nil
}

func disablePasswordSSH(ctx context.Context) error {
	if err := os.MkdirAll(sshdConfDir, sshdDirMode); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrConfigure, sshdConfDir, err)
	}

	file := filepath.Join(sshdConfDir, sshdConfFile)
	if err := xos.WriteFile(file, []byte(sshdConf), sshdConfMode); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrConfigure, file, err)
	}

	out, err := exec.CommandContext(ctx, "launchctl", "kickstart", "-k", sshdService).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: restarting sshd: %w: %s", ErrConfigure, err, bytes.TrimSpace(out))
	}

	return nil
}

// Exec runs the command, streams its output as it comes, and ends with the
// exit status. A command that cannot start reports exit 127 with the reason
// on stderr, the way a shell would. Run as a user, the command gets that
// user's uid and gid and nothing more: no supplementary groups, so no admin
// group and no sudo; what needs root runs as the agent, which is root.
func (*Server) Exec(
	ctx context.Context,
	req *agentrpc.ExecRequest,
	stream agentrpcconnect.AgentExecServerStream,
) error {
	argv := req.GetArgv()
	if len(argv) == 0 {
		return connect.NewError(connect.CodeInvalidArgument, "exec: empty argv")
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204 -- the call

	cmd.Env = append(os.Environ(), req.GetEnv()...)
	cmd.Stdin = bytes.NewReader(req.GetStdin())
	cmd.Stdout = &streamWriter{stream: stream, stdout: true}
	cmd.Stderr = &streamWriter{stream: stream, stdout: false}

	if username := req.GetUser(); username != "" {
		runAs, err := lookup(username)
		if err != nil {
			return connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
		}

		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: runAs.uid, Gid: runAs.gid}}
		cmd.Dir = runAs.HomeDir
		cmd.Env = append(cmd.Env, "HOME="+runAs.HomeDir, "USER="+runAs.Username)
	}

	exit := 0

	if err := cmd.Run(); err != nil {
		if exitErr, isExit := errors.AsType[*exec.ExitError](err); isExit {
			exit = exitErr.ExitCode()
		} else {
			exit = exitOnBadStart
			_ = stream.Send(
				&agentrpc.ExecResponse{Event: &agentrpc.ExecResponse_Stderr{Stderr: []byte(err.Error() + "\n")}},
			)
		}
	}

	if err := stream.Send(
		&agentrpc.ExecResponse{Event: &agentrpc.ExecResponse_Exit{Exit: int32(exit)}},
	); err != nil { // #nosec G115 -- an exit status is one byte
		return fmt.Errorf("exec: sending exit: %w", err)
	}

	return nil
}

// streamWriter turns the command's output into stream messages, in chunks the
// stream handles comfortably.
type streamWriter struct {
	stream agentrpcconnect.AgentExecServerStream
	stdout bool
}

func (w *streamWriter) Write(data []byte) (int, error) {
	for offset := 0; offset < len(data); offset += outputChunk {
		end := min(offset+outputChunk, len(data))
		chunk := bytes.Clone(data[offset:end])

		var event *agentrpc.ExecResponse
		if w.stdout {
			event = &agentrpc.ExecResponse{Event: &agentrpc.ExecResponse_Stdout{Stdout: chunk}}
		} else {
			event = &agentrpc.ExecResponse{Event: &agentrpc.ExecResponse_Stderr{Stderr: chunk}}
		}

		if err := w.stream.Send(event); err != nil {
			return offset, fmt.Errorf("exec: sending output: %w", err)
		}
	}

	return len(data), nil
}

// Shutdown asks macOS to shut down now; the host sees the machine stop.
func (*Server) Shutdown(ctx context.Context, _ *agentrpc.ShutdownRequest) (*agentrpc.ShutdownResponse, error) {
	out, err := exec.CommandContext(ctx, "shutdown", "-h", "now").CombinedOutput()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Sprintf("shutdown: %v: %s", err, bytes.TrimSpace(out))).
			WithCause(err)
	}

	return &agentrpc.ShutdownResponse{}, nil
}
