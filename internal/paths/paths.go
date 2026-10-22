// Package paths is where zerogravity keeps things: the host cache, shared by
// every project on the machine, and the project's own VM directories under its
// build directory. Nothing else on the host is written.
package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// AppName names the cache directory and the per-project build subdirectory.
const AppName = "zerogravity"

// buildDir is the project's build directory, the one limen's recipes use and
// .gitignore ignores; the VM directories live under it.
const buildDir = ".build"

// ErrName reports a VM name the layout refuses: a name is a single path
// component of letters, digits, dots, dashes and underscores, so it cannot
// leave its directory or collide with the files the layout writes.
var ErrName = errors.New("invalid vm name")

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ErrIPSWName reports a restore image name the link refuses: it must be a
// single path component ending in .ipsw, since Virtualization takes the
// file's type from that extension.
var ErrIPSWName = errors.New("invalid restore image name")

const (
	// ipswDir holds the links under the cache root.
	ipswDir      = "ipsw"
	ipswSuffix   = ".ipsw"
	cacheDirMode = 0o700
)

// Cache is the host cache root, ~/Library/Caches/zerogravity on macOS. Restore
// images and bases live under it, content-addressed. A cache, not application
// data: the user's cache cleaners may empty it, and everything in it can be
// fetched or installed again. Whoever writes into it creates it.
func Cache() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("cache directory: %w", err)
	}

	return filepath.Join(base, AppName), nil
}

// Project is the root of the project a VM belongs to: the given directory, or
// the working directory when empty, made absolute.
func Project(dir string) (string, error) {
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("working directory: %w", err)
		}

		dir = cwd
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("project directory %q: %w", dir, err)
	}

	return abs, nil
}

// vmsDir is the VMs' directory under .build. Not the application's name: in
// this repository `just build` writes the binary at .build/zerogravity.
const vmsDir = "vms"

// VMs is the directory holding a project's VMs: <project>/.build/vms.
func VMs(project string) string {
	return filepath.Join(project, buildDir, vmsDir)
}

// VM is one VM's directory under the project, after the name passed
// ValidName.
func VM(project, name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}

	return filepath.Join(VMs(project), name), nil
}

// ValidName refuses anything that is not a plain name.
func ValidName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf(
			"%w: %q (letters, digits, '.', '-', '_'; 1 to 64 characters; must start with a letter or digit)",
			ErrName, name,
		)
	}

	return nil
}

// IPSWLink makes <cache>/ipsw/<name> a hard link to blob and returns its path.
// Virtualization reads a restore image's type from the .ipsw extension and the
// content store keeps its blob under a bare name, so the blob is presented
// through this link. An existing entry under name, a stale link to a blob the
// store has since reclaimed included, is replaced. A hard link, since that is
// the shape known to load; both paths are under the cache root, one volume.
func IPSWLink(cache, name, blob string) (string, error) {
	if name == "" || name != filepath.Base(name) || !strings.HasSuffix(name, ipswSuffix) || name == ipswSuffix {
		return "", fmt.Errorf("%w: %q", ErrIPSWName, name)
	}

	dir := filepath.Join(cache, ipswDir)
	if err := os.MkdirAll(dir, cacheDirMode); err != nil {
		return "", fmt.Errorf("restore image link: %w", err)
	}

	link := filepath.Join(dir, name)

	if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("restore image link: %w", err)
	}

	if err := os.Link(blob, link); err != nil {
		return "", fmt.Errorf("restore image link: %w", err)
	}

	return link, nil
}
