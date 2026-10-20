package hostos_test

import (
	"errors"
	"testing"

	"github.com/farcloser/zerogravity/internal/hostos"
)

const plist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>ProductBuildVersion</key>
	<string>25G83</string>
	<key>ProductName</key>
	<string>macOS</string>
	<key>ProductVersion</key>
	<string>26.6.2</string>
</dict>
</plist>
`

func TestMajorFrom(t *testing.T) {
	t.Parallel()

	major, err := hostos.MajorFrom([]byte(plist))
	if err != nil {
		t.Fatalf("MajorFrom: %v", err)
	}

	if major != 26 {
		t.Fatalf("MajorFrom = %d, want 26", major)
	}

	if _, err := hostos.MajorFrom([]byte("<plist/>")); !errors.Is(err, hostos.ErrVersion) {
		t.Fatalf("MajorFrom on an empty plist = %v, want ErrVersion", err)
	}

	version, versionErr := hostos.VersionFrom([]byte(plist))
	if versionErr != nil || version != "26.6.2" {
		t.Fatalf("VersionFrom = %q, %v; want 26.6.2", version, versionErr)
	}
}

func TestMajorReadsTheHost(t *testing.T) {
	t.Parallel()

	major, err := hostos.Major()
	if err != nil {
		t.Fatalf("Major: %v", err)
	}

	if major < 11 {
		t.Fatalf("Major = %d, want a macOS on Apple silicon", major)
	}
}
