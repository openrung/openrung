// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The control check compares two fetches taken seconds apart. Relay lists are
// re-signed continuously, so comparing raw bytes would fail constantly;
// comparing the relay configuration is what actually answers "did both paths
// reach the same origin". Every field except the volatile envelope timestamps
// counts, because a front serving a stale or different signed configuration is
// exactly what this is for.
func TestSameRelayPayloadIgnoresFreshnessButNotContent(t *testing.T) {
	const signingKey = `"key_id":"627405615601c589"`
	const stamps = `"server_time":"2026-08-03T08:30:00Z","not_after":"2026-08-03T09:00:00Z"`
	relays := func(entries string) []byte {
		return []byte(`{` + signingKey + `,` + stamps + `,"relays":[` + entries + `]}`)
	}
	// A descriptor with the connection-critical fields the narrow comparison
	// this replaced would have ignored entirely.
	relay := func(id, host string, port int, protocol, realityKey string) string {
		return fmt.Sprintf(
			`{"id":%q,"public_host":%q,"public_port":%d,"protocol":%q,"reality_public_key":%q}`,
			id, host, port, protocol, realityKey,
		)
	}
	first := relay("relay-a", "198.51.100.10", 443, "vless", "key-a")
	second := relay("relay-b", "198.51.100.11", 443, "vless", "key-b")

	for _, test := range []struct {
		name  string
		left  []byte
		right []byte
		same  bool
	}{
		{
			name: "identical",
			left: relays(first + "," + second), right: relays(first + "," + second), same: true,
		},
		{
			// Only the envelope moved on. Same origin, later signature.
			// Staleness is caught by relayListAge, not by this comparison.
			name: "re-signed with later timestamps",
			left: relays(first),
			right: []byte(`{` + signingKey +
				`,"server_time":"2026-08-03T08:35:00Z","not_after":"2026-08-03T10:00:00Z","relays":[` + first + `]}`),
			same: true,
		},
		{
			// Ranking is per-request; order carries no identity.
			name: "same relays in a different order",
			left: relays(first + "," + second), right: relays(second + "," + first), same: true,
		},
		{
			// Field order within a descriptor is a JSON encoding detail.
			name:  "same descriptor, fields in a different order",
			left:  relays(`{"id":"relay-a","public_port":443,"public_host":"198.51.100.10"}`),
			right: relays(`{"public_host":"198.51.100.10","id":"relay-a","public_port":443}`),
			same:  true,
		},
		{
			name: "a relay the other path did not return",
			left: relays(first + "," + second), right: relays(first),
		},
		{
			name:  "same id, different host",
			left:  relays(relay("relay-a", "198.51.100.10", 443, "vless", "key-a")),
			right: relays(relay("relay-a", "203.0.113.10", 443, "vless", "key-a")),
		},
		// The three below are the whole point of comparing full descriptors:
		// each would have compared EQUAL under an id+host-only rule while
		// sending clients somewhere different.
		{
			name:  "same relay, different port",
			left:  relays(relay("relay-a", "198.51.100.10", 443, "vless", "key-a")),
			right: relays(relay("relay-a", "198.51.100.10", 8443, "vless", "key-a")),
		},
		{
			name:  "same relay, different protocol",
			left:  relays(relay("relay-a", "198.51.100.10", 443, "vless", "key-a")),
			right: relays(relay("relay-a", "198.51.100.10", 443, "trojan", "key-a")),
		},
		{
			name:  "same relay, different Reality key",
			left:  relays(relay("relay-a", "198.51.100.10", 443, "vless", "key-a")),
			right: relays(relay("relay-a", "198.51.100.10", 443, "vless", "key-b")),
		},
		{
			// A field this code has never heard of still has to count.
			name:  "a field added to the wire schema later",
			left:  relays(`{"id":"relay-a","future_field":"one"}`),
			right: relays(`{"id":"relay-a","future_field":"two"}`),
		},
		{
			// These move on the fleet's own 30s heartbeat, not in response to
			// which front was asked. Comparing them mismatched 31% of adjacent
			// live fetches and made the gate unusable.
			name: "per-relay heartbeat timestamps moved on",
			left: relays(`{"id":"relay-a","public_host":"198.51.100.10",` +
				`"last_heartbeat_at":"2026-08-03T08:30:00Z","expires_at":"2026-08-03T08:33:00Z",` +
				`"registered_at":"2026-08-01T00:00:00Z"}`),
			right: relays(`{"id":"relay-a","public_host":"198.51.100.10",` +
				`"last_heartbeat_at":"2026-08-03T08:30:30Z","expires_at":"2026-08-03T08:33:30Z",` +
				`"registered_at":"2026-08-02T00:00:00Z"}`),
			same: true,
		},
		{
			// Excluding the heartbeat fields must not blind the comparison to a
			// real change on the same descriptor.
			name: "heartbeat moved on AND the port changed",
			left: relays(`{"id":"relay-a","public_port":443,` +
				`"last_heartbeat_at":"2026-08-03T08:30:00Z"}`),
			right: relays(`{"id":"relay-a","public_port":8443,` +
				`"last_heartbeat_at":"2026-08-03T08:30:30Z"}`),
		},
		{
			// Numeric fields are compared as written, never through float64.
			name:  "coordinates that differ only far past float precision",
			left:  relays(`{"id":"relay-a","latitude":35.6895000000000001}`),
			right: relays(`{"id":"relay-a","latitude":35.6895000000000002}`),
		},
		{
			name:  "a different signing key",
			left:  relays(first),
			right: []byte(`{"key_id":"0000000000000000",` + stamps + `,"relays":[` + first + `]}`),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sameRelayPayload(test.left, test.right); got != test.same {
				t.Fatalf("sameRelayPayload = %v, want %v", got, test.same)
			}
		})
	}
}

