//go:build darwin && arm64

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/mycophonic/primordium/human"

	"github.com/farcloser/zerogravity/internal/controlsock"
	"github.com/farcloser/zerogravity/internal/lease"
	"github.com/farcloser/zerogravity/internal/paths"
	"github.com/farcloser/zerogravity/internal/protocol"
	"github.com/farcloser/zerogravity/pkg/agent"
	"github.com/farcloser/zerogravity/pkg/install"
	"github.com/farcloser/zerogravity/pkg/machine"
	"github.com/farcloser/zerogravity/pkg/vm"
)

// ErrNotRunning reports a VM no zerogravity process holds.
var ErrNotRunning = errors.New("vm is not running")

const (
	stopGrace   = 2 * time.Minute
	agentWait   = 2 * time.Minute
	shareOptRO  = "ro"
	shareOptSep = ":"
)

// project resolves the project root the commands work in.
func project(root *CLI) (string, error) {
	return paths.Project(root.Project)
}

// vmDir resolves a named VM's directory under the project.
func vmDir(root *CLI, name string) (string, error) {
	dir, err := project(root)
	if err != nil {
		return "", err
	}

	return paths.VM(dir, name)
}

func openVM(root *CLI, name string) (*machine.Dir, error) {
	path, err := vmDir(root, name)
	if err != nil {
		return nil, err
	}

	return machine.Open(path)
}

type installCmd struct {
	Name      string `arg:""                                                                 help:"name of the base to create"`
	IPSW      string `default:"latest"                                                       help:"restore image: a path, or latest"     name:"ipsw"`
	CPUs      uint   `default:"4"                                                            help:"guest cpus"                           name:"cpu"`
	Memory    string `default:"8GiB"                                                         help:"guest memory"                         name:"memory"`
	Disk      string `default:"64GiB"                                                        help:"disk image size"                      name:"disk"`
	User      string `default:"zg"                                                           help:"guest account the first boot creates" name:"user"`
	FullName  string `default:"zerogravity"                                                  help:"that account's full name"             name:"full-name"`
	AutoLogin bool   `help:"log the account in at startup (for a base meant for --gui work)" name:"autologin"`
}

// Run installs a base into this project, unattended: DESIGN.md §5.
func (c installCmd) Run(ctx context.Context, root *CLI) error {
	dir, err := vmDir(root, c.Name)
	if err != nil {
		return err
	}

	cache, err := paths.Cache()
	if err != nil {
		return err
	}

	memory, err := parseSize(c.Memory)
	if err != nil {
		return err
	}

	disk, err := parseSize(c.Disk)
	if err != nil {
		return err
	}

	return install.Install(ctx, cache, dir, install.Options{
		IPSW:      c.IPSW,
		CPUs:      c.CPUs,
		Memory:    memory,
		Disk:      disk,
		Display:   machine.DefaultDisplay(),
		User:      c.User,
		FullName:  c.FullName,
		AutoLogin: c.AutoLogin,
		Progress:  os.Stderr,
	})
}

func parseSize(value string) (uint64, error) {
	size, err := human.ParseSize(value)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", value, err)
	}

	if size <= 0 {
		return 0, fmt.Errorf("size %q: %w", value, machine.ErrConfig)
	}

	return uint64(size), nil
}

type cloneCmd struct {
	Base string `arg:"" help:"base to clone: a VM of this project"`
	Name string `arg:"" help:"name of the new VM"`
}

// Run clones one VM of the project into another. Cloning a base from the host
// cache comes with the cache (DESIGN.md §3); cloning within the project is the
// same operation on a different source.
func (c cloneCmd) Run(_ context.Context, root *CLI) error {
	src, err := openVM(root, c.Base)
	if err != nil {
		return err
	}

	dstPath, err := vmDir(root, c.Name)
	if err != nil {
		return err
	}

	config, err := src.ReadConfig()
	if err != nil {
		return err
	}

	base := config.Base
	if base == "" {
		base = c.Base
	}

	if _, err = machine.Clone(src, dstPath, base); err != nil {
		return err
	}

	if _, err = fmt.Fprintf(os.Stdout, "%s: cloned from %s\n", c.Name, c.Base); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}

	return nil
}

