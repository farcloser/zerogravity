//go:build darwin && arm64

// Command zerogravity installs, clones and boots macOS virtual machines inside
// a project: no daemon, nothing system-wide. See DESIGN.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/alecthomas/kong"
	"github.com/mycophonic/primordium/app/logger"
)

const appName = "zerogravity"

// version is set by the release build.
var version = "0.0.1-dev"

// CLI is the command tree.
type CLI struct {
	Project string `env:"ZEROGRAVITY_PROJECT" help:"project directory (default: the working directory)" name:"project" type:"path"`

	Install installCmd `cmd:"" help:"install a macOS base from Apple's restore image, unattended"`
	Clone   cloneCmd   `cmd:"" help:"clone a base into this project"`
	Run     runCmd     `cmd:"" help:"boot a VM, headless or with a window"`
	Stop    stopCmd    `cmd:"" help:"stop a running VM"`
	IP      ipCmd      `cmd:"" help:"print a running VM's address"                                name:"ip"`
	Exec    execCmd    `cmd:"" help:"run a command in a VM through the agent"`
	SSH     sshCmd     `cmd:"" help:"ssh into a running VM as the guest user"                     name:"ssh"`
	List    listCmd    `cmd:"" help:"list this project's VMs"                                     name:"ls"`
	Remove  removeCmd  `cmd:"" help:"remove a VM that is not running"                             name:"rm"`
	Version versionCmd `cmd:"" help:"print version"`
}

func main() {
	os.Exit(run())
}

// run is main without the exit, so that the deferred signal reset runs.
func run() int {
	// A signal cancels the context and nothing more: `run` turns it into the
	// guest's clean shutdown with its own grace, then returns, so a stopped VM
	// exits 0 and a guest is never torn down mid-shutdown by a deadline here.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.SetDefaultsForLogger(ctx)

	var root CLI

	kctx := kong.Parse(&root,
		kong.Name(appName),
		kong.Description("Install, clone and boot macOS virtual machines inside a project."),
		kong.UsageOnError(),
		kong.Vars{"version": version},
		kong.BindTo(ctx, (*context.Context)(nil)),
	)

	if err := kctx.Run(&root); err != nil {
		if status, isStatus := errors.AsType[exitStatusError](err); isStatus {
			return int(status)
		}

		slog.ErrorContext(ctx, "fatal", slog.Any("error", err))

		return 1
	}

	return 0
}

// ErrNotImplemented is what a command returns while its design is written and
// its code is not. Every such command names the design section it waits on.
var ErrNotImplemented = errors.New("not implemented yet")

type versionCmd struct{}

func (versionCmd) Run() error {
	if _, err := fmt.Fprintln(os.Stdout, appName+" "+version); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}

	return nil
}
