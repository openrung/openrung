// Package config holds the desktop client's broker endpoints and discovery
// tuning. It is the Go analog of the mobile app's src/config.ts (AppConfig):
// the same constant names and values, so the two clients discover relays
// identically.
package config

import (
	"strings"
	"time"
)

const (
	// DefaultBrokerURL is the HTTPS, Cloudflare-fronted discovery endpoint.
	// Discovery runs BEFORE the tunnel is up, so it must be TLS: the relay list
	// seeds the entire VPN config and the request carries the client identity, so
	// a cleartext endpoint would hand both to an on-path censor.
	DefaultBrokerURL = "https://broker.openrung.org/"

	// TelemetryBrokerURL is the endpoint for client telemetry. It must be HTTPS:
	// the first events (BeginSession / connection_attempted) fire BEFORE the
	// tunnel is up, so a cleartext endpoint would expose the persistent client
	// identity to a network observer. Reuses the HTTPS discovery endpoint; a
	// pinned bare-IP fallback can be layered on later if CDN quota is a concern.
	TelemetryBrokerURL = DefaultBrokerURL

	// RelayLimit is the connect-path page size; DirectoryRelayLimit is the
	// broker's maximum page size (larger is rejected with HTTP 400), used to
	// populate the exit-node map.
	RelayLimit          = 5
	DirectoryRelayLimit = 20

	// MaxRecents bounds the main-screen "Recents" row.
	MaxRecents = 8

	// SourceURL is the GPL-3.0 corresponding-source offer surfaced in the
	// in-app licenses screen.
	SourceURL = "https://github.com/openrung/openrung"

	// MinDirectoryRefreshInterval throttles automatic map refreshes so the GUI
	// cannot trip the broker's per-IP rate limit on its own (see broker PR #5).
	MinDirectoryRefreshInterval = 30 * time.Second

	// DiscoveryStagger is the head start each discovery candidate gets over the
	// next one in discovery.FirstReachable's staggered race: candidate[0] starts
	// immediately and, until an attempt succeeds, the next candidate joins every
	// DiscoveryStagger. Long enough that a healthy primary almost always wins
	// outright (so fallback fronts see no extra traffic), short enough that a
	// blocked or hung primary delays discovery by one interval instead of a full
	// request timeout. Must stay in sync with the mobile AppConfig's
	// DISCOVERY_STAGGER_MS so every client races identically.
	DiscoveryStagger = 2500 * time.Millisecond
)

// DefaultBrokerURLs are the ordered discovery candidates. They are raced with
// a staggered start — each entry gets a DiscoveryStagger head start over the
// next, and the first to return relays wins (see discovery.FirstReachable).
//
// Every entry MUST be HTTPS. The relay list is now Ed25519-signed (see
// signing.go), which detaches its authenticity from the transport — but
// discovery still runs BEFORE the tunnel and the client-identity headers ride
// these requests, so a cleartext or bare-IP entry would expose them to an
// on-path censor. EnforceSecureBrokerURL rejects non-HTTPS hosts.
//
// Two independent fronts are deployed: the Cloudflare Worker
// (broker.openrung.org) and an AWS CloudFront distribution on a different
// provider AND DNS zone, so a single CDN/zone/account failure no longer fails
// discovery CLOSED. Both proxy the one signing origin, so both serve signed
// lists. Now that the list is signed, non-TLS / out-of-band channels (signed
// static mirrors, a pinned direct-IP fallback) become safe to add in a later
// step. Keep this list in sync with the mobile clients' AppConfig so every
// client discovers identically.
var DefaultBrokerURLs = []string{
	"https://broker.openrung.org/",
	// Independent second front: AWS CloudFront (different provider + DNS zone).
	"https://d2r7mdpyevvs1m.cloudfront.net/",
}

// Candidates are the ordered discovery endpoints for one request, plus
// whether URLs[0] is a genuine user override. Built by BrokerCandidates and
// consumed by discovery.FirstReachable; carrying the flag alongside the list
// keeps the two from being computed inconsistently.
type Candidates struct {
	URLs []string
	// OverrideFirst marks URLs[0] as a genuine user override — a non-blank
	// primary that is not one of DefaultBrokerURLs. discovery.FirstReachable
	// then tries it strictly first (full per-attempt timeout) and only races
	// the remaining defaults after it fails, so a custom broker that is merely
	// slower than the stagger is never silently outrun by a default front.
	OverrideFirst bool
}

// BrokerCandidates returns the ordered, de-duplicated discovery candidates for
// a request: a genuine primary override first (with OverrideFirst set), then
// the built-in defaults.
//
// Ported from the mobile app's candidates() (src/net/brokerClient.ts): a
// non-blank primary is tried FIRST only when it is a genuine override, i.e.
// not already one of the defaults — and only such an override sets
// OverrideFirst, giving it the strict head phase described on Candidates. A
// persisted value that merely echoes a default must not reorder the defaults'
// HTTPS-first preference (or claim the override phase), otherwise an upgrader
// whose last-used default was the raw IP would keep hitting the IP before the
// Cloudflare-fronted endpoint.
func BrokerCandidates(primary string) Candidates {
	ordered := make([]string, 0, len(DefaultBrokerURLs)+1)
	seen := make(map[string]struct{}, len(DefaultBrokerURLs)+1)
	add := func(value string) {
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		ordered = append(ordered, value)
	}

	overrideFirst := false
	trimmedPrimary := strings.TrimSpace(primary)
	if trimmedPrimary != "" {
		isDefault := false
		for _, fallback := range DefaultBrokerURLs {
			if strings.TrimSpace(fallback) == trimmedPrimary {
				isDefault = true
				break
			}
		}
		if !isDefault {
			add(trimmedPrimary)
			overrideFirst = true
		}
	}
	for _, fallback := range DefaultBrokerURLs {
		if trimmed := strings.TrimSpace(fallback); trimmed != "" {
			add(trimmed)
		}
	}
	return Candidates{URLs: ordered, OverrideFirst: overrideFirst}
}