type runCmd struct {
	Name   string   `arg:""                                                              help:"VM to boot"`
	GUI    bool     `help:"open a window on the VM"                                      name:"gui"`
	Shares []string `help:"directory to share, dir[:ro], under /Volumes/My Shared Files" name:"share"                       sep:"none"`
	SSHKey string   `help:"public key to install for the guest user at boot"             name:"ssh-key"                     type:"existingfile"`
	Args   []string `arg:""                                                              help:"command to run in the guest" optional:""`
}

// Run boots a VM and holds it until the guest halts, the window closes, or a
// stop arrives (a signal, or `zerogravity stop`). The ssh key at boot goes to
// the agent; a command in the guest is `exec` against the running VM.
func (c runCmd) Run(ctx context.Context, root *CLI) error {
	if len(c.Args) > 0 {
		return fmt.Errorf("%w: a command on `run` (use `exec` against the running VM)", ErrNotImplemented)
	}

	authorizedKeys, err := readOptional(c.SSHKey)
	if err != nil {
		return err
	}

	dir, err := openVM(root, c.Name)
	if err != nil {
		return err
	}

	config, err := dir.ReadConfig()
	if err != nil {
		return err
	}

	if config.Agent == "" && authorizedKeys != nil {
		return fmt.Errorf(
			"%w: --ssh-key needs the agent, and this base has none (install writes it)",
			machine.ErrConfig,
		)
	}

	shares, err := parseShares(c.Shares)
	if err != nil {
		return err
	}

	lock, err := dir.Lock()
	if err != nil {
		return err
	}

	defer func() { _ = machine.Unlock(lock) }()

	guest, err := vm.New(vm.Options{Dir: dir, Config: config, Shares: shares})
	if err != nil {
		return err
	}

	defer guest.Close()

	if err = guest.Start(ctx); err != nil {
		return err
	}

	return c.serve(ctx, dir, config, guest, authorizedKeys)
}

// serve holds the running guest: the control socket for other processes, the
// agent's configuration for this boot, then the window or the wait.
func (c runCmd) serve(
	ctx context.Context,
	dir *machine.Dir,
	config *machine.Config,
	guest *vm.VM,
	authorizedKeys []byte,
) error {
	dial := func(ctx context.Context) (net.Conn, error) {
		return guest.ConnectRetry(ctx, protocol.Port, agentWait)
	}

	// The control socket lets exec, ip and ssh reach the agent from their own
	// process; the vsock belongs to this one.
	controlCtx, stopControl := context.WithCancel(ctx)
	defer stopControl()

	go func() {
		if err := controlsock.Serve(controlCtx, dir.File(controlsock.File), dial); err != nil {
			slog.ErrorContext(controlCtx, "control socket", slog.Any("error", err))
		}
	}()

	if config.Agent != "" {
		if err := configureGuest(ctx, dial, config.User, c.Name, authorizedKeys); err != nil {
			_ = guest.Shutdown(context.WithoutCancel(ctx), stopGrace)

			return err
		}
	}

	// With a window, closing the window is the stop: the AppKit loop holds the
	// main thread until then, and a signal or `zerogravity stop` waits for it.
	if c.GUI {
		if err := guest.Window(c.Name); err != nil {
			return err
		}

		return guest.Shutdown(context.WithoutCancel(ctx), stopGrace)
	}

	// The guest halts on its own, or a stop arrives: a signal, which cancels
	// ctx, or `zerogravity stop`, which sends the same signal. Either way the
	// guest gets its grace to shut down cleanly before this process exits 0.
	if err := guest.Wait(ctx); err != nil {
		if ctx.Err() == nil {
			return err
		}

		return guest.Shutdown(context.WithoutCancel(ctx), stopGrace)
	}

	return nil
}

// readOptional reads a file when a path was given.
func readOptional(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path) // #nosec G304 -- the user's own path from the command line
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	return data, nil
}

