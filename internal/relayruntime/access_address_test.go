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
	other := netip.MustParseAddr("192.0.2.1")
	route := func(table uint32, dst string, src netip.Addr) IPv4Route {
		return IPv4Route{Table: table, Dst: netip.MustParsePrefix(dst), PrefSrc: src}
	}
	isolated := []IPv4Route{
		route(254, "0.0.0.0/0", primary),
		route(254, "172.31.1.1/32", primary),
		route(254, "172.17.0.0/16", other),
		route(100, "0.0.0.0/0", primary),
	}
	for _, tc := range []struct {
		name       string
		configured []netip.Addr
		routes     []IPv4Route
		wantErr    string
	}{
		{"every route names another source", []netip.Addr{primary, access}, isolated, ""},
		{"no routes", []netip.Addr{primary, access}, nil, ""},
		{"absent", []netip.Addr{primary}, isolated, "not configured"},
		{"no addresses", nil, isolated, "not configured"},
		{"default route sourced from it", []netip.Addr{primary, access},
			[]IPv4Route{route(254, "0.0.0.0/0", access)}, "route to 0.0.0.0/0 (table 254) uses 198.51.100.9"},
		// The default route (and so a lookup for any one internet
		// destination) uses the primary, but another destination does not.
		{"one destination sourced from it", []netip.Addr{primary, access},
			append(append([]IPv4Route(nil), isolated...), route(254, "192.0.2.0/24", access)), "route to 192.0.2.0/24 (table 254) uses 198.51.100.9"},
		{"policy-routing table sourced from it", []netip.Addr{primary, access},
			append(append([]IPv4Route(nil), isolated...), route(1000, "0.0.0.0/0", access)), "(table 1000) uses 198.51.100.9"},
		// Without a preferred source the kernel picks one from the
		// interface per lookup, which may be the access address.
		{"a route without a preferred source", []netip.Addr{primary, access},
			append(append([]IPv4Route(nil), isolated...), route(254, "198.18.0.0/15", netip.Addr{})), "route to 198.18.0.0/15 (table 254) names no preferred source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyIPv4AccessAddress(access, tc.configured, tc.routes)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want one mentioning %q", err, tc.wantErr)
			}
		})
	}
}

func TestHostIPv4AddressesListsIPv4Only(t *testing.T) {
	addresses, err := HostIPv4Addresses()
	if err != nil {
		t.Fatalf("HostIPv4Addresses: %v", err)
	}
	for _, address := range addresses {
		if !address.Is4() {
			t.Fatalf("non-IPv4 address in the list: %v", address)
		}
	}
}
