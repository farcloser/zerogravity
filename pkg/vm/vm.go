//go:build darwin && arm64

// Package vm boots one macOS guest through Virtualization.framework: the
// machine directory's files become the platform, the disk and the boot loader;
// the guest gets one display, a keyboard and trackpad, a virtio socket, a
// network of its own, and the project's directories over virtio-fs. Nothing
// here persists state: pkg/machine owns the directory, this package owns the
// running machine.
package vm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/forkcloser/vz"
	"github.com/forkcloser/vz/vmnet"

	"github.com/farcloser/zerogravity/pkg/machine"
)

var (
	// ErrConfiguration reports a machine the framework refuses to build.
	ErrConfiguration = errors.New("vm configuration refused")
	// ErrLifecycle reports a machine that did not reach or left the expected
	// state.
	ErrLifecycle = errors.New("vm lifecycle failure")
)

// Share is a directory the guest sees under /Volumes/My Shared Files/<Name>.
type Share struct {
	Name     string
	Dir      string
	ReadOnly bool
}

// Options describe the machine to build from a machine directory.
type Options struct {
	Dir    *machine.Dir
	Config *machine.Config
	Shares []Share
}

// VM is a built, startable machine.
type VM struct {
	vm      *vz.VirtualMachine
	network *vmnet.PerVM
	mac     net.HardwareAddr
}

const statePoll = 20 * time.Millisecond

// New builds the machine. The directory must hold the hardware model, the
// machine identifier, the auxiliary storage and the disk image an install
// wrote; the config says how much CPU and memory, which display, and the MAC.
func New(opts Options) (*VM, error) {
	platform, err := platform(opts.Dir)
	if err != nil {
		return nil, err
	}

	bootloader, err := vz.NewMacOSBootLoader()
	if err != nil {
		return nil, fmt.Errorf("%w: boot loader: %w", ErrConfiguration, err)
	}

	vmc, err := vz.NewVirtualMachineConfiguration(bootloader, opts.Config.CPUs, opts.Config.Memory)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfiguration, err)
	}

	vmc.SetPlatformVirtualMachineConfiguration(platform)

	if err = configureStorage(vmc, opts.Dir); err != nil {
		return nil, err
	}

	if err = configureDisplayAndInput(vmc, opts.Config.Display); err != nil {
		return nil, err
	}

	if err = configureSocket(vmc); err != nil {
		return nil, err
	}

	network, mac, err := configureNetwork(vmc, opts.Config.MAC)
	if err != nil {
		return nil, err
	}

	if err = configureShares(vmc, opts.Shares); err != nil {
		network.Close()

		return nil, err
	}

	var ok bool
	if ok, err = vmc.Validate(); !ok || err != nil {
		network.Close()

		return nil, fmt.Errorf("%w: %w", ErrConfiguration, err)
	}

	raw, err := vz.NewVirtualMachine(vmc)
	if err != nil {
		network.Close()

		return nil, fmt.Errorf("%w: %w", ErrConfiguration, err)
	}

	return &VM{vm: raw, network: network, mac: mac}, nil
}

func platform(dir *machine.Dir) (*vz.MacPlatformConfiguration, error) {
	model, err := vz.NewMacHardwareModelWithDataPath(dir.File(machine.HardwareModelFile))
	if err != nil {
		return nil, fmt.Errorf("%w: hardware model: %w", ErrConfiguration, err)
	}

	if !model.Supported() {
		return nil, fmt.Errorf(
			"%w: the hardware model in %s is not supported on this host",
			ErrConfiguration,
			dir.Path(),
		)
	}

	identifier, err := vz.NewMacMachineIdentifierWithDataPath(dir.File(machine.IdentifierFile))
	if err != nil {
		return nil, fmt.Errorf("%w: machine identifier: %w", ErrConfiguration, err)
	}

	auxiliary, err := vz.NewMacAuxiliaryStorage(dir.File(machine.AuxiliaryFile))
	if err != nil {
		return nil, fmt.Errorf("%w: auxiliary storage: %w", ErrConfiguration, err)
	}

	config, err := vz.NewMacPlatformConfiguration(
		vz.WithMacHardwareModel(model),
		vz.WithMacMachineIdentifier(identifier),
		vz.WithMacAuxiliaryStorage(auxiliary),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: platform: %w", ErrConfiguration, err)
	}

	return config, nil
}

