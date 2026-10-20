//go:build darwin && arm64

package install

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/forkcloser/xos"
	"github.com/mycophonic/primordium/filesystem"
	"golang.org/x/crypto/ssh"

	"github.com/farcloser/zerogravity/internal/protocol"
	"github.com/farcloser/zerogravity/pkg/agent"
	"github.com/farcloser/zerogravity/pkg/machine"
	"github.com/farcloser/zerogravity/pkg/vm"
)

// ErrAgentInstall reports the agent not reaching the guest, or not answering
// once there.
var ErrAgentInstall = errors.New("agent install")

const (
	// AgentBinary is the agent's file name beside the zerogravity binary; the
	// build writes both.
	AgentBinary = "zerogravity-agent"
	// agentPath is where the agent lives in the guest.
	agentPath = "/usr/local/libexec/zerogravity-agent"
	// plistPath is the launchd daemon that keeps it running.
	plistPath  = "/Library/LaunchDaemons/world.farcloser.zerogravity-agent.plist"
	plistLabel = "world.farcloser.zerogravity-agent"
	uploadPath = "/tmp/zerogravity-agent"

	sshTimeout     = 30 * time.Second
	knownHostsMode = 0o600
	agentHandshake = 60 * time.Second
	agentPortFlag  = "--port"
)

// plist is the launchd daemon: root, at boot, restarted if it dies, the
// agent's port on the command line so the host and the guest cannot disagree.
const plist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + plistLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + agentPath + `</string>
		<string>` + agentPortFlag + `</string>
		<string>%d</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>/var/log/zerogravity-agent.log</string>
	<key>StandardErrorPath</key>
	<string>/var/log/zerogravity-agent.log</string>
</dict>
</plist>
`

// agentBinary finds the agent beside the running zerogravity binary.
func agentBinary() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("%w: locating the agent: %w", ErrAgentInstall, err)
	}

	path := filepath.Join(filepath.Dir(self), AgentBinary)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf(
			"%w: %s must sit beside %s (`just build` writes both): %w",
			ErrAgentInstall,
			AgentBinary,
			self,
			err,
		)
	}

	return path, nil
}

// installAgent copies the agent into the guest over ssh with the generated
// password, installs it as a launchd daemon, then proves it answers over vsock
// and turns password ssh off. The password never travels again after this.
func installAgent(
	ctx context.Context, guest *vm.VM, dir *machine.Dir, addr netip.Addr, user, password string, progress io.Writer,
) (string, error) {
	binary, err := agentBinary()
	if err != nil {
		return "", err
	}

	payload, err := xos.ReadFile(binary)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrAgentInstall, err)
	}

	client, hostKey, err := dialSSH(ctx, addr, user, password)
	if err != nil {
		return "", err
	}

	// The key the guest presented on its first boot is the one every later
	// `zerogravity ssh` holds it to.
	if err = filesystem.WriteFile(
		dir.File(machine.KnownHostsFile),
		knownHostsLine(hostKey),
		knownHostsMode,
	); err != nil {
		return "", fmt.Errorf("%w: known_hosts: %w", ErrAgentInstall, err)
	}

	defer func() { _ = client.Close() }()

	fmt.Fprint(progress, "agent: uploading\n")

	if err = runSSH(client, "cat > "+uploadPath, payload, ""); err != nil {
		return "", err
	}

	// sudo reads the password from stdin (-S) once; the script after it runs
	// as root in one shell so there is one prompt, not five.
	script := strings.Join([]string{
		"set -e",
		// Stock macOS has nothing under /usr/local: the directory is made first.
		"install -d -o root -g wheel -m 0755 " + filepath.Dir(agentPath),
		"install -o root -g wheel -m 0755 " + uploadPath + " " + agentPath,
		"rm -f " + uploadPath,
		"cat > " + plistPath + " <<'PLIST'\n" + fmt.Sprintf(plist, protocol.Port) + "PLIST",
		"chown root:wheel " + plistPath,
		"chmod 0644 " + plistPath,
		"launchctl bootout system " + plistPath + " 2>/dev/null || true",
		"launchctl bootstrap system " + plistPath,
	}, "\n")

	fmt.Fprint(progress, "agent: installing the launchd daemon\n")

	if err = runSSH(client, "sudo -S -p '' /bin/sh -s", []byte(password+"\n"+script+"\n"), password); err != nil {
		return "", err
	}

	fmt.Fprint(progress, "agent: waiting for it on vsock\n")

	handshakeCtx, cancel := context.WithTimeout(ctx, agentHandshake)
	defer cancel()

	rpc, info, err := agent.Dial(handshakeCtx, func(ctx context.Context) (net.Conn, error) {
		return guest.ConnectRetry(ctx, protocol.Port, agentHandshake)
	})
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrAgentInstall, err)
	}

	defer rpc.Close()

	fmt.Fprintf(progress, "agent: %s answers; turning password ssh off\n", info.Version)

	if err = rpc.Configure(ctx, agent.Configuration{User: user, DisablePasswordSSH: true}); err != nil {
		return "", fmt.Errorf("%w: %w", ErrAgentInstall, err)
	}

	return info.Version, nil
}

// dialSSH connects with the generated password and captures the host key the
// guest presents. Nothing checks that key: this is the guest's first boot, on a
// network only this process can reach, and the key is recorded here so that
// every later connection is held to it.
func dialSSH(ctx context.Context, addr netip.Addr, user, password string) (*ssh.Client, ssh.PublicKey, error) {
	var hostKey ssh.PublicKey

	config := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			hostKey = key

			return nil
		},
		Timeout: sshTimeout,
	}

	target := net.JoinHostPort(addr.String(), sshPort)

	var dialer net.Dialer

	conn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: ssh %s: %w", ErrAgentInstall, target, err)
	}

	sshConn, channels, requests, err := ssh.NewClientConn(conn, target, config)
	if err != nil {
		_ = conn.Close()

		return nil, nil, fmt.Errorf("%w: ssh %s: %w", ErrAgentInstall, target, err)
	}

	if hostKey == nil {
		_ = conn.Close()

		return nil, nil, fmt.Errorf("%w: ssh %s: the handshake presented no host key", ErrAgentInstall, target)
	}

	return ssh.NewClient(sshConn, channels, requests), hostKey, nil
}

// knownHostsLine is the guest's key in known_hosts form, for any address: the
// guest's address changes with every boot, its key does not.
func knownHostsLine(key ssh.PublicKey) []byte {
	return []byte("* " + key.Type() + " " + base64.StdEncoding.EncodeToString(key.Marshal()) + "\n")
}

// runSSH runs one command with stdin fed whole, and reports its output on
// failure with the password, if it appears, replaced.
func runSSH(client *ssh.Client, command string, stdin []byte, secret string) error {
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("%w: ssh session: %w", ErrAgentInstall, err)
	}

	defer func() { _ = session.Close() }()

	var output bytes.Buffer

	session.Stdin = bytes.NewReader(stdin)
	session.Stdout = &output
	session.Stderr = &output

	if err := session.Run(command); err != nil {
		text := output.String()
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "<password>")
		}

		return fmt.Errorf("%w: %s: %w: %s", ErrAgentInstall, firstWord(command), err, strings.TrimSpace(text))
	}

	return nil
}

func firstWord(command string) string {
	word, _, _ := strings.Cut(command, " ")

	return word
}
