// Command zerogravity installs, clones and boots macOS virtual machines inside
// a project: no daemon, nothing system-wide. See DESIGN.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/alecthomas/kong"
	"github.com/mycophonic/primordium/app/logger"
	"github.com/mycophonic/primordium/app/shutdown"
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
	ctx := shutdown.SetDefaults(context.Background())
	logger.SetDefaultsForLogger(ctx)

	var root CLI

	kctx := kong.Parse(&root,
		kong.Name(appName),
		kong.Description("Install, clone and boot macOS virtual machines inside a project."),
		kong.UsageOnError(),
		kong.Vars{"version": version},
	)

	shutdown.Run(ctx, func(ctx context.Context) error {
		return kctx.Run(ctx, &root)
	})
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