func configureStorage(vmc *vz.VirtualMachineConfiguration, dir *machine.Dir) error {
	attachment, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(
		dir.File(machine.DiskFile),
		false,
		vz.DiskImageCachingModeAutomatic,
		vz.DiskImageSynchronizationModeFsync,
	)
	if err != nil {
		return fmt.Errorf("%w: disk: %w", ErrConfiguration, err)
	}

	disk, err := vz.NewVirtioBlockDeviceConfiguration(attachment)
	if err != nil {
		return fmt.Errorf("%w: block device: %w", ErrConfiguration, err)
	}

	vmc.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{disk})

	return nil
}

// configureDisplayAndInput attaches one display, a keyboard and a trackpad,
// window or not: a macOS guest that expects a display behaves the same
// headless, and a window attached later finds the devices there.
func configureDisplayAndInput(vmc *vz.VirtualMachineConfiguration, display machine.Display) error {
	graphics, err := vz.NewMacGraphicsDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("%w: graphics: %w", ErrConfiguration, err)
	}

	screen, err := vz.NewMacGraphicsDisplayConfiguration(display.Width, display.Height, display.DPI)
	if err != nil {
		return fmt.Errorf("%w: display: %w", ErrConfiguration, err)
	}

	graphics.SetDisplays(screen)
	vmc.SetGraphicsDevicesVirtualMachineConfiguration([]vz.GraphicsDeviceConfiguration{graphics})

	keyboard, err := vz.NewMacKeyboardConfiguration()
	if err != nil {
		return fmt.Errorf("%w: keyboard: %w", ErrConfiguration, err)
	}

	vmc.SetKeyboardsVirtualMachineConfiguration([]vz.KeyboardConfiguration{keyboard})

	trackpad, err := vz.NewMacTrackpadConfiguration()
	if err != nil {
		return fmt.Errorf("%w: trackpad: %w", ErrConfiguration, err)
	}

	vmc.SetPointingDevicesVirtualMachineConfiguration([]vz.PointingDeviceConfiguration{trackpad})

	return nil
}

func configureSocket(vmc *vz.VirtualMachineConfiguration) error {
	socket, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("%w: vsock: %w", ErrConfiguration, err)
	}

	vmc.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{socket})

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("%w: entropy: %w", ErrConfiguration, err)
	}

	vmc.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	return nil
}

// configureNetwork gives the guest a vmnet network of its own and the MAC the
// config records, so the DHCP lease the guest takes can be found by it.
func configureNetwork(vmc *vz.VirtualMachineConfiguration, mac string) (*vmnet.PerVM, net.HardwareAddr, error) {
	hardware, err := net.ParseMAC(mac)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: mac %q: %w", ErrConfiguration, mac, err)
	}

	network, err := vmnet.NewPerVM()
	if err != nil {
		return nil, nil, err
	}

	attachment, err := vz.NewVmnetNetworkDeviceAttachment(network.Network())
	if err != nil {
		network.Close()

		return nil, nil, fmt.Errorf("%w: vmnet attachment: %w", ErrConfiguration, err)
	}

	device, err := vz.NewVirtioNetworkDeviceConfiguration(attachment)
	if err != nil {
		network.Close()

		return nil, nil, fmt.Errorf("%w: network device: %w", ErrConfiguration, err)
	}

	address, err := vz.NewMACAddress(hardware)
	if err != nil {
		network.Close()

		return nil, nil, fmt.Errorf("%w: mac %q: %w", ErrConfiguration, mac, err)
	}

	device.SetMACAddress(address)
	vmc.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{device})

	return network, hardware, nil
}

// configureShares puts every share behind one virtio-fs device carrying the
// macOS automount tag, so the guest mounts them under /Volumes/My Shared Files
// without being told.
func configureShares(vmc *vz.VirtualMachineConfiguration, shares []Share) error {
	if len(shares) == 0 {
		return nil
	}

	directories := make(map[string]*vz.SharedDirectory, len(shares))

	for _, share := range shares {
		directory, err := vz.NewSharedDirectory(share.Dir, share.ReadOnly)
		if err != nil {
			return fmt.Errorf("%w: share %s: %w", ErrConfiguration, share.Dir, err)
		}

		directories[share.Name] = directory
	}

	multiple, err := vz.NewMultipleDirectoryShare(directories)
	if err != nil {
		return fmt.Errorf("%w: shares: %w", ErrConfiguration, err)
	}

	tag, err := vz.MacOSGuestAutomountTag()
	if err != nil {
		return fmt.Errorf("%w: automount tag: %w", ErrConfiguration, err)
	}

	device, err := vz.NewVirtioFileSystemDeviceConfiguration(tag)
	if err != nil {
		return fmt.Errorf("%w: virtio-fs: %w", ErrConfiguration, err)
	}

	device.SetDirectoryShare(multiple)
	vmc.SetDirectorySharingDevicesVirtualMachineConfiguration([]vz.DirectorySharingDeviceConfiguration{device})

	return nil
}

