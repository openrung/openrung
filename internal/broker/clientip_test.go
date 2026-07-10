package broker

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIPResolver(t *testing.T) {
	// 192.0.2.0/24 (a documentation range) stands in for an extra operator-configured trusted proxy.
	resolver := newClientIPResolver([]string{"192.0.2.0/24"})

	cases := []struct {
		name       string
		remoteAddr string
		cfIP       string
		xff        string
		want       string
	}{
		{"untrusted peer ignores forwarded headers", "203.0.113.10:5000", "", "9.9.9.9", "203.0.113.10"},
		{"trusted cloudflare peer honors x-forwarded-for", "104.16.0.5:443", "", "198.51.100.7", "198.51.100.7"},
		{"trusted cloudflare peer prefers cf-connecting-ip", "104.16.0.5:443", "198.51.100.7", "10.0.0.1", "198.51.100.7"},
		{"trusted peer without headers falls back to peer", "104.16.0.5:443", "", "", "104.16.0.5"},
		{"x-forwarded-for uses left-most entry", "104.16.0.5:443", "", "198.51.100.7, 70.41.3.18, 150.172.238.178", "198.51.100.7"},
		{"operator-configured extra cidr is trusted", "192.0.2.44:443", "", "198.51.100.9", "198.51.100.9"},
		{"bare remote addr without port", "104.16.0.5", "", "198.51.100.7", "198.51.100.7"},
		{"invalid forwarded values fall back to peer", "104.16.0.5:443", "not-an-ip", "also-bad", "104.16.0.5"},
		{"ipv6 cloudflare peer honors header", "[2400:cb00::1]:443", "", "198.51.100.7", "198.51.100.7"},
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

func TestRelayListHonorsForwardedClientIPFromCloudflare(t *testing.T) {
	sink := &memoryTelemetrySink{}
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), TelemetrySink: sink})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/relays?limit=5", nil)
	req.RemoteAddr = "104.16.0.9:443" // a Cloudflare edge IP
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
	req.RemoteAddr = "203.0.113.42:5000"             // NOT Cloudflare (e.g. a direct hit on the raw origin)
	req.Header.Set("X-Forwarded-For", "10.10.10.10") // spoofed — must be ignored
	req.Header.Set("X-OpenRung-Client-ID", "client-1")
	req.Header.Set("X-OpenRung-Session-ID", "session-1")
	server.ServeHTTP(httptest.NewRecorder(), req)

	if len(sink.records) != 1 {
		t.Fatalf("expected one client_seen record, got %d", len(sink.records))
	}
	if got := sink.records[0].SourceIP; got != "203.0.113.42" {
		t.Fatalf("untrusted forwarded header must be ignored; got %q", got)
	}
}