// Anything that is not a relay list is incomparable, so an error page cannot
// stand in for agreement between the two paths.
func TestSameRelayPayloadRequiresARelayArray(t *testing.T) {
	for _, body := range []string{
		`{"key_id":"627405615601c589"}`,
		`{"key_id":"627405615601c589","relays":null}`,
		`{"key_id":"627405615601c589","relays":{"id":"relay-a"}}`,
		`[]`,
	} {
		if sameRelayPayload([]byte(body), []byte(body)) {
			t.Errorf("%s compared equal to itself", body)
		}
	}
}

// The handshake and the routing probe must test the same origin and the same
// route as the signed fetch. A candidate carrying a port or a base path used to
// be silently rewritten to host:443/api/v1/relays, so the routing check could
// pass on a 404 from a path the real route never uses.
func TestProbeKeepsCandidatePortAndBasePath(t *testing.T) {
	for _, test := range []struct {
		name        string
		candidate   string
		wantAddress string
		wantHost    string
		wantURI     string
	}{
		{
			name:        "root on the default port",
			candidate:   "https://openrung-broker-abcd1234.z01.azurefd.net/",
			wantAddress: "openrung-broker-abcd1234.z01.azurefd.net:443",
			wantHost:    unroutableHost,
			wantURI:     "/api/v1/relays?limit=5",
		},
		{
			name:        "explicit non-default port",
			candidate:   "https://broker.example.org:8443/",
			wantAddress: "broker.example.org:8443",
			wantHost:    unroutableHost + ":8443",
			wantURI:     "/api/v1/relays?limit=5",
		},
		{
			name:        "base path",
			candidate:   "https://broker.example.org/prefix/",
			wantAddress: "broker.example.org:443",
			wantHost:    unroutableHost,
			wantURI:     "/prefix/api/v1/relays?limit=5",
		},
		{
			name:        "port and base path together",
			candidate:   "https://broker.example.org:8443/prefix/",
			wantAddress: "broker.example.org:8443",
			wantHost:    unroutableHost + ":8443",
			wantURI:     "/prefix/api/v1/relays?limit=5",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			endpoint, err := url.Parse(test.candidate)
			if err != nil {
				t.Fatalf("parse candidate: %v", err)
			}
			checker := &frontChecker{brokerURL: test.candidate, endpoint: endpoint, limit: 5}

			if got := checker.address(); got != test.wantAddress {
				t.Errorf("address() = %q, want %q", got, test.wantAddress)
			}
			probe, err := checker.unroutableProbeURL()
			if err != nil {
				t.Fatalf("unroutableProbeURL: %v", err)
			}
			if probe.Host != test.wantHost {
				t.Errorf("probe Host = %q, want %q", probe.Host, test.wantHost)
			}
			if probe.RequestURI() != test.wantURI {
				t.Errorf("probe request URI = %q, want %q", probe.RequestURI(), test.wantURI)
			}
			if probe.Hostname() == endpoint.Hostname() {
				t.Errorf("probe still targets the candidate host %q", probe.Hostname())
			}
		})
	}
}