// parseShares reads dir[:ro] into shares named after the directory.
func parseShares(specs []string) ([]vm.Share, error) {
	shares := make([]vm.Share, 0, len(specs))

	for _, spec := range specs {
		dir, option, _ := strings.Cut(spec, shareOptSep)

		if option != "" && option != shareOptRO {
			return nil, fmt.Errorf("%w: share %q: the only option is %q", machine.ErrConfig, spec, shareOptRO)
		}

		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("share %q: %w", spec, err)
		}

		shares = append(shares, vm.Share{Name: filepath.Base(abs), Dir: abs, ReadOnly: option == shareOptRO})
	}

	return shares, nil
}

type stopCmd struct {
	Name string `arg:"" help:"VM to stop"`
}

// Run signals the zerogravity process holding the VM, which asks the guest to
// shut down.
func (c stopCmd) Run(_ context.Context, root *CLI) error {
	dir, err := openVM(root, c.Name)
	if err != nil {
		return err
	}

	running, err := dir.Running()
	if err != nil {
		return err
	}

	if !running {
		return fmt.Errorf("%w: %s", ErrNotRunning, c.Name)
	}

	pid, err := dir.RunningPID()
	if err != nil {
		// The lock is taken before the pid is written: a run that just started
		// is between the two.
		return fmt.Errorf("%w: %s is starting; try again", machine.ErrRunning, c.Name)
	}

	if err = syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("stopping %s (pid %d): %w", c.Name, pid, err)
	}

	return nil
}

type ipCmd struct {
	Name string `arg:"" help:"VM to query"`
}

// Run prints the address the guest took on its network: from the agent when
// the VM runs one, from the host's lease file otherwise.
func (c ipCmd) Run(ctx context.Context, root *CLI) error {
	dir, config, err := runningVM(root, c.Name)
	if err != nil {
		return err
	}

	if config.Agent != "" {
		addr, agentErr := guestAddress(ctx, dir)
		if agentErr == nil {
			if _, err = fmt.Fprintln(os.Stdout, addr); err != nil {
				return fmt.Errorf("writing output: %w", err)
			}

			return nil
		}
	}

	mac, err := net.ParseMAC(config.MAC)
	if err != nil {
		return fmt.Errorf("%w: mac %q: %w", machine.ErrConfig, config.MAC, err)
	}

	leases, err := lease.Read()
	if err != nil {
		return err
	}

	addr, err := lease.Lookup(leases, mac)
	if err != nil {
		return err
	}

	if _, err = fmt.Fprintln(os.Stdout, addr); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}

	return nil
}

type execCmd struct {
	Name string   `arg:"" help:"VM to run in"`
	Args []string `arg:"" help:"command"`
}

// Run runs a command in the running VM through its agent, as the guest user,
// and exits with the command's status.
func (c execCmd) Run(ctx context.Context, root *CLI) error {
	dir, config, err := runningVM(root, c.Name)
	if err != nil {
		return err
	}

	rpc, _, err := agent.Dial(ctx, func(ctx context.Context) (net.Conn, error) {
		return controlsock.Dial(ctx, dir.File(controlsock.File))
	})
	if err != nil {
		return err
	}

	defer rpc.Close()

	exit, err := rpc.Exec(ctx, agent.ExecRequest{Argv: c.Args, User: config.User}, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}

	if exit != 0 {
		return exitStatusError(exit)
	}

	return nil
}

// exitStatusError is a command's status to exit this process with, carried as
// an error so kong's Run can return it.
type exitStatusError int

