package relay

import "testing"

func TestIsIPv6Host(t *testing.T) {
	tests := []struct {
		name string
		host string
		want bool
	}{
		{name: "ipv6", host: "2001:db8::1", want: true},
		{name: "bracketed ipv6", host: "[2001:db8::1]", want: true},
		{name: "ipv4", host: "203.0.113.10", want: false},
		{name: "hostname", host: "volunteer.example.com", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsIPv6Host(tt.host); got != tt.want {
				t.Fatalf("IsIPv6Host(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}
