# zerogravity: design

> A lightweight tart. One binary that installs, clones and boots a macOS virtual
> machine, headless or with a window, inside a project, with a shared folder
> and a way in. No daemon, nothing system-wide.

Status: design, 2026-10-19. Nothing here is built. Facts are marked as
verified, read in a source, or to be measured; the spikes at the end turn the
last kind into the first.

Decided by Apostasie, 2026-10-19:

- macOS 27 only, host and guest; the unattended install is Apple's guest
  provisioning API. No older guests, no third-party images, no Setup
  Assistant automation of any kind.
- The binary is `zerogravity`.
- What ossein and zerogravity share lives in a new fork repository,
  `forkcloser/go-vz`; section 4 lists what goes there and what does not.
- Guest user `zg`, auto-login off by default, host cache under
  `~/Library/Caches/zerogravity` (recommendations, section 11).

## 1. What it is for

A developer, a CI job or an agent session needs a clean macOS to build, test
or run something: an Xcode build, a notarisation dry run, a Homebrew formula,
a signed binary, anything that must see a real macOS and not the host. Today
that is tart, which is a fine tool with a daemon-free model but its own image
registry, its own conventions, Swift, and a scope wider than ours. zerogravity
is the smallest thing that does our part of it, built the way ossein is built,
sharing ossein's code where the code is the same.

Three uses set the shape:

1. **Project runner.** `zerogravity run` in a repository boots a VM that
   belongs to that repository, with the repository shared into it, and runs a
   command or gives a shell. State lives under the project's build directory.
2. **Disposable clean rooms.** Every run can start from a pristine base and
   throw the changes away. Clones are instant.
3. **Unattended.** Everything works headless from a script, the install
   included. A window is an option, never a requirement.

Non-goals: guests older than macOS 27, Linux guests (ossein), Intel hosts or
guests, nested virtualisation, suspend and resume, VNC, an image registry,
pulling anyone else's images, a long-running service of any kind.

## 2. Constraints that decide the design

Verified or read in a source; the source is named.

- **Host.** Apple silicon, macOS 27 or newer. Virtualization framework only.
  The binary carries the `com.apple.security.virtualization` entitlement and
  is ad-hoc signed at build time, as ossein's is. macOS 27 "Golden Gate" has
  been public since 2026-09-14 and runs on every Apple silicon Mac; this
  machine is on 26.6.2 with Xcode 27.1 and the macOS 27 SDK installed, so the
  build side is ready and the host upgrade is a routine update.
- **Guest.** macOS 27 or newer, installed by us from Apple's restore image.
  Nothing else boots under zerogravity.
- **Unattended first boot is Apple's.** On the first start after a restore,
  the framework accepts `VZMacGuestProvisioningOptions` on
  `VZMacOSVirtualMachineStartOptions`: full name, username, password, whether
  to log in automatically, whether to enable Remote Login. The guest's Setup
  Assistant consumes them, creates the user and lands on the desktop. The
  options are evaluated on that first boot only and ignored by guests older
  than macOS 27, which is why older guests are out. (WWDC26 session 224; tart
  `--provisioning-opts`, guarded `@available(macOS 27)`.) The Go binding does
  not exist in Code-Hex/vz and is written in go-vz; spike S1.
- **Two macOS guests per host.** The framework refuses a third running macOS
  VM. We live inside it.
- **macOS guests get their own devices.** A Mac platform configuration
  (hardware model, machine identifier, auxiliary storage) and the macOS boot
  loader, not a kernel. Installation goes through `VZMacOSInstaller` from an
  IPSW. (Code-Hex/vz; the 2023 go-vz prototype ran this path.)
