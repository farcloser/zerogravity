//go:build darwin

// Package machine is one virtual machine on disk: its directory, the files
// Virtualization.framework needs, its config.json, and the lock a running
// zerogravity holds on it. Booting is pkg/vm's; this package never touches the
// framework, so every function here is tested on any Mac.
package machine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/forkcloser/xos"
	"github.com/mycophonic/primordium/filesystem"
	"github.com/mycophonic/primordium/filesystem/flock"
	"golang.org/x/sys/unix"
)

// The files a VM directory holds. Every one is per VM: a clone copies them,
// a base keeps its own.
const (
	// DiskFile is the guest's disk image.
	DiskFile = "disk.img"
	// NVRAMFile is the EFI variable store.
	NVRAMFile = "nvram.bin"
	// AuxiliaryFile is the Mac auxiliary storage.
	AuxiliaryFile = "aux.bin"
	// IdentifierFile is the Mac machine identifier's data representation.
	IdentifierFile = "identifier"
	// HardwareModelFile is the Mac hardware model's data representation.
	HardwareModelFile = "hardware-model"
	// ConfigFile is the VM's config.json.
	ConfigFile = "config.json"
	// LockFile is held by the zerogravity process running the VM.
	LockFile = "run.lock"
)

// Files are the framework files a clone takes from its base, in the order
// they are cloned.
func Files() []string {
	return []string{DiskFile, NVRAMFile, AuxiliaryFile, IdentifierFile, HardwareModelFile}
}

const (
	dirMode    = 0o700
	configMode = 0o600
	lockMode   = 0o600

	defaultDisplayWidth  = 1920
	defaultDisplayHeight = 1080
	defaultDisplayDPI    = 72
)

var (
	// ErrExists reports a VM directory that is already there.
	ErrExists = errors.New("vm already exists")
	// ErrMissing reports a VM directory, or a file in it, that is not there.
	ErrMissing = errors.New("vm not found")
	// ErrRunning reports a VM another zerogravity process holds.
	ErrRunning = errors.New("vm is running")
	// ErrConfig reports a config.json that does not parse or does not hold up.
	ErrConfig = errors.New("invalid vm config")
)

// Config is config.json: what the VM is made of. Sizes are bytes.
type Config struct {
	// Base is the digest of the base this VM was cloned from, empty for a base.
	Base string `json:"base,omitempty"`
	// CPUs is the guest's CPU count.
	CPUs uint `json:"cpus"`
	// Memory is the guest's RAM, in bytes.
	Memory uint64 `json:"memory"`
	// Disk is the disk image's size, in bytes.
	Disk uint64 `json:"disk"`
	// Display is the graphics display attached, headless or not.
	Display Display `json:"display"`
	// User is the guest account the provisioned first boot created.
	User string `json:"user"`
	// Password is that account's password, generated at install, kept for
	// console login; ssh uses keys. The file is mode 0600.
	Password string `json:"password,omitempty"`
	// AutoLogin is whether the guest logs the account in at startup.
	AutoLogin bool `json:"autoLogin"`
	// MacOS is the guest's macOS version, from the restore image.
	MacOS string `json:"macos,omitempty"`
	// Agent is the version of the zerogravity agent installed in the guest,
	// empty until install's last step ran.
	Agent string `json:"agent,omitempty"`
}

// Display is the one display the graphics device presents to the guest, also
// when no window shows it: macOS behaves the same headless or not.
type Display struct {
	Width  int64 `json:"width"`
	Height int64 `json:"height"`
	DPI    int64 `json:"dpi"`
}

// DefaultDisplay is what a VM gets when nobody asks: a plain 1920×1080 at
// 72 DPI, the framework's own default density.
func DefaultDisplay() Display {
	return Display{Width: defaultDisplayWidth, Height: defaultDisplayHeight, DPI: defaultDisplayDPI}
}

// Validate reports the first thing wrong with a config.
func (c *Config) Validate() error {
	switch {
	case c.CPUs == 0:
		return fmt.Errorf("%w: cpus must be at least 1", ErrConfig)
	case c.Memory == 0:
		return fmt.Errorf("%w: memory must be set", ErrConfig)
	case c.Disk == 0:
		return fmt.Errorf("%w: disk size must be set", ErrConfig)
	case c.Display.Width <= 0 || c.Display.Height <= 0 || c.Display.DPI <= 0:
		return fmt.Errorf("%w: display needs positive width, height and dpi", ErrConfig)
	case c.User == "":
		return fmt.Errorf("%w: user must be set", ErrConfig)
	}

	return nil
}

