//go:build darwin && arm64

// Package install turns Apple's restore image into a base: a machine directory
// with macOS installed, one user created on a provisioned first boot, Remote
// Login on, and the guest's address known. Unattended from end to end; the
// host must run macOS 27 and the restore image must be macOS 27 or newer,
// since the provisioning the first boot relies on exists nowhere earlier.
package install

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path"
	"time"

	"github.com/forkcloser/vz"
	"github.com/mycophonic/primordium/digest"
	"github.com/mycophonic/primordium/filesystem"
	"github.com/mycophonic/primordium/human"
	"github.com/mycophonic/primordium/store/content"

	"github.com/farcloser/zerogravity/internal/download"
	"github.com/farcloser/zerogravity/internal/hostos"
	"github.com/farcloser/zerogravity/internal/lease"
	"github.com/farcloser/zerogravity/internal/paths"
	"github.com/farcloser/zerogravity/pkg/machine"
	"github.com/farcloser/zerogravity/pkg/vm"
)

// Latest asks for the latest restore image the host supports.
const Latest = "latest"

var (
	// ErrRestoreImage reports a restore image that could not be fetched, read
	// or used.
	ErrRestoreImage = errors.New("restore image")
	// ErrRequirements reports a request below what the restore image needs.
	ErrRequirements = errors.New("below the restore image's requirements")
	// ErrFirstBoot reports a provisioned first boot that did not come up.
	ErrFirstBoot = errors.New("first boot")
	// ErrHost reports a host that cannot run the pipeline: macOS 27 is the
	// first with guest provisioning.
	ErrHost = errors.New("host")
)

// hostMinimum is the macOS the install pipeline needs on the host.
const hostMinimum = 27

// Options are what install needs beyond the directory.
type Options struct {
	// IPSW is a path to a restore image, or Latest.
	IPSW string
	// CPUs, Memory and Disk size the guest; Display is its one display.
	CPUs    uint
	Memory  uint64
	Disk    uint64
	Display machine.Display
	// User and FullName are the account the first boot creates; AutoLogin
	// logs it in at startup.
	User      string
	FullName  string
	AutoLogin bool
	// Progress receives one line per step and the installer's percentage.
	Progress io.Writer
	// FirstBootTimeout bounds the wait for the provisioned guest's ssh.
	FirstBootTimeout time.Duration
}

const (
	ipswMode        = 0o600
	passwordBytes   = 24
	progressEvery   = 2 * time.Second
	leasePoll       = time.Second
	sshPort         = "22"
	sshDialTimeout  = 2 * time.Second
	shutdownGrace   = 2 * time.Minute
	installerSettle = 3 * time.Minute
	defaultBootWait = 15 * time.Minute
	sshBannerPrefix = "SSH-"
)

// Install runs the pipeline into dir, which must not exist yet. On failure the
// directory is left for inspection; config.json is written last, so a
// directory without one is an install that did not finish.
func Install(ctx context.Context, cache, dir string, opts Options) error {
	progress := opts.Progress
	if progress == nil {
		progress = io.Discard
	}

	if opts.FirstBootTimeout == 0 {
		opts.FirstBootTimeout = defaultBootWait
	}

	// Checked first: the provisioned first boot needs macOS 27 on the host, and
	// finding that out after a twenty-minute restore would waste the restore.
	major, err := hostos.Major()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrHost, err)
	}

	if major < hostMinimum {
		return fmt.Errorf("%w: macOS %d, the provisioned first boot needs macOS %d", ErrHost, major, hostMinimum)
	}

	ipsw, release, err := restoreImage(ctx, cache, opts.IPSW, progress)
	if err != nil {
		return err
	}

	// Held through the restore: the installer reads the image from the cache.
	defer release()

	image, err := vz.LoadMacOSRestoreImageFromPath(ipsw)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrRestoreImage, ipsw, err)
	}

	requirements := image.MostFeaturefulSupportedConfiguration()
	if requirements == nil || !requirements.HardwareModel().Supported() {
		return fmt.Errorf("%w: %s has no configuration this host supports", ErrRestoreImage, ipsw)
	}

	if err = checkRequirements(requirements, opts); err != nil {
		return err
	}

	fmt.Fprintf(progress, "restore image: macOS %s (%s)\n", image.OperatingSystemVersion(), image.BuildVersion())

	target, config, err := prepare(dir, requirements.HardwareModel(), image, opts)
	if err != nil {
		return err
	}

	// One machine for the restore and the first boot: the framework locks the
	// auxiliary storage for the life of a VZVirtualMachine, and a second one
	// on the same files, made while the installer's still exists, fails to
	// start with "Failed to lock auxiliary storage". The installer's
	// completion leaves the machine running; restore settles it to stopped.
	guest, err := vm.New(vm.Options{Dir: target, Config: config})
	if err != nil {
		return err
	}

	defer guest.Close()

	if err = restore(ctx, guest, ipsw, progress); err != nil {
		return err
	}

	addr, err := firstBoot(ctx, guest, target, config, opts, progress)
	if err != nil {
		return err
	}

	fmt.Fprintf(
		progress,
		"first boot: %s answered ssh at %s; agent %s installed; shut down\n",
		config.User,
		addr,
		config.Agent,
	)

	// Written last: a directory with a config is a finished base.
	return target.WriteConfig(config)
}

