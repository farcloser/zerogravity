//go:build darwin

package machine_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/farcloser/zerogravity/pkg/machine"
)

func validConfig() *machine.Config {
	return &machine.Config{
		CPUs:     4,
		Memory:   8 << 30,
		Disk:     64 << 30,
		Display:  machine.DefaultDisplay(),
		User:     "zg",
		Password: "generated",
		MacOS:    "27.0",
		MAC:      "02:aa:bb:cc:dd:ee",
	}
}

func TestConfigRoundTrip(t *testing.T) {
	t.Parallel()

	dir, err := machine.Create(filepath.Join(t.TempDir(), "vm"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := validConfig()
	if writeErr := dir.WriteConfig(want); writeErr != nil {
		t.Fatalf("WriteConfig: %v", writeErr)
	}

	info, statErr := os.Stat(dir.File(machine.ConfigFile))
	if statErr != nil {
		t.Fatalf("Stat: %v", statErr)
	}

	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("config.json mode = %o, want 0600 (it carries the password)", mode)
	}

	got, err := dir.ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}

	if *got != *want {
		t.Fatalf("ReadConfig = %+v, want %+v", got, want)
	}
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()

	cases := map[string]func(*machine.Config){
		"no cpus":    func(c *machine.Config) { c.CPUs = 0 },
		"no memory":  func(c *machine.Config) { c.Memory = 0 },
		"no disk":    func(c *machine.Config) { c.Disk = 0 },
		"no display": func(c *machine.Config) { c.Display = machine.Display{} },
		"no user":    func(c *machine.Config) { c.User = "" },
		"bad mac":    func(c *machine.Config) { c.MAC = "not-a-mac" },
	}

	for name, breakIt := range cases {
		config := validConfig()
		breakIt(config)

		if err := config.Validate(); !errors.Is(err, machine.ErrConfig) {
			t.Errorf("%s: Validate = %v, want ErrConfig", name, err)
		}
	}

	dir, err := machine.Create(filepath.Join(t.TempDir(), "vm"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := os.WriteFile(dir.File(machine.ConfigFile), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := dir.ReadConfig(); !errors.Is(err, machine.ErrConfig) {
		t.Fatalf("ReadConfig on garbage = %v, want ErrConfig", err)
	}
}

func TestCreateRefusesExisting(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "vm")

	if _, err := machine.Create(path); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := machine.Create(path); !errors.Is(err, machine.ErrExists) {
		t.Fatalf("second Create = %v, want ErrExists", err)
	}

	if _, err := machine.Open(filepath.Join(t.TempDir(), "absent")); !errors.Is(err, machine.ErrMissing) {
		t.Fatalf("Open absent = %v, want ErrMissing", err)
	}
}

func TestLockIsExclusive(t *testing.T) {
	t.Parallel()

	dir, err := machine.Create(filepath.Join(t.TempDir(), "vm"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	lock, err := dir.Lock()
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// flock is per open file description, so a second open of the same path in
	// this process conflicts the way another process would.
	if _, second := dir.Lock(); !errors.Is(second, machine.ErrRunning) {
		t.Fatalf("second Lock = %v, want ErrRunning", second)
	}

	pid, pidErr := dir.RunningPID()
	if pidErr != nil || pid != os.Getpid() {
		t.Fatalf("RunningPID = %d, %v; want this process's pid", pid, pidErr)
	}

	if err := machine.Unlock(lock); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	again, againErr := dir.Lock()
	if againErr != nil {
		t.Fatalf("Lock after Unlock: %v", againErr)
	}

	_ = machine.Unlock(again)
}

func TestCloneCopiesFilesAndRecordsBase(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	src, err := machine.Create(filepath.Join(root, "base"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, name := range []string{machine.DiskFile, machine.NVRAMFile, machine.AuxiliaryFile} {
		if writeErr := os.WriteFile(src.File(name), []byte(name+" bytes"), 0o600); writeErr != nil {
			t.Fatalf("WriteFile %s: %v", name, writeErr)
		}
	}
	// identifier and hardware-model deliberately absent: a clone skips them.

	if writeErr := src.WriteConfig(validConfig()); writeErr != nil {
		t.Fatalf("WriteConfig: %v", writeErr)
	}

	dst := filepath.Join(root, "clone")

	clone, err := machine.Clone(src, dst, "sha256:abc")
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}

	for _, name := range []string{machine.DiskFile, machine.NVRAMFile, machine.AuxiliaryFile} {
		got, readErr := os.ReadFile(clone.File(name))
		if readErr != nil {
			t.Fatalf("ReadFile %s: %v", name, readErr)
		}

		if string(got) != name+" bytes" {
			t.Fatalf("%s = %q, want the source's bytes", name, got)
		}
	}

	if _, statErr := os.Stat(clone.File(machine.IdentifierFile)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("identifier: %v, want absent as in the source", statErr)
	}

	config, configErr := clone.ReadConfig()
	if configErr != nil {
		t.Fatalf("ReadConfig: %v", configErr)
	}

	if config.Base != "sha256:abc" {
		t.Fatalf("Base = %q, want the given base", config.Base)
	}

	if config.MAC == "02:aa:bb:cc:dd:ee" || config.MAC == "" {
		t.Fatalf("MAC = %q, want a fresh address for the clone", config.MAC)
	}

	// Writing to the clone leaves the source alone: copy on write.
	if writeErr := os.WriteFile(clone.File(machine.DiskFile), []byte("changed"), 0o600); writeErr != nil {
		t.Fatalf("WriteFile: %v", writeErr)
	}

	original, readErr := os.ReadFile(src.File(machine.DiskFile))
	if readErr != nil {
		t.Fatalf("ReadFile: %v", readErr)
	}

	if string(original) != machine.DiskFile+" bytes" {
		t.Fatalf("source disk changed with the clone: %q", original)
	}

	if _, again := machine.Clone(src, dst, "sha256:abc"); !errors.Is(again, machine.ErrExists) {
		t.Fatalf("Clone over existing = %v, want ErrExists", again)
	}
}

func TestRemoveRefusesRunning(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "vm")

	dir, err := machine.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	lock, err := dir.Lock()
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	if err := machine.Remove(path); !errors.Is(err, machine.ErrRunning) {
		t.Fatalf("Remove while locked = %v, want ErrRunning", err)
	}

	_ = machine.Unlock(lock)

	if err := machine.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory still there after Remove: %v", err)
	}
}

// TestNewMACIsLocalUnicastAndFresh pins the two bits the lease lookup and the
// framework both rely on: locally administered set, multicast clear, and no
// two calls alike.
func TestNewMACIsLocalUnicastAndFresh(t *testing.T) {
	t.Parallel()

	first, err := machine.NewMAC()
	if err != nil {
		t.Fatalf("NewMAC: %v", err)
	}

	second, err := machine.NewMAC()
	if err != nil {
		t.Fatalf("NewMAC: %v", err)
	}

	for _, mac := range []net.HardwareAddr{first, second} {
		if _, parseErr := net.ParseMAC(mac.String()); parseErr != nil {
			t.Fatalf("NewMAC = %s, does not parse: %v", mac, parseErr)
		}

		if mac[0]&0x02 == 0 {
			t.Fatalf("NewMAC = %s, locally administered bit clear", mac)
		}

		if mac[0]&0x01 != 0 {
			t.Fatalf("NewMAC = %s, multicast bit set", mac)
		}
	}

	if first.String() == second.String() {
		t.Fatalf("two NewMAC calls gave the same address %s", first)
	}
}

// TestRunningProbeLeavesNoPid pins that a probe never writes its own pid where
// a stop would read it, and that Unlock clears the pid a run wrote.
func TestRunningProbeLeavesNoPid(t *testing.T) {
	t.Parallel()

	dir, err := machine.Create(filepath.Join(t.TempDir(), "vm"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if running, probeErr := dir.Running(); probeErr != nil || running {
		t.Fatalf("Running on a fresh directory = %v, %v; want false", running, probeErr)
	}

	lock, err := dir.Lock()
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	if running, probeErr := dir.Running(); probeErr != nil || !running {
		t.Fatalf("Running while locked = %v, %v; want true", running, probeErr)
	}

	if pid, pidErr := dir.RunningPID(); pidErr != nil || pid != os.Getpid() {
		t.Fatalf("RunningPID after a probe = %d, %v; the probe must not have written", pid, pidErr)
	}

	if err := machine.Unlock(lock); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	if _, pidErr := dir.RunningPID(); pidErr == nil {
		t.Fatal("RunningPID after Unlock found a pid; Unlock must clear it")
	}
}
