package broker

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientDenyListMatching(t *testing.T) {
	list := newClientDenyList([]string{
		" 198.51.100.0/24 ", // padding is trimmed
		"2001:db8:1:2::/64",
		"",           // skipped
		"not-a-cidr", // skipped
	})
	if list == nil {
		t.Fatal("expected a deny list from valid CIDRs")
	}

	for _, tc := range []struct {
		name   string
		source string
		denied bool
	}{
		{"v4 inside", "198.51.100.7", true},
		{"v4 outside", "198.51.101.7", false},
		{"v6 inside", "2001:db8:1:2::aa", true},
		{"v6 sibling prefix", "2001:db8:1:3::aa", false},
		{"v4-mapped v6 inside", "::ffff:198.51.100.7", true},
		{"unparseable is never denied", "not-an-ip", false},
		{"empty is never denied", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := list.denies(tc.source); got != tc.denied {
				t.Fatalf("denies(%q) = %v, want %v", tc.source, got, tc.denied)
			}
		})
	}
}

func TestClientDenyListEmptyConfigIsNil(t *testing.T) {
	for _, cidrs := range [][]string{nil, {}, {"", "   "}, {"garbage"}} {
		if list := newClientDenyList(cidrs); list != nil {
			t.Fatalf("expected nil deny list for %q", cidrs)
		}
	}
	// A nil list denies nothing and leaves the handler untouched.
	var list *clientDenyList
	if list.denies("198.51.100.7") {
		t.Fatal("nil deny list must deny nothing")
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	if got := list.guard(newClientIPResolver(nil), next); got == nil {
		t.Fatal("nil deny list must pass the handler through")
	}
}

func TestRelayListDeniedSourceGetsForbidden(t *testing.T) {
	sink := &memoryTelemetrySink{}
	server := NewServer(NewStore(), Config{
		SigningSeed:     testSigningSeed(),
		TelemetrySink:   sink,
		ClientDenyCIDRs: []string{"2001:db8:1:2::/64"},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/relays?limit=5", nil)
	req.RemoteAddr = "[2001:db8:1:2::aa]:5000"
	req.Header.Set("X-OpenRung-Client-ID", "client-1")
	req.Header.Set("X-OpenRung-Session-ID", "session-1")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a denied source, got %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("a per-client 403 must not be cacheable; got Cache-Control %q", got)
	}
	// The handler never ran, so the denied caller leaves no telemetry behind.
	if len(sink.records) != 0 {
		t.Fatalf("expected no telemetry from a denied source, got %d records", len(sink.records))
	}
}

func TestRelayListAllowedSourceUnaffectedByDenyList(t *testing.T) {
	sink := &memoryTelemetrySink{}
	server := NewServer(NewStore(), Config{
		SigningSeed:     testSigningSeed(),
		TelemetrySink:   sink,
		ClientDenyCIDRs: []string{"2001:db8:1:2::/64"},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/relays?limit=5", nil)
	req.RemoteAddr = "203.0.113.42:5000"
	req.Header.Set("X-OpenRung-Client-ID", "client-1")
	req.Header.Set("X-OpenRung-Session-ID", "session-1")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for an allowed source, got %d", rec.Code)
	}
	if len(sink.records) != 1 {
		t.Fatalf("expected one client_seen record, got %d", len(sink.records))
	}
}

// A denied source is matched on the IP the trusted-proxy chain resolves, not on
// the edge address that carried the request.
func TestDenyListMatchesForwardedClientIP(t *testing.T) {
	server := NewServer(NewStore(), Config{
		SigningSeed:     testSigningSeed(),
		ClientDenyCIDRs: []string{"198.51.100.0/24"},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/relays?limit=5", nil)
	req.RemoteAddr = "104.16.0.5:443" // a trusted Cloudflare edge
	req.Header.Set("CF-Connecting-IP", "198.51.100.7")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a denied forwarded source, got %d", rec.Code)
	}
}

// A forged forwarded header from an untrusted peer must not let a denied source
// slip through by claiming to be someone else.
func TestDenyListIgnoresForwardedHeaderFromUntrustedPeer(t *testing.T) {
	server := NewServer(NewStore(), Config{
		SigningSeed:     testSigningSeed(),
		ClientDenyCIDRs: []string{"203.0.113.0/24"},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/relays?limit=5", nil)
	req.RemoteAddr = "203.0.113.42:5000"          // denied, and not a trusted proxy
	req.Header.Set("X-Forwarded-For", "9.9.9.9")  // spoofed — must be ignored
	req.Header.Set("CF-Connecting-IP", "9.9.9.9") // same
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("a spoofed forwarded header must not bypass the deny list; got %d", rec.Code)
	}
}