// restoreImage resolves opts.IPSW to a file: a given path is used as is; Latest
// asks the framework for the URL of the latest image the host supports and
// takes the bytes from the content store under the cache root, keyed by that
// URL, fetched once and held to their digest after. The returned release must
// be called once the file is no longer read: it is the store's pin, and the
// store's garbage collection respects a pinned blob. Should Apple publish
// between the URL and the bytes, the URL names the older build and the newer
// is what the store holds, which is harmless: both are macOS 27 or newer, and
// the base records the build it got.
func restoreImage(
	ctx context.Context, cache, requested string, progress io.Writer,
) (ipsw string, release func(), err error) {
	if requested != Latest {
		if _, err = os.Stat(requested); err != nil {
			return "", nil, fmt.Errorf("%w: %w", ErrRestoreImage, err)
		}

		return requested, func() {}, nil
	}

	url, err := vz.GetLatestSupportedMacOSRestoreImageURL()
	if err != nil {
		return "", nil, fmt.Errorf("%w: latest: %w", ErrRestoreImage, err)
	}

	store, err := content.New(cache, nil)
	if err != nil {
		return "", nil, fmt.Errorf("%w: cache: %w", ErrRestoreImage, err)
	}

	fetched := false

	pin, err := store.AcquireFile(url, digest.Digest{}, func() (io.ReadCloser, error) {
		fetched = true

		fmt.Fprintf(progress, "restore image: fetching %s\n", url)

		stream, openErr := download.Open(ctx, url, func(done, total int64) {
			if total <= 0 {
				fmt.Fprintf(progress, "restore image: %s\n", human.BinarySize(float64(done)))

				return
			}

			fmt.Fprintf(progress, "restore image: %3.0f%%\n", float64(done)/float64(total)*100) //nolint:mnd // percent
		})
		if openErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrRestoreImage, openErr)
		}

		return stream, nil
	})
	if err != nil {
		_ = store.Close()

		return "", nil, fmt.Errorf("%w: %s: %w", ErrRestoreImage, path.Base(url), err)
	}

	if fetched {
		fmt.Fprint(progress, "restore image: fetched\n")
	} else {
		fmt.Fprintf(progress, "restore image: %s, cached\n", path.Base(url))
	}

	release = func() {
		_ = pin.Release()
		_ = store.Close()
	}

	// The framework takes the file's type from its name and the store's blob
	// has none: the blob is presented under Apple's name, by a hard link in
	// the cache. Replaced every run.
	link, err := paths.IPSWLink(cache, path.Base(url), pin.Path)
	if err != nil {
		release()

		return "", nil, fmt.Errorf("%w: %w", ErrRestoreImage, err)
	}

	return link, release, nil
}

