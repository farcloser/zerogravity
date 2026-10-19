package paths_test

import (
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

	want := filepath.Join(project, ".build", "zerogravity", "base")
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