// Raw is the framework machine, for the installer.
func (m *VM) Raw() *vz.VirtualMachine { return m.vm }

// Network is the guest's own network.
func (m *VM) Network() *vmnet.PerVM { return m.network }

// MAC is the guest's network device address, the key to its DHCP lease.
func (m *VM) MAC() net.HardwareAddr { return m.mac }

// Start starts the machine and returns once it runs. Options are the
// framework's: the provisioned first boot passes WithMacOSGuestProvisioning.
func (m *VM) Start(ctx context.Context, opts ...vz.VirtualMachineStartOption) error {
	if err := m.vm.Start(opts...); err != nil {
		return fmt.Errorf("%w: start: %w", ErrLifecycle, err)
	}

	return m.waitFor(ctx, vz.VirtualMachineStateRunning)
}

// Wait returns when the guest has stopped, or with an error when the machine
// entered the error state or the context ended.
func (m *VM) Wait(ctx context.Context) error {
	return m.waitFor(ctx, vz.VirtualMachineStateStopped)
}

// Shutdown asks the guest to shut down and waits up to grace for it, then stops
// the machine outright. A macOS guest honours the request as a power-button
// press and shuts down cleanly.
func (m *VM) Shutdown(ctx context.Context, grace time.Duration) error {
	if m.vm.CanRequestStop() {
		if _, err := m.vm.RequestStop(); err == nil {
			graceCtx, cancel := context.WithTimeout(ctx, grace)
			defer cancel()

			if err := m.waitFor(graceCtx, vz.VirtualMachineStateStopped); err == nil {
				return nil
			}
		}
	}

	if m.vm.State() == vz.VirtualMachineStateStopped {
		return nil
	}

	if err := m.vm.Stop(); err != nil {
		return fmt.Errorf("%w: stop: %w", ErrLifecycle, err)
	}

	return m.waitFor(ctx, vz.VirtualMachineStateStopped)
}

// Close releases the network. The machine must be stopped.
func (m *VM) Close() {
	m.network.Close()
}

// Window opens a window on the machine and blocks until it is closed. It must
// run on the main thread: call it from main, after Start, never from a
// goroutine.
func (m *VM) Window(title string) error {
	runtime.LockOSThread()

	defer runtime.UnlockOSThread()

	if err := m.vm.StartGraphicApplication(
		float64(defaultWindowWidth), float64(defaultWindowHeight), vz.WithWindowTitle(title),
	); err != nil {
		return fmt.Errorf("%w: window: %w", ErrLifecycle, err)
	}

	return nil
}

const (
	defaultWindowWidth  = 1280
	defaultWindowHeight = 800
	diskMode            = 0o600
)

// Connect dials a guest vsock port.
func (m *VM) Connect(port uint32) (net.Conn, error) {
	return vz.VsockConnect(m.vm, port)
}

// ConnectRetry dials a guest vsock port, retrying until timeout.
func (m *VM) ConnectRetry(ctx context.Context, port uint32, timeout time.Duration) (net.Conn, error) {
	return vz.VsockConnectRetry(ctx, m.vm, port, timeout)
}

func (m *VM) waitFor(ctx context.Context, want vz.VirtualMachineState) error {
	ticker := time.NewTicker(statePoll)
	defer ticker.Stop()

	for {
		switch state := m.vm.State(); {
		case state == want:
			return nil
		case state == vz.VirtualMachineStateError:
			return fmt.Errorf("%w: the machine entered the error state", ErrLifecycle)
		case want == vz.VirtualMachineStateRunning && state == vz.VirtualMachineStateStopped:
			return fmt.Errorf("%w: the machine stopped before running", ErrLifecycle)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: waiting for state %v: %w", ErrLifecycle, want, ctx.Err())
		case <-ticker.C:
		}
	}
}

// CreateDiskImage makes a sparse file of size bytes for a guest disk; the
// framework fills it as the guest writes.
func CreateDiskImage(path string, size uint64) error {
	file, err := os.OpenFile(
		path,
		os.O_RDWR|os.O_CREATE|os.O_EXCL,
		diskMode,
	) // #nosec G304 -- the machine directory's own disk file
	if err != nil {
		return fmt.Errorf("creating disk image %s: %w", path, err)
	}

	defer func() { _ = file.Close() }()

	if err := file.Truncate(int64(size)); err != nil { // #nosec G115 -- disk sizes fit int64
		return fmt.Errorf("sizing disk image %s: %w", path, err)
	}

	return nil
}