func checkRequirements(requirements *vz.MacOSConfigurationRequirements, opts Options) error {
	if uint64(opts.CPUs) < requirements.MinimumSupportedCPUCount() {
		return fmt.Errorf(
			"%w: %d cpus, the image needs %d",
			ErrRequirements,
			opts.CPUs,
			requirements.MinimumSupportedCPUCount(),
		)
	}

	if opts.Memory < requirements.MinimumSupportedMemorySize() {
		return fmt.Errorf(
			"%w: %d bytes of memory, the image needs %d",
			ErrRequirements,
			opts.Memory,
			requirements.MinimumSupportedMemorySize(),
		)
	}

	return nil
}

// prepare creates the machine directory with the framework files a macOS
// guest needs before anything boots: hardware model and machine identifier as
// data, auxiliary storage created for the model, an empty disk. The config is
// returned, not written: that is Install's last step.
func prepare(
	dir string,
	model *vz.MacHardwareModel,
	image *vz.MacOSRestoreImage,
	opts Options,
) (*machine.Dir, *machine.Config, error) {
	target, err := machine.Create(dir)
	if err != nil {
		return nil, nil, err
	}

	if err = filesystem.WriteFile(
		target.File(machine.HardwareModelFile),
		model.DataRepresentation(),
		ipswMode,
	); err != nil {
		return nil, nil, fmt.Errorf("writing hardware model: %w", err)
	}

	identifier, err := vz.NewMacMachineIdentifier()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: machine identifier: %w", vm.ErrConfiguration, err)
	}

	if err = filesystem.WriteFile(
		target.File(machine.IdentifierFile),
		identifier.DataRepresentation(),
		ipswMode,
	); err != nil {
		return nil, nil, fmt.Errorf("writing machine identifier: %w", err)
	}

	if _, err = vz.NewMacAuxiliaryStorage(
		target.File(machine.AuxiliaryFile),
		vz.WithCreatingMacAuxiliaryStorage(model),
	); err != nil {
		return nil, nil, fmt.Errorf("%w: auxiliary storage: %w", vm.ErrConfiguration, err)
	}

	if err = vm.CreateDiskImage(target.File(machine.DiskFile), opts.Disk); err != nil {
		return nil, nil, err
	}

	password, err := generatePassword()
	if err != nil {
		return nil, nil, err
	}

	mac, err := machine.NewMAC()
	if err != nil {
		return nil, nil, err
	}

	config := &machine.Config{
		CPUs:      opts.CPUs,
		Memory:    opts.Memory,
		Disk:      opts.Disk,
		Display:   opts.Display,
		User:      opts.User,
		Password:  password,
		AutoLogin: opts.AutoLogin,
		MacOS:     fmt.Sprintf("%s (%s)", image.OperatingSystemVersion(), image.BuildVersion()),
		MAC:       mac.String(),
	}

	if err := config.Validate(); err != nil {
		return nil, nil, err
	}

	return target, config, nil
}

func restore(ctx context.Context, guest *vm.VM, ipsw string, progress io.Writer) error {
	installer, err := vz.NewMacOSInstaller(guest.Raw(), ipsw)
	if err != nil {
		return fmt.Errorf("%w: installer: %w", ErrRestoreImage, err)
	}

	done := make(chan error, 1)

	go func() { done <- installer.Install(ctx) }()

	ticker := time.NewTicker(progressEvery)
	defer ticker.Stop()

	for {
		select {
		case err := <-done:
			if err != nil {
				return fmt.Errorf("%w: install: %w", ErrRestoreImage, err)
			}

			fmt.Fprint(progress, "restore: done\n")

			return settle(ctx, guest, progress)
		case <-ticker.C:
			fmt.Fprintf(progress, "restore: %3.0f%%\n", installer.FractionCompleted()*100) //nolint:mnd // percent
		}
	}
}

