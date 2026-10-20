// Package lease reads the addresses vmnet's DHCP server handed out. A macOS
// guest cannot be told its address before boot, so the one boot before the
// agent exists learns it from the host's lease file, matched on the MAC
// address zerogravity gave the guest's network device.
package lease

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// File is where macOS's DHCP server for vmnet networks records its leases.
const File = "/var/db/dhcpd_leases"

var (
	// ErrNoLease reports that no lease matches the MAC address.
	ErrNoLease = errors.New("no dhcp lease for the guest")
	// ErrFormat reports a lease file the parser does not understand.
	ErrFormat = errors.New("lease file format")
)

// Lease is one record of the file.
type Lease struct {
	Name string
	IP   netip.Addr
	MAC  net.HardwareAddr
}

// Parse reads the lease file's format: records in braces, one key=value per
// line. The server writes MAC octets without zero padding ("1,9e:7:f1:…"), so
// addresses are normalised through net.ParseMAC after padding.
func Parse(data []byte) ([]Lease, error) {
	var (
		leases  []Lease
		current *Lease
	)

	scanner := bufio.NewScanner(bytes.NewReader(data))

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		switch {
		case line == "{":
			current = &Lease{}
		case line == "}":
			if current != nil && current.IP.IsValid() && current.MAC != nil {
				leases = append(leases, *current)
			}

			current = nil
		case current == nil:
			continue
		default:
			key, value, found := strings.Cut(line, "=")
			if !found {
				continue
			}

			if err := current.set(key, value); err != nil {
				return nil, err
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading leases: %w", err)
	}

	return leases, nil
}

func (l *Lease) set(key, value string) error {
	switch key {
	case "name":
		l.Name = value
	case "ip_address":
		addr, err := netip.ParseAddr(value)
		if err != nil {
			return fmt.Errorf("lease ip_address %q: %w", value, err)
		}

		l.IP = addr
	case "hw_address":
		// "1,<mac>": the leading 1 is the hardware type, Ethernet.
		_, mac, found := strings.Cut(value, ",")
		if !found {
			return fmt.Errorf("%w: hw_address %q: no hardware type", ErrFormat, value)
		}

		parsed, err := parseMAC(mac)
		if err != nil {
			return fmt.Errorf("lease hw_address %q: %w", value, err)
		}

		l.MAC = parsed
	}

	return nil
}

func parseMAC(mac string) (net.HardwareAddr, error) {
	octets := strings.Split(mac, ":")

	const octetsInMAC = 6
	if len(octets) != octetsInMAC {
		return nil, fmt.Errorf("%w: %d octets, want %d", ErrFormat, len(octets), octetsInMAC)
	}

	addr := make(net.HardwareAddr, 0, octetsInMAC)

	for _, octet := range octets {
		value, err := strconv.ParseUint(octet, 16, 8)
		if err != nil {
			return nil, fmt.Errorf("octet %q: %w", octet, err)
		}

		addr = append(addr, byte(value))
	}

	return addr, nil
}

// Lookup returns the address leased to mac, or ErrNoLease.
func Lookup(leases []Lease, mac net.HardwareAddr) (netip.Addr, error) {
	for _, lease := range leases {
		if bytes.Equal(lease.MAC, mac) {
			return lease.IP, nil
		}
	}

	return netip.Addr{}, fmt.Errorf("%w: %s", ErrNoLease, mac)
}

// Read parses the host's lease file.
func Read() ([]Lease, error) {
	data, err := os.ReadFile(File)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", File, err)
	}

	return Parse(data)
}

// Wait polls the lease file until mac has an address or the context ends. A
// fresh guest asks for its lease within seconds of boot; the poll is coarse on
// purpose, the file is rewritten whole by the server.
func Wait(ctx context.Context, mac net.HardwareAddr, every time.Duration) (netip.Addr, error) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		leases, err := Read()
		if err == nil {
			if addr, err := Lookup(leases, mac); err == nil {
				return addr, nil
			}
		}

		select {
		case <-ctx.Done():
			return netip.Addr{}, fmt.Errorf("%w: %s: %w", ErrNoLease, mac, ctx.Err())
		case <-ticker.C:
		}
	}
}
