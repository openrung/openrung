package broker

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIPResolver(t *testing.T) {
	// 192.0.2.0/24 (a documentation range) stands in for an operator-configured trusted proxy.
	resolver := newClientIPResolver([]string{"192.0.2.0/24", " 127.0.0.1/32 ", "not-a-cidr"})

	cases := []struct {
		name       string
		remoteAddr string
		cfIP       string
		xff        string
		want       string
	}{
		{"untrusted peer ignores forwarded headers", "203.0.113.10:5000", "", "9.9.9.9", "203.0.113.10"},
		{"cloudflare peer is not trusted unless configured", "104.16.0.5:443", "198.51.100.7", "198.51.100.8", "104.16.0.5"},
		{"trusted peer honors x-forwarded-for", "192.0.2.44:443", "", "198.51.100.7", "198.51.100.7"},
		{"trusted peer prefers cf-connecting-ip", "192.0.2.44:443", "198.51.100.7", "10.0.0.1", "198.51.100.7"},
		{"trusted peer without headers falls back to peer", "192.0.2.44:443", "", "", "192.0.2.44"},
		{"x-forwarded-for uses left-most entry", "192.0.2.44:443", "", "198.51.100.7, 70.41.3.18, 150.172.238.178", "198.51.100.7"},
		{"trimmed loopback cidr is trusted", "127.0.0.1:51000", "", "198.51.100.9", "198.51.100.9"},
		{"bare remote addr without port", "192.0.2.44", "", "198.51.100.7", "198.51.100.7"},
		{"invalid forwarded values fall back to peer", "192.0.2.44:443", "not-an-ip", "also-bad", "192.0.2.44"},
		{"v4-mapped ipv6 peer matches ipv4 cidr", "[::ffff:192.0.2.44]:443", "", "198.51.100.7", "198.51.100.7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.cfIP != "" {
				req.Header.Set("CF-Connecting-IP", tc.cfIP)
			}
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := resolver.clientIP(req); got != tc.want {
				t.Fatalf("clientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUnconfiguredResolverTrustsNoProxy(t *testing.T) {
	resolver := newClientIPResolver(nil)
	for _, remoteAddr := range []string{"104.16.0.5:443", "[2400:cb00::1]:443", "127.0.0.1:51000"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil)
		req.RemoteAddr = remoteAddr
		req.Header.Set("CF-Connecting-IP", "198.51.100.7")
		req.Header.Set("X-Forwarded-For", "198.51.100.8")
		if got, want := resolver.clientIP(req), peerIP(remoteAddr); got != want {
			t.Fatalf("clientIP(%s) = %q, want peer %q", remoteAddr, got, want)
		}
	}
}

func TestRelayListHonorsForwardedClientIPFromConfiguredProxy(t *testing.T) {
	sink := &memoryTelemetrySink{}
	server := NewServer(NewStore(), Config{
		SigningSeed:       testSigningSeed(),
		TelemetrySink:     sink,
		TrustedProxyCIDRs: []string{"127.0.0.1/32"},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/relays?limit=5", nil)
	req.RemoteAddr = "127.0.0.1:51000" // the local TLS terminator
	req.Header.Set("X-Forwarded-For", "203.0.113.77")
	req.Header.Set("X-OpenRung-Client-ID", "client-1")
	req.Header.Set("X-OpenRung-Session-ID", "session-1")
	server.ServeHTTP(httptest.NewRecorder(), req)

	if len(sink.records) != 1 {
		t.Fatalf("expected one client_seen record, got %d", len(sink.records))
	}
	if got := sink.records[0].SourceIP; got != "203.0.113.77" {
		t.Fatalf("expected forwarded client IP, got %q", got)
	}
}

func TestRelayListIgnoresForwardedClientIPFromUntrustedPeer(t *testing.T) {
	sink := &memoryTelemetrySink{}
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), TelemetrySink: sink})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/relays?limit=5", nil)
	req.RemoteAddr = "104.16.0.9:5000"               // a Cloudflare address, not a configured proxy
	req.Header.Set("X-Forwarded-For", "10.10.10.10") // spoofed — must be ignored
	req.Header.Set("X-OpenRung-Client-ID", "client-1")
	req.Header.Set("X-OpenRung-Session-ID", "session-1")
	server.ServeHTTP(httptest.NewRecorder(), req)

	if len(sink.records) != 1 {
		t.Fatalf("expected one client_seen record, got %d", len(sink.records))
	}
	if got := sink.records[0].SourceIP; got != "104.16.0.9" {
		t.Fatalf("untrusted forwarded header must be ignored; got %q", got)
	}
}
