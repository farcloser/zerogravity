package paths_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/farcloser/zerogravity/internal/paths"
)

func TestValidName(t *testing.T) {
	t.Parallel()

	good := []string{"a", "base", "tahoe-27.0", "my_vm", "X1"}
	for _, name := range good {
		if err := paths.ValidName(name); err != nil {
			t.Errorf("ValidName(%q) = %v, want nil", name, err)
		}
	}

	bad := []string{"", ".", "..", "-x", "a/b", "a b", "../x", "disk.img/", string(make([]byte, 65))}
	for _, name := range bad {
		if err := paths.ValidName(name); err == nil {
			t.Errorf("ValidName(%q) = nil, want an error", name)
		}
	}
}

func TestVMUnderProjectBuildDir(t *testing.T) {
	t.Parallel()

	project := t.TempDir()

	dir, err := paths.VM(project, "base")
	if err != nil {
		t.Fatalf("VM: %v", err)
	}

	want := filepath.Join(project, ".build", "vms", "base")
	if dir != want {
		t.Fatalf("VM = %q, want %q", dir, want)
	}

	if _, err := paths.VM(project, "../escape"); err == nil {
		t.Fatal("VM accepted a name that leaves the directory")
	}
}

func TestProjectDefaultsToWorkingDirectory(t *testing.T) {
	t.Parallel()

	explicit := t.TempDir()

	got, err := paths.Project(explicit)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}

	if !filepath.IsAbs(got) {
		t.Fatalf("Project = %q, want an absolute path", got)
	}

	got, err = paths.Project("")
	if err != nil {
		t.Fatalf("Project(\"\"): %v", err)
	}

	if !filepath.IsAbs(got) {
		t.Fatalf("Project(\"\") = %q, want an absolute path", got)
	}
}

func TestIPSWLink(t *testing.T) {
	t.Parallel()

	cache := t.TempDir()
	blob := filepath.Join(cache, "blob")

	if err := os.WriteFile(blob, []byte("PK\x03\x04 image"), 0o600); err != nil {
		t.Fatal(err)
	}

	link, err := paths.IPSWLink(cache, "UniversalMac_27.0.1_26A434_Restore.ipsw", blob)
	if err != nil {
		t.Fatal(err)
	}

	if want := filepath.Join(cache, "ipsw", "UniversalMac_27.0.1_26A434_Restore.ipsw"); link != want {
		t.Fatalf("link %q, want %q", link, want)
	}

	assertSameFile(t, blob, link)

	// A second run replaces the entry, whatever it was: here a stale regular
	// file left by an earlier scheme.
	other := filepath.Join(cache, "blob2")
	if err := os.WriteFile(other, []byte("another build"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(link, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := paths.IPSWLink(cache, "UniversalMac_27.0.1_26A434_Restore.ipsw", other); err != nil {
		t.Fatal(err)
	}

	assertSameFile(t, other, link)
}

func TestIPSWLinkRefusesABadName(t *testing.T) {
	t.Parallel()

	cache := t.TempDir()
	blob := filepath.Join(cache, "blob")

	if err := os.WriteFile(blob, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"", ".", "/", ".ipsw", "image", "image.zip", "a/b.ipsw", "../b.ipsw"} {
		if _, err := paths.IPSWLink(cache, name, blob); !errors.Is(err, paths.ErrIPSWName) {
			t.Errorf("name %q: got %v, want ErrIPSWName", name, err)
		}
	}
}

// assertSameFile holds link to be a hard link to blob: the same file, and a
// regular file in its own right, not a symbolic link that resolves to one.
func assertSameFile(t *testing.T, blob, link string) {
	t.Helper()

	infoBlob, err := os.Stat(blob)
	if err != nil {
		t.Fatal(err)
	}

	infoLink, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}

	if !infoLink.Mode().IsRegular() {
		t.Fatalf("%s is %v, want a regular file (a hard link), not a symbolic link", link, infoLink.Mode())
	}

	if !os.SameFile(infoBlob, infoLink) {
		t.Fatalf("%s and %s are not the same file", blob, link)
	}
}
