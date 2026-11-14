//go:build darwin

package hostos_test

import (
	"testing"

	"github.com/farcloser/zerogravity/internal/hostos"
)

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
