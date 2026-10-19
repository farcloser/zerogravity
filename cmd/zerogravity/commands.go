//go:build darwin && arm64

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"

	"github.com/mycophonic/primordium/human"

	"github.com/farcloser/zerogravity/internal/paths"
	"github.com/farcloser/zerogravity/pkg/machine"
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

type installCmd struct {
	Name string `arg:"" help:"name of the base to create"`
}

func (installCmd) Run(_ context.Context) error {
	return fmt.Errorf("%w: install (DESIGN.md §5 waits on the macOS bindings of forkcloser/vz)", ErrNotImplemented)
}

type cloneCmd struct {
	Base string `arg:"" help:"base to clone: a VM of this project"`
	Name string `arg:"" help:"name of the new VM"`
}

// Run clones one VM of the project into another. Cloning a base from the host
// cache comes with install (DESIGN.md §5); cloning within the project is the
// same operation on a different source and is here already.
func (c cloneCmd) Run(_ context.Context, root *CLI) error {
	srcPath, err := vmDir(root, c.Base)
	if err != nil {
		return err
	}

	dstPath, err := vmDir(root, c.Name)
	if err != nil {
		return err
	}

	src, err := machine.Open(srcPath)
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

func (runCmd) Run(_ context.Context) error {
	return fmt.Errorf("%w: run (DESIGN.md §3, §6 to §9)", ErrNotImplemented)
}

type stopCmd struct {
	Name string `arg:"" help:"VM to stop"`
}

func (stopCmd) Run(_ context.Context) error {
	return fmt.Errorf("%w: stop (DESIGN.md §8)", ErrNotImplemented)
}

type ipCmd struct {
	Name string `arg:"" help:"VM to query"`
}

func (ipCmd) Run(_ context.Context) error {
	return fmt.Errorf("%w: ip (DESIGN.md §6)", ErrNotImplemented)
}

type execCmd struct {
	Name string   `arg:"" help:"VM to run in"`
	Args []string `arg:"" help:"command"`
}

func (execCmd) Run(_ context.Context) error {
	return fmt.Errorf("%w: exec (DESIGN.md §8)", ErrNotImplemented)
}

type sshCmd struct {
	Name string   `arg:"" help:"VM to connect to"`
	Args []string `arg:"" help:"arguments for ssh" optional:""`
}

func (sshCmd) Run(_ context.Context) error {
	return fmt.Errorf("%w: ssh (DESIGN.md §8)", ErrNotImplemented)
}

type listCmd struct{}

// Run lists the project's VMs with what config.json says about them. A
// directory without a readable config is listed with the error, so a broken VM
// is visible rather than hidden.
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

	if err := table.Flush(); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}

	return nil
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

	lock, err := machineDir.Lock()
	if err != nil {
		state = "running"
	} else {
		_ = machine.Unlock(lock)
	}

	return fmt.Sprintf("%s\t%d\t%s\t%s\t%s\t%s\t%s",
		name, config.CPUs, human.BinarySize(float64(config.Memory)),
		human.BinarySize(float64(config.Disk)), config.MacOS, config.Base, state)
}