- **Shared directories are virtio-fs.** `VZVirtioFileSystemDeviceConfiguration`
  is the framework's only file sharing. A share configured with the
  `com.apple.virtio-fs.automount` tag mounts itself in the guest under
  `/Volumes/My Shared Files/<name>`. (Code-Hex/vz `MacOSGuestAutomountTag`;
  tart's `--dir`.)
- **vsock works in macOS guests.** Tart's guest agent talks to the host over a
  virtio socket control channel, and ossein's whole control plane is Connect
  RPC over vsock. The guest side is a plain `AF_VSOCK` socket. (tart Run.swift
  and tart-guest-agent; ossein `pkg/guest`.) Measured here in S2.
- **Networking: own the network.** The framework's NAT attachment joins every
  guest to one host-wide vmnet network whose state any VM product can rewrite,
  and ossein was taken down by exactly that. ossein creates one vmnet network
  per VM through `VZVmnetNetworkDeviceAttachment` (macOS 26+), carried in its
  vz fork because the attachment type cannot be added from outside the
  package. (ossein `third_party/vz/FORK.md`, `pkg/vm/network.go`.) Same path.
- **The agent sandbox cannot boot VMs.** Claude Code's sandbox denies the
  `kern.hv_support` sysctl, so sessions build and test zerogravity but a human
  runs the boots. (limen book, upstream.md.) Every boot must therefore be one
  command a session can print and a human can paste.

## 3. Shape

```
zerogravity install <name> [--ipsw <path>|latest] [--cpu N] [--memory 8GiB] [--disk 64GiB]
zerogravity clone   <base> <name>          instant copy of a base into this project
zerogravity run     <name> [--gui] [--share dir[:ro]]... [--ssh-key path] [--] [cmd...]
zerogravity stop    <name>
zerogravity ip      <name>                 the guest's address on its own network
zerogravity exec    <name> -- cmd...       run through the agent, no network needed
zerogravity ssh     <name> [-- ssh args]   ssh as the guest user
zerogravity ls / zerogravity rm <name>
```

One process per running VM: `zerogravity run` is the VM. It holds the
framework objects, the vmnet network, the vsock listener and the window if
any, and exits when the guest halts or the command returns. There is no
daemon to discover, no socket to leave behind. A second invocation finds a
running VM through a lock file under the VM directory (primordium
`filesystem/flock`), exactly as ossein finds its guests.

### Layout

```
~/Library/Caches/zerogravity/            host cache: primordium's content store
  index.dat                              restore image URL -> digest
  cache/<blake3>                         restore images, by content
  bases/<sha256>/{disk.img,nvram.bin,aux.bin,identifier,config.json}
<project>/.build/vms/<name>/            one VM
  disk.img        APFS clone of the base's disk.img (copy-on-write, instant)
  nvram.bin       EFI variable store, per VM
  aux.bin         Mac auxiliary storage, per VM
  identifier      Mac machine identifier, per VM
  config.json     cpu, memory, display, shares, network, the base it came from
  run.lock        held by the running zerogravity process
```

Clones use `clonefile(2)` on APFS, so a 50 GB base clones in milliseconds and
costs only what the guest writes afterwards. The cache directory is the one
path outside the project the tool writes, like `~/Library/Caches/ossein`, and
enters the agent sandbox's write list the same way.

### Primordium and xos

- `filesystem` and `filesystem/flock` for every file the tool owns: atomic
  writes of `config.json`, the run lock, directory creation with the right
  umask. `filesystem/dirs` for the cache location.
- `store/content` and `digest` for the host cache: IPSWs and bases are stored
  and looked up by digest, verified on use, never trusted by name.
- `network/transporter` for the IPSW download: backoff, retry, one connection
  pool. Resuming a body cut mid-way is not a transport's job and sits above it,
  in `internal/download`.
- `bytesize` for `--memory 8GiB`; `system/machine` for host CPU and memory;
  `app/logger` and `app/shutdown` for the process lifecycle, so a run
  interrupted by signal stops the guest cleanly and releases the network.
- `xos` for every `os` call that touches a file another process may replace.
- Conventions as ossein's: kong for the CLI, limen for lint, test, release and
  the signed, embedded-artifact build; `just build` as the only sanctioned
  build.

## 4. Shared with ossein: `forkcloser/go-vz`

ossein carries Code-Hex/vz in its tree at `third_party/vz`: upstream plus the
unmerged vmnet pull request, five fixes of its own, and everything ossein does
not use removed, the macOS guest bindings among them. That copy cannot be
imported by a second project without each importer vendoring it again, and
zerogravity needs the removed half back. So the fork becomes a repository in
the forks organisation, maintained the way every forkcloser fork is, and both
projects depend on it as a module.

### What goes into go-vz

1. **ossein's fork, as it is.** The starting point is `third_party/vz` moved
   out of ossein's tree: upstream at ossein's pinned tag, the vmnet pull
   request (Code-Hex/vz #205) with `VZVmnetNetworkDeviceAttachment` and the
   `vmnet` package, and ossein's five marked fixes (the SDK gate that fails
   the build instead of raising at run time, the version guards on the vmnet
   accessors, the `VirtioSocketListener` close and accept lifecycle,
   `VirtioSocketConnection.CloseWrite`, the removal of the eager notification
   channels and their dependency). Nothing ossein removed comes back on
   principle; the fork stays what it is, a binding carrying exactly what its
   consumers call. There are two consumers and no third in sight.
2. **The macOS guest bindings zerogravity calls, re-added one by one from
   upstream**, each with the file it lives in and the version guard it needs,
   and nothing beside it. The list, from the install and run paths in
   sections 5 to 9:
   - `MacOSRestoreImage` with `FetchLatestSupportedMacOSRestoreImage` and
     `LoadMacOSRestoreImageFromPath`, and `MacOSConfigurationRequirements`
     (hardware model, minimum CPU and memory);
   - `MacHardwareModel`, `MacMachineIdentifier`, `MacAuxiliaryStorage`,
     `MacPlatformConfiguration`;
   - `MacOSBootLoader`;
   - `MacOSInstaller` with its progress and completion;
   - `MacGraphicsDeviceConfiguration` and `MacGraphicsDisplayConfiguration`,
     one display, needed even headless;
   - `USBKeyboardConfiguration` and `MacTrackpadConfiguration`, for `--gui`
     only, and `StartGraphicApplication` to open the window;
   - the virtio-fs automount tag, if the fork's `shared_directory.go` no
     longer carries it.
   Audio, USB mass storage, SPICE, Rosetta, NBD, console and clipboard devices
   do not come back; nothing here uses them. The import compiles against the
   macOS 27 SDK and the file-level guards are what upstream has.
3. **Upstream tracking.** The forks owner rebases the fork on upstream tags as
   every forkcloser fork is, with the two ossein files and the re-added files
   as the patch set. Re-adding a file verbatim from upstream keeps that patch
   set a list of file names rather than a diff.
4. **New: the provisioning binding.** `MacGuestProvisioningOptions` and
   `MacOSVirtualMachineStartOptions` with `SetGuestProvisioningOptions`,
   guarded `macOSAvailable(27)`, SDK-gated the same way as the vmnet calls.
   Offered upstream once it works.
5. **A per-VM network helper**, moved from ossein's `pkg/vm/network.go`: create
   one vmnet network in shared mode for one VM, derive gateway and guest
   addressing from the subnet the framework hands back, close it with the VM.
   Both tools want precisely this and nothing else from vmnet; a hundred
   lines, no ossein types in its signature.
6. **A vsock dialer**, the host side of ossein's control plane: a `net.Conn`
   over `VirtioSocketDevice` to a guest port, with the retry-until-the-guest-
   listens loop. The Connect and HTTP wiring on top stays in each project; it
   depends on connectrpc and on each project's own protocol.

### What stays where it is

- **Boot contracts.** ossein's kernel-and-initfs boot with its command line;
  zerogravity's macOS boot loader and platform configuration. Nothing in
  common beyond the binding.
- **Guest agents and their protocols.** ossein's PID 1 speaks the vendored
  apple/containerization `SandboxContext`; zerogravity's launchd daemon speaks
  a four-call contract of its own. Same transport, different wire.
- **Images and storage.** ossein's OCI, erofs and ext4 volumes; zerogravity's
  IPSW, bases and APFS clones.
- **The macOS guest address from a DHCP lease** (section 5): zerogravity only,
  ossein assigns addresses statically inside its Linux guest.
- **Entitlement and signing in the build**: a limen recipe, not Go.

### Order of work

go-vz is created first, from ossein's tree plus the restored upstream files
and the provisioning binding; ossein then replaces `third_party/vz` and
`pkg/vm/network.go` with the module in one pull request and proves nothing
changed with its own tests; zerogravity depends on go-vz from its first
commit. The forks owner maintains the fork against upstream tags; the
provisioning binding and the lifecycle fixes are offered upstream.

## 5. `zerogravity install`: from Apple's restore image to a ready base, unattended

One command, no questions, no window. Four steps, each resumable from the
cache if the previous run got that far.

1. **Restore image.** `--ipsw latest` asks the framework for the URL of the
   latest supported restore image and acquires it from the content store by
   that URL: a miss streams the download into the store, which hashes it and
   commits it under its digest; a hit costs a lookup. The stream resumes a
   body cut mid-way with a Range request held to the first response's ETag,
   so the store never hashes a splice of two publications. The pin on the
   blob is held through the restore. `--ipsw <path>` uses a file. The image's most featureful supported
   configuration gives the hardware model and the minimum CPU and memory.
2. **Restore.** Create the platform configuration (hardware model, fresh
   machine identifier, auxiliary storage), a disk image of the requested size,
   and run `VZMacOSInstaller`. Fifteen to thirty minutes, progress on stderr.
3. **Provisioned first boot.** Start the VM once with provisioning options:
   user `zg`, full name "zerogravity", a password generated for this base and
   stored in its `config.json` with mode 0600, Remote Login on, auto-login as
   asked (off by default). Wait for the agent-less signal that the guest is
   up: the one DHCP lease on the VM's own network, then an ssh banner.
   Headless throughout.
4. **Agent.** Over that ssh connection, install the zerogravity agent and its
   launchd daemon plist (section 8), then shut the guest down cleanly. The
   agent binary is embedded in `zerogravity` with `//go:embed` and verified by
   the same harness as the rest, the way ossein embeds its kernel and initfs;
   no download, no second artifact to pin.

The result is a **base**: a macOS 27 installation with one user, Remote Login
on, and the agent, recorded in the cache under the digest of its `disk.img`
with the agent version and the macOS build in `config.json`. Clones inherit
all of it and never go through first boot again.

Two design points to hold on to. The generated password is the base's, never
reused, and `zerogravity ssh` never types it: keys do the work after the first
boot (section 8). And the first boot is the only moment the tool depends on
the network to reach the guest; from the agent onward it does not.

## 6. Networking and finding the guest

One vmnet network per VM in shared mode, created by the host process through
go-vz, torn down when the process exits. The guest gets an address from
vmnet's DHCP on that network. A macOS guest cannot be told its address before
boot, so the address is learnt, in this order:

1. **From the agent over vsock**, which needs no network at all and is the
   normal case for every clone of a base.
2. **From the DHCP lease**, during the one boot before the agent exists: the
   network is ours and has exactly one client, so the one lease on it is the
   guest. The network helper already knows the subnet; the lease file read is
   the only new piece. Tart matches MAC addresses across a shared network; we
   do not have to, the network has one tenant.

Outbound: NAT through the host, as the framework provides. Inbound from the
host to the guest: the host is on the vmnet subnet, so `ssh` to the guest's
address just works, no port forwarding. Inbound from elsewhere: no. Bridged
networking is out of scope.

## 7. Sharing a folder

virtio-fs, automount tag, one share per `--share` flag, named by directory
basename unless given. In the guest: `/Volumes/My Shared Files/<name>`. Read
only when asked. That is the only mechanism the framework offers and the one
tart uses, so correctness is Apple's problem up to a point; the points we
measure are in section 10, because the project runner's whole promise is "the
repository is there, and it is right".

Known shape of the risk, from tart users and the Linux virtio-fs record:
metadata caching that lags host changes, file locking across the boundary,
extended attributes and resource forks, case sensitivity (host APFS is
case-insensitive by default, guest APFS likewise, but a project's expectations
are the project's), `mmap` writeback, and performance on many small files,
which is what a Git checkout and an Xcode build are. If virtio-fs fails a
correctness test we care about, the fallback is NFS from the host over the
vmnet subnet, which is well understood and slower; the design keeps the share
behind one interface so that swap is local.

## 8. The way in: ssh and an agent, both

The question was ssh or a socket. The answer is both, for different callers.

**ssh for people and tools.** Every macOS developer tool assumes ssh: Xcode's
remote builds, rsync, scp, Ansible, a person at a terminal. The guest has
Remote Login on from the provisioned first boot. `zerogravity run --ssh-key
~/.ssh/id_ed25519.pub` delivers the public key at boot (below) and
`zerogravity ssh` wraps the connection with the right user, address and host
key, so nothing is typed twice. Password login over ssh is turned off by the
agent on its first run; the generated password remains for console login
under `--gui`.

**A vsock agent for zerogravity itself.** The tool needs to know the guest is
up, its address, to run a command and get its exit status and output, to
deliver the ssh key and a few settings at boot, and to ask for a clean
shutdown. Doing that over the network means waiting for DHCP, trusting a host
key on first contact, and failing when the guest's sshd or firewall is
misconfigured, which is the very thing a clean room is for finding. A vsock
channel exists the moment the guest's launchd starts the agent, needs no
address, no key, and cannot be reached by anything but the host process that
owns the VM. ossein's control plane is exactly this; the dialer comes from
go-vz, the Connect wiring is the project's.

The agent is small, Go, static, installed by `zerogravity install` step 4 as
a launchd daemon. It serves, over Connect on vsock:

- `Ready` with the guest's addresses and macOS version;
- `Configure`: write `authorized_keys` for the user, set the hostname, set
  the clock, apply a small set of defaults; called once per boot with what
  `run` was given;
- `Exec` with stdio and exit status;
- `Shutdown`.

Nothing else. No clipboard, no disk resize, no file transfer: the share does
files, ssh does the rest.

**Key delivery at boot.** `run --ssh-key` hands the key to `Configure`; the
agent writes it and nothing about the caller persists in the base.

### Decisions made while building, 2026-10-19

- **The restore image is fetched by us, not by the framework.** The
  framework's `FetchLatestSupportedMacOSRestoreImage` is one connection with
  no resume; a real run lost 54% of 15 GiB to one "unexpected EOF". The
  framework now gives the URL only; `internal/download` streams it and
  resumes a cut in place, and the content store owns the bytes. The cost: a
  run killed mid-download starts over, since the store stages per run and a
  partial file is never kept under a name.

- **A control socket beside the lock.** A virtio socket belongs to the process
  that owns the machine, so `exec`, `ip` and `ssh` from another process could
  not reach the agent. `run` listens on `control.sock` in the VM directory and
  relays each connection to the agent's vsock port; the other commands dial
  that. The lock, not the socket file, says a VM is running.
- **The agent travels beside the binary, not inside it.** `just build` writes
  `zerogravity` and `zerogravity-agent` side by side; install reads the agent
  from the binary's own directory. Embedding would have tied every `go build`
  to a prior agent build, as ossein's embeds do; a sibling file costs nothing
  and keeps `go build ./...` honest.
- **The agent is installed over one ssh with the generated password**, in
  install's last step: upload, `sudo -S` with the password on stdin, launchd
  daemon, then the agent is proven over vsock and asked to turn password ssh
  off. The password never travels again.
- **Every VM has its own MAC**, generated at install and again on clone, so
  the one DHCP lease on its network is found by it when no agent runs yet.
- **The running process records its pid in the lock file**, in place, never
  by rename: `stop` reads it and sends SIGTERM, which `run` turns into the
  guest's clean shutdown.
- **Install refuses before spending anything** when the host is older than
  macOS 27, read from `SystemVersion.plist` rather than a sysctl the sandbox
  denies.

## 9. GUI

`--gui` opens a window on the VM through the framework's view, which
Code-Hex/vz exposes as `StartGraphicApplication` (an `NSApplication` run
loop in the same process). Headless is the default and attaches a graphics
device anyway, so a guest that expects a display behaves the same either way
and a window can be attached to a running VM later if that proves useful.
Auto-login at provisioning is the user's choice: on for a VM meant to be used
with a window, off otherwise.

## 10. Spikes, in order

Each is a day or less, produces numbers, and ends in a decision recorded in
this file. They assume a macOS 27 host; the first act is the upgrade. S1 also
creates go-vz, since nothing runs without it.

- **S1. go-vz and the unattended install.** Create `forkcloser/go-vz` from
  ossein's tree, re-add the macOS bindings listed in section 4 one by one and
  write the provisioning binding; `zerogravity install --ipsw latest` through step 3; ssh in with the
  generated password. Measures time from restore to ssh.
- **S2. vsock agent.** A twenty-line Go program listening on `AF_VSOCK`
  inside the guest, started by launchd; the host connects through go-vz's
  dialer. Measures connect latency after boot.
- **S3. Clone and disk.** `clonefile` the base, boot a clone headless and with
  `--gui`, write 5 GB, measure time and host space. Confirms the per-project
  layout costs what it claims.
- **S4. virtio-fs correctness and speed.** A Git checkout of a large
  repository on the share: clone, status, checkout branches, build with Xcode
  and with Go; compare to the same on a guest-local disk; run a small
  correctness battery (xattrs, locks, mmap, rename over open file, case).
  Decides whether virtio-fs is the only sharing or NFS is kept as fallback.
- **S5. Lifecycle.** Signal `run` mid-boot and mid-command; confirm the guest
  stops, the network is released, the lock clears, and a second `run` works
  at once. Two guests at once, then a third to see the framework's refusal
  surface as a clear error.
- **S6. ossein on go-vz.** ossein swaps its in-tree fork for the module; its
  test suite and a container boot prove nothing changed. Closes the sharing
  question for good.

### Running the spikes

Boots run on a macOS 27 host, outside CI. Each is one command from the
zerogravity directory, after
`just build`; the output of each is what the spike measures, and goes into
this file as a line under the spike.

```sh
# S1: unattended install, restore to ssh. Prints each step and its time.
time .build/zerogravity install base

# S2: the agent over vsock; `ip` comes from the agent, not from a lease.
.build/zerogravity run base &
.build/zerogravity ip base
.build/zerogravity exec base -- sw_vers
.build/zerogravity stop base

# S3: clone and disk. Instant clone, then 5 GB written in the guest.
time .build/zerogravity clone base work
du -sh .build/vms/base .build/vms/work
.build/zerogravity run work &
.build/zerogravity exec work -- dd if=/dev/zero of=/tmp/five bs=1m count=5000
du -sh .build/vms/work
.build/zerogravity stop work

# S4: virtio-fs correctness and speed, the repository shared in.
.build/zerogravity run work --share "$PWD" &
.build/zerogravity exec work -- ls "/Volumes/My Shared Files/zerogravity"
.build/zerogravity exec work -- git -C "/Volumes/My Shared Files/zerogravity" status
.build/zerogravity stop work

# S5: lifecycle. A second run while one holds the lock refuses; stop ends it.
.build/zerogravity run work &
.build/zerogravity run work
.build/zerogravity stop work

# A window on a running VM, auto-login or not.
.build/zerogravity run work --gui
```

## 11. Recommendations taken as decisions

Apostasie left these to me; they stand unless overruled.

- **Guest user `zg`**, full name "zerogravity". Short to type at a console,
  distinct from `admin`, never shared with another tool's images.
- **Auto-login off by default**, on with `install --autologin` for a base
  meant for `--gui` work. A headless clean room has no one to log in.
- **Cache at `~/Library/Caches/zerogravity`**, content-addressed, the same
  convention and the same sandbox allowance as ossein's cache.
- **Password login over ssh off** once the agent runs; the generated password
  stays for the console. Gatekeeper stays on: nothing in the design needs it
  off, and a clean room that disables it is not clean.
