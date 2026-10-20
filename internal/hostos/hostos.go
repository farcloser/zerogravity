// Package hostos reads the host's macOS version from the file every macOS
// carries, so a pipeline that needs a given release refuses before it has
// spent anything. The sysctl the shell tool reads is denied under some
// sandboxes; the plist is a plain file read.
package hostos

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/forkcloser/xos"
)

// File is where macOS records its product version.
const File = "/System/Library/CoreServices/SystemVersion.plist"

// ErrVersion reports a version file the parser does not understand.
var ErrVersion = errors.New("macOS version")

var productVersion = regexp.MustCompile(`<key>ProductVersion</key>\s*<string>((\d+)(?:\.\d+)*)</string>`)

// MajorFrom parses the major version out of a SystemVersion.plist.
func MajorFrom(plist []byte) (int, error) {
	match := productVersion.FindSubmatch(plist)
	if match == nil {
		return 0, fmt.Errorf("%w: no ProductVersion in the plist", ErrVersion)
	}

	major, err := strconv.Atoi(string(match[2]))
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrVersion, err)
	}

	return major, nil
}

// Major is the host's macOS major version, 27 for macOS 27.
func Major() (int, error) {
	data, err := xos.ReadFile(File)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrVersion, err)
	}

	return MajorFrom(data)
}

// VersionFrom parses the full product version, "26.6.2", out of a
// SystemVersion.plist.
func VersionFrom(plist []byte) (string, error) {
	match := productVersion.FindSubmatch(plist)
	if match == nil {
		return "", fmt.Errorf("%w: no ProductVersion in the plist", ErrVersion)
	}

	return string(match[1]), nil
}

// Version is the host's full macOS version.
func Version() (string, error) {
	data, err := xos.ReadFile(File)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrVersion, err)
	}

	return VersionFrom(data)
}
