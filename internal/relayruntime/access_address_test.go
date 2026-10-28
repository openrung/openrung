// SPDX-License-Identifier: GPL-3.0-or-later

package relayruntime

import (
	"net/netip"
	"strings"
	"testing"
)

func TestVerifyAccessAddress(t *testing.T) {
	access := netip.MustParseAddr("2001:db8:1::10")
	primary := HostAddress{IP: netip.MustParseAddr("2001:db8:1::1")}
	for _, tc := range []struct {
		name       string
		configured []HostAddress
		wantErr    string
	}{
		{"deprecated and settled", []HostAddress{primary, {IP: access, Deprecated: true}}, ""},
		{"absent", []HostAddress{primary}, "not configured"},
		{"no addresses", nil, "not configured"},
		{"preferred", []HostAddress{primary, {IP: access}}, "not deprecated"},
		{"tentative", []HostAddress{{IP: access, Deprecated: true, Tentative: true}}, "tentative"},
		{"dad failed", []HostAddress{{IP: access, Deprecated: true, DADFailed: true}}, "duplicate address detection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyAccessAddress(access, tc.configured)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want one mentioning %q", err, tc.wantErr)
			}
		})
	}
}

func TestVerifyIPv4AccessAddress(t *testing.T) {
	access := netip.MustParseAddr("198.51.100.9")
	primary := netip.MustParseAddr("203.0.113.7")
	for _, tc := range []struct {
		name       string
		configured []netip.Addr
		egress     netip.Addr
		wantErr    string
	}{
		{"secondary", []netip.Addr{primary, access}, primary, ""},
		{"absent", []netip.Addr{primary}, primary, "not configured"},
		{"no addresses", nil, primary, "not configured"},
		{"egress source", []netip.Addr{primary, access}, access, "source address of outgoing IPv4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyIPv4AccessAddress(access, tc.configured, tc.egress)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want one mentioning %q", err, tc.wantErr)
			}
		})
	}
}

// The real lookups run unprivileged; the route source, when the host has an
// IPv4 route at all, is one of the host's own addresses.
func TestHostIPv4AddressesAndRouteSource(t *testing.T) {
	addresses, err := HostIPv4Addresses()
	if err != nil {
		t.Fatalf("HostIPv4Addresses: %v", err)
	}
	for _, address := range addresses {
		if !address.Is4() {
			t.Fatalf("non-IPv4 address in the list: %v", address)
		}
	}
	source, err := IPv4RouteSource()
	if err != nil {
		t.Skipf("no IPv4 route on this host: %v", err)
	}
	if err := VerifyIPv4AccessAddress(source, addresses, source); err == nil {
		t.Fatalf("the route source %s passed as an access address", source)
	}
}
