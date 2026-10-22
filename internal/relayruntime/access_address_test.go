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
