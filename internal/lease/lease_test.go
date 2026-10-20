package lease_test

import (
	"errors"
	"net"
	"testing"

	"github.com/farcloser/zerogravity/internal/lease"
)

const sample = `{
	name=somebody
	ip_address=192.168.64.3
	hw_address=1,9e:7:f1:a:bb:1
	identifier=1,9e:7:f1:a:bb:1
	lease=0x6acc7626
}
{
	ip_address=192.168.65.2
	hw_address=1,2:aa:bb:cc:dd:ee
	identifier=1,2:aa:bb:cc:dd:ee
	lease=0x6a6d00ea
}
{
	ip_address=not-an-address-free-record
}
`

func TestParseNormalisesUnpaddedOctets(t *testing.T) {
	t.Parallel()

	if _, err := lease.Parse([]byte(sample)); err == nil {
		t.Fatal("Parse accepted a record with an unparsable ip_address")
	}

	valid := sample[:len(sample)-len("{\n\tip_address=not-an-address-free-record\n}\n")]

	leases, err := lease.Parse([]byte(valid))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(leases) != 2 {
		t.Fatalf("Parse = %d leases, want 2", len(leases))
	}

	want, _ := net.ParseMAC("9e:07:f1:0a:bb:01")
	if leases[0].MAC.String() != want.String() {
		t.Fatalf("MAC = %s, want %s", leases[0].MAC, want)
	}

	if leases[0].Name != "somebody" || leases[0].IP.String() != "192.168.64.3" {
		t.Fatalf("lease 0 = %+v", leases[0])
	}
}

func TestLookup(t *testing.T) {
	t.Parallel()

	valid := sample[:len(sample)-len("{\n\tip_address=not-an-address-free-record\n}\n")]

	leases, err := lease.Parse([]byte(valid))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	mac, _ := net.ParseMAC("02:aa:bb:cc:dd:ee")

	addr, err := lease.Lookup(leases, mac)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	if addr.String() != "192.168.65.2" {
		t.Fatalf("Lookup = %s, want 192.168.65.2", addr)
	}

	other, _ := net.ParseMAC("02:00:00:00:00:01")
	if _, err := lease.Lookup(leases, other); !errors.Is(err, lease.ErrNoLease) {
		t.Fatalf("Lookup unknown = %v, want ErrNoLease", err)
	}
}