func (e exitStatusError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// runningVM opens a VM that another zerogravity process holds.
func runningVM(root *CLI, name string) (*machine.Dir, *machine.Config, error) {
	dir, err := openVM(root, name)
	if err != nil {
		return nil, nil, err
	}

	config, err := dir.ReadConfig()
	if err != nil {
		return nil, nil, err
	}

	running, err := dir.Running()
	if err != nil {
		return nil, nil, err
	}

	if !running {
		return nil, nil, fmt.Errorf("%w: %s", ErrNotRunning, name)
	}

	return dir, config, nil
}

// configureGuest hands the agent what this run was given: the hostname and
// the ssh key for the guest user.
func configureGuest(ctx context.Context, dial agent.Dialer, user, hostname string, authorizedKeys []byte) error {
	rpc, _, err := agent.Dial(ctx, dial)
	if err != nil {
		return err
	}

	defer rpc.Close()

	return rpc.Configure(ctx, agent.Configuration{
		User:           user,
		AuthorizedKeys: string(authorizedKeys),
		Hostname:       hostname,
	})
}

type sshCmd struct {
	Name string   `arg:"" help:"VM to connect to"`
	Args []string `arg:"" help:"arguments for ssh" optional:""`
}

// Run execs ssh to the running VM as the guest user, the address from the
// agent, the host key pinned to the VM directory's known_hosts.
func (c sshCmd) Run(ctx context.Context, root *CLI) error {
	dir, config, err := runningVM(root, c.Name)
	if err != nil {
		return err
	}

	addr, err := guestAddress(ctx, dir)
	if err != nil {
		return err
	}

	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh: %w", err)
	}

	args := append([]string{
		"ssh",
		"-o", "UserKnownHostsFile=" + dir.File(machine.KnownHostsFile),
		"-o", "StrictHostKeyChecking=yes",
		"-o", "PasswordAuthentication=no",
		config.User + "@" + addr,
	}, c.Args...)

	env := os.Environ()

	execErr := syscall.Exec(sshPath, args, env) // #nosec G204 -- our own ssh invocation
	if execErr != nil {
		return fmt.Errorf("ssh: %w", execErr)
	}

	return nil
}

// guestAddress asks the agent, which needs no lease: the first IPv4 address
// it reports is the one on the VM's network.
func guestAddress(ctx context.Context, dir *machine.Dir) (string, error) {
	rpc, info, err := agent.Dial(ctx, func(ctx context.Context) (net.Conn, error) {
		return controlsock.Dial(ctx, dir.File(controlsock.File))
	})
	if err != nil {
		return "", err
	}

	defer rpc.Close()

	if len(info.Addresses) == 0 {
		return "", fmt.Errorf("%w: the guest reports no address yet", agent.ErrAgent)
	}

	return info.Addresses[0], nil
}

type listCmd struct{}

// Run lists the project's VMs with what config.json says about them.
func (listCmd) Run(_ context.Context, root *CLI) error {
	dir, err := project(root)
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(paths.VMs(dir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("listing %s: %w", paths.VMs(dir), err)
	}

	names := make([]string, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}

	sort.Strings(names)

	table := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "NAME\tCPUS\tMEMORY\tDISK\tMACOS\tBASE\tSTATE")

	for _, name := range names {
		_, _ = fmt.Fprintln(table, describe(filepath.Join(paths.VMs(dir), name), name))
	}

	if err = table.Flush(); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}

	return nil
}

// describe is one row of `ls`: the VM's config and whether a zerogravity
// process holds it. A VM whose config does not read is a row with the error,
// so a broken VM is visible rather than hidden.
func describe(path, name string) string {
	machineDir, err := machine.Open(path)
	if err != nil {
		return fmt.Sprintf("%s\t\t\t\t\t\t%v", name, err)
	}

	config, err := machineDir.ReadConfig()
	if err != nil {
		return fmt.Sprintf("%s\t\t\t\t\t\t%v", name, err)
	}

	state := "stopped"

	if running, probeErr := machineDir.Running(); probeErr == nil && running {
		state = "running"
	}

	return fmt.Sprintf("%s\t%d\t%s\t%s\t%s\t%s\t%s",
		name, config.CPUs, human.BinarySize(float64(config.Memory)),
		human.BinarySize(float64(config.Disk)), config.MacOS, config.Base, state)
}

type removeCmd struct {
	Name string `arg:"" help:"VM to remove"`
}

func (c removeCmd) Run(_ context.Context, root *CLI) error {
	path, err := vmDir(root, c.Name)
	if err != nil {
		return err
	}

	return machine.Remove(path)
}
