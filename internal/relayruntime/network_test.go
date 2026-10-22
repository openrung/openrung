package relayruntime

import (
	"net"
	"net/netip"
	"testing"
)

func TestIsAdvertisableIPv6(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want bool
	}{
		{name: "global ipv6", ip: "2001:4860:4860::8888", want: true},
		{name: "ula ipv6", ip: "fd00::1", want: false},
		{name: "link local ipv6", ip: "fe80::1", want: false},
		{name: "ipv4", ip: "203.0.113.10", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAdvertisableIPv6(net.ParseIP(tt.ip)); got != tt.want {
				t.Fatalf("isAdvertisableIPv6(%q) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func TestSameIPMatchesAnySpelling(t *testing.T) {
	access := netip.MustParseAddr("2001:db8:1::10")
	if !sameIP(net.ParseIP("2001:DB8:1:0:0:0:0:10"), access) {
		t.Fatal("the same address in another spelling did not match")
	}
	if sameIP(net.ParseIP("2001:db8:1::1"), access) {
		t.Fatal("a different address matched")
	}
	if sameIP(net.ParseIP("2001:db8:1::10"), netip.Addr{}) {
		t.Fatal("an empty exclusion matched")
	}
}
