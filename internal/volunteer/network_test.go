package volunteer

import (
	"net"
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