// settle brings the machine the installer ran on to the stopped state the
// first boot needs. The installer's completion leaves the machine running
// (Start right after it fails with "transition from running to running");
// the framework does not say whether it stops on its own after that, so the
// machine gets a while to do so, and is shut down if it has not.
func settle(ctx context.Context, guest *vm.VM, progress io.Writer) error {
	if guest.Raw().State() == vz.VirtualMachineStateStopped {
		return nil
	}

	fmt.Fprint(progress, "restore: the installer's machine is still running, waiting for it to stop\n")

	settleCtx, cancel := context.WithTimeout(ctx, installerSettle)
	defer cancel()

	if err := guest.Wait(settleCtx); err == nil {
		return nil
	}

	if ctx.Err() != nil {
		return fmt.Errorf("%w: %w", ErrRestoreImage, ctx.Err())
	}

	// A machine in the error state honours no stop request: say so, rather
	// than report the stop that would fail after it.
	if guest.Raw().State() == vz.VirtualMachineStateError {
		return fmt.Errorf("%w: after install: the machine entered the error state", ErrRestoreImage)
	}

	fmt.Fprint(progress, "restore: shutting the installer's machine down\n")

	if err := guest.Shutdown(ctx, shutdownGrace); err != nil {
		return fmt.Errorf("%w: after install: %w", ErrRestoreImage, err)
	}

	return nil
}

// firstBoot starts the installed guest once with the provisioning options,
// waits for its lease and its ssh banner, and shuts it down cleanly. The
// address is returned for the message; the guest learns a new one each boot.
func firstBoot(
	ctx context.Context,
	guest *vm.VM,
	dir *machine.Dir,
	config *machine.Config,
	install Options,
	progress io.Writer,
) (netip.Addr, error) {
	provisioning := vz.MacGuestProvisioningOptions{
		FullName:            install.FullName,
		Username:            config.User,
		Password:            config.Password,
		LogsInAutomatically: config.AutoLogin,
		EnablesRemoteLogin:  true,
	}

	fmt.Fprintf(progress, "first boot: provisioning user %s\n", config.User)

	if err := guest.Start(ctx, vz.WithMacOSGuestProvisioning(provisioning)); err != nil {
		return netip.Addr{}, fmt.Errorf("%w: %w", ErrFirstBoot, err)
	}

	bootCtx, cancel := context.WithTimeout(ctx, install.FirstBootTimeout)
	defer cancel()

	addr, err := lease.Wait(bootCtx, guest.MAC(), leasePoll)
	if err != nil {
		_ = guest.Shutdown(ctx, shutdownGrace)

		return netip.Addr{}, fmt.Errorf("%w: %w", ErrFirstBoot, err)
	}

	fmt.Fprintf(progress, "first boot: guest at %s, waiting for ssh\n", addr)

	if err := waitForSSH(bootCtx, addr); err != nil {
		_ = guest.Shutdown(ctx, shutdownGrace)

		return netip.Addr{}, fmt.Errorf("%w: %w", ErrFirstBoot, err)
	}

	agentVersion, agentErr := installAgent(ctx, guest, dir, addr, config.User, config.Password, progress)
	if agentErr != nil {
		_ = guest.Shutdown(ctx, shutdownGrace)

		return netip.Addr{}, agentErr
	}

	config.Agent = agentVersion

	if err := guest.Shutdown(ctx, shutdownGrace); err != nil {
		return netip.Addr{}, fmt.Errorf("%w: shutdown: %w", ErrFirstBoot, err)
	}

	return addr, nil
}

// waitForSSH dials the guest's port 22 until a server banner arrives: the sign
// that Setup Assistant finished, the user exists and Remote Login is on.
func waitForSSH(ctx context.Context, addr netip.Addr) error {
	ticker := time.NewTicker(leasePoll)
	defer ticker.Stop()

	target := net.JoinHostPort(addr.String(), sshPort)

	for {
		if banner(ctx, target) {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("ssh on %s: %w", target, ctx.Err())
		case <-ticker.C:
		}
	}
}

func banner(ctx context.Context, target string) bool {
	dialCtx, cancel := context.WithTimeout(ctx, sshDialTimeout)
	defer cancel()

	var dialer net.Dialer

	conn, err := dialer.DialContext(dialCtx, "tcp", target)
	if err != nil {
		return false
	}

	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(sshDialTimeout))

	buf := make([]byte, len(sshBannerPrefix))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return false
	}

	return string(buf) == sshBannerPrefix
}

func generatePassword() (string, error) {
	raw := make([]byte, passwordBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating password: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(raw), nil
}