// Excluding the heartbeat timestamps from the comparison removed the only
// signal that a front is serving a cached body, so freshness is asserted
// directly instead. A cached list still verifies — the signature covers a
// 30-minute window plus 5 minutes of skew — so nothing else here would catch it.
func TestRelayListAgeDetectsACachedBody(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	body := func(serverTime string) []byte {
		return []byte(`{"key_id":"627405615601c589","server_time":"` + serverTime + `","relays":[]}`)
	}

	for _, test := range []struct {
		name       string
		serverTime string
		wantAge    time.Duration
		wantReject bool
	}{
		{name: "signed for this request", serverTime: "2026-08-03T09:00:00Z", wantAge: 0},
		{name: "within tolerance", serverTime: "2026-08-03T08:57:00Z", wantAge: 3 * time.Minute},
		{
			name: "a cached body that still verifies", serverTime: "2026-08-03T08:31:00Z",
			wantAge: 29 * time.Minute, wantReject: true,
		},
		{
			// The four-hour Cloudflare stale-cache incident this guards against.
			name: "the stale-edge case", serverTime: "2026-08-03T05:00:00Z",
			wantAge: 4 * time.Hour, wantReject: true,
		},
		{
			// Ordinary skew in the other direction is tolerated.
			name: "broker clock slightly ahead of ours", serverTime: "2026-08-03T09:00:30Z",
			wantAge: -30 * time.Second,
		},
		{
			// The reason the lower bound exists. A clock this far behind would
			// make a four-hour-old cached list read as zero seconds old, and
			// not_after is relative to the same stale server_time so the
			// signature check passes too. Freshness is simply unknowable here.
			name: "this machine's clock is hours behind", serverTime: "2026-08-03T13:00:00Z",
			wantAge: -4 * time.Hour, wantReject: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			age, err := checkRelayListFreshness(body(test.serverTime), now)
			if age != test.wantAge {
				t.Fatalf("age = %s, want %s", age, test.wantAge)
			}
			if rejected := err != nil; rejected != test.wantReject {
				t.Fatalf("rejected = %v (%v), want %v", rejected, err, test.wantReject)
			}
		})
	}
}

// A body with no server_time cannot be shown to be fresh, so it must be an
// error rather than an implicit pass.
func TestRelayListAgeRequiresServerTime(t *testing.T) {
	for _, body := range []string{
		`{"key_id":"627405615601c589","relays":[]}`,
		`not json at all`,
	} {
		if _, err := checkRelayListFreshness([]byte(body), time.Now()); err == nil {
			t.Errorf("%s was treated as having a known age", body)
		}
	}
}

// The refusal message reaches terminal scrollback and CI logs, so it must name
// the proxy without carrying any part of its credentials.
func TestProxyLocationCarriesNoCredentials(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want string
	}{
		{raw: "https://review-user:review-secret@proxy.invalid", want: "https://proxy.invalid"},
		{raw: "http://token-as-username@proxy.invalid:3128", want: "http://proxy.invalid:3128"},
		{raw: "http://127.0.0.1:3128", want: "http://127.0.0.1:3128"},
		{raw: "socks5://user:pw@127.0.0.1:1080", want: "socks5://127.0.0.1:1080"},
	} {
		t.Run(test.raw, func(t *testing.T) {
			proxy, err := url.Parse(test.raw)
			if err != nil {
				t.Fatalf("parse proxy: %v", err)
			}
			got := proxyLocation(proxy)
			if got != test.want {
				t.Fatalf("proxyLocation = %q, want %q", got, test.want)
			}
			// Belt and braces: whatever the shape, no credential material may
			// survive into the message.
			for _, secret := range []string{"review-secret", "token-as-username", "review-user", ":pw@", "user:"} {
				if strings.Contains(got, secret) {
					t.Fatalf("proxyLocation(%q) = %q leaks %q", test.raw, got, secret)
				}
			}
		})
	}
	if got := proxyLocation(nil); got == "" {
		t.Fatal("a nil proxy produced an empty location")
	}
}

// Two unparseable bodies must not compare equal, or a front answering with an
// error page on both paths would pass the control check.
func TestSameRelayPayloadRejectsUnparseableBodies(t *testing.T) {
	page := []byte("<!DOCTYPE html><html><title>Page not found</title></html>")
	if sameRelayPayload(page, page) {
		t.Fatal("two unparseable bodies compared equal")
	}
}

func TestOneLineCollapsesAndBounds(t *testing.T) {
	if got := oneLine("broker list relays:\n  <!DOCTYPE html>\n\n  <html>"); got != "broker list relays: <!DOCTYPE html> <html>" {
		t.Fatalf("oneLine = %q", got)
	}
	long := oneLine(strings.Repeat("x", 4000))
	if len(long) > 340 || !strings.HasSuffix(long, "(truncated)") {
		t.Fatalf("oneLine did not bound a long message: %d bytes, %q", len(long), long[max(0, len(long)-20):])
	}
}