// Dir is a VM directory.
type Dir struct {
	path string
}

// Open opens an existing VM directory.
func Open(path string) (*Dir, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrMissing, path)
		}

		return nil, fmt.Errorf("vm directory %s: %w", path, err)
	}

	if !info.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrMissing, path)
	}

	return &Dir{path: path}, nil
}

// Create makes a new, empty VM directory, private to the user, and refuses one
// that exists.
func Create(path string) (*Dir, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrExists, path)
	}

	if err := os.MkdirAll(path, dirMode); err != nil {
		return nil, fmt.Errorf("creating vm directory %s: %w", path, err)
	}

	return &Dir{path: path}, nil
}

// Path is the directory.
func (d *Dir) Path() string { return d.path }

// File is the path of one of the VM's files.
func (d *Dir) File(name string) string { return filepath.Join(d.path, name) }

// ReadConfig reads and validates config.json.
func (d *Dir) ReadConfig() (*Config, error) {
	data, err := xos.ReadFile(d.File(ConfigFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s has no %s", ErrMissing, d.path, ConfigFile)
		}

		return nil, fmt.Errorf("reading %s: %w", d.File(ConfigFile), err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrConfig, d.File(ConfigFile), err)
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", d.File(ConfigFile), err)
	}

	return &config, nil
}

// WriteConfig validates and writes config.json atomically, mode 0600: the
// file carries the guest password.
func (d *Dir) WriteConfig(config *Config) error {
	if err := config.Validate(); err != nil {
		return err
	}

	data, err := json.MarshalIndent(
		config,
		"",
		"  ",
	) // #nosec G117 -- the VM's own generated password, in a 0600 file only it reads
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}

	if err := filesystem.WriteFile(d.File(ConfigFile), append(data, '\n'), configMode); err != nil {
		return fmt.Errorf("writing %s: %w", d.File(ConfigFile), err)
	}

	return nil
}

// Lock takes the VM's run lock without blocking, so that one zerogravity
// process runs a VM at a time; a second one learns the VM is running from the
// error. The lock lives until Unlock or the process exits.
func (d *Dir) Lock() (*os.File, error) {
	// TryLock opens the file it locks and creates nothing: the lock file is
	// made here, once, and lives with the directory.
	created, err := xos.OpenFile(d.File(LockFile), os.O_RDONLY|os.O_CREATE, lockMode)
	if err != nil {
		return nil, fmt.Errorf("creating lock file in %s: %w", d.path, err)
	}

	_ = created.Close()

	lock, err := flock.TryLock(d.File(LockFile))
	if err != nil {
		if errors.Is(err, flock.ErrLockWouldBlock) {
			return nil, fmt.Errorf("%w: %s", ErrRunning, d.path)
		}

		return nil, fmt.Errorf("locking %s: %w", d.path, err)
	}

	return lock, nil
}

// Unlock releases a lock Lock returned.
func Unlock(lock *os.File) error {
	if err := flock.Unlock(lock); err != nil {
		return fmt.Errorf("unlocking: %w", err)
	}

	return nil
}

// Clone makes dst from src: every framework file is an APFS clone of the
// source's (copy-on-write, so a 50 GB disk clones in milliseconds and costs
// what the guest writes afterwards), and config.json is the source's with Base
// set to base. Files the source lacks are skipped: a base made by an installer
// that does not write one of them is still clonable. Fails if dst exists.
func Clone(src *Dir, dst, base string) (*Dir, error) {
	config, err := src.ReadConfig()
	if err != nil {
		return nil, err
	}

	target, err := Create(dst)
	if err != nil {
		return nil, err
	}

	for _, name := range Files() {
		from := src.File(name)
		if _, err := os.Stat(from); errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err := unix.Clonefile(from, target.File(name), unix.CLONE_NOFOLLOW); err != nil {
			_ = os.RemoveAll(dst)

			return nil, fmt.Errorf("cloning %s into %s: %w", from, dst, err)
		}
	}

	config.Base = base
	if err := target.WriteConfig(config); err != nil {
		_ = os.RemoveAll(dst)

		return nil, err
	}

	return target, nil
}

// Remove deletes a VM directory that nobody runs.
func Remove(path string) error {
	dir, err := Open(path)
	if err != nil {
		return err
	}

	lock, err := dir.Lock()
	if err != nil {
		return err
	}

	defer func() { _ = Unlock(lock) }()

	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("removing %s: %w", path, err)
	}

	return nil
}
