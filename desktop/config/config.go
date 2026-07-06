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
	// Discovery runs BEFORE the tunnel is up, so TLS + CDN edge IPs make it
	// costly to block; the raw-IP fallback in DefaultBrokerURLs covers a
	// blocked edge.
	DefaultBrokerURL = "https://broker.openrung.org/"

	// TelemetryBrokerURL is the raw origin IP. Heartbeats fire ~once/minute
	// per connected user and ride the established tunnel, so they skip the CDN
	// front to avoid burning the Workers free-tier quota.
	TelemetryBrokerURL = "http://54.238.185.205:8080/"

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
)

// DefaultBrokerURLs are the ordered discovery candidates: Cloudflare-fronted
// endpoint first, raw IP as a fallback so a blocked edge never takes discovery
// offline.
var DefaultBrokerURLs = []string{
	"https://broker.openrung.org/",
	"http://54.238.185.205:8080/",
}

// BrokerCandidates returns the ordered, de-duplicated discovery candidates for
// a request: a genuine primary override first, then the built-in defaults.
//
// Ported from the mobile app's candidates() (src/net/brokerClient.ts): a
// non-blank primary is tried FIRST only when it is a genuine override, i.e.
// not already one of the defaults. A persisted value that merely echoes a
// default must not reorder the defaults' HTTPS-first preference, otherwise an
// upgrader whose last-used default was the raw IP would keep hitting the IP
// before the Cloudflare-fronted endpoint.
func BrokerCandidates(primary string) []string {
	ordered := make([]string, 0, len(DefaultBrokerURLs)+1)
	seen := make(map[string]struct{}, len(DefaultBrokerURLs)+1)
	add := func(value string) {
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		ordered = append(ordered, value)
	}

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
		}
	}
	for _, fallback := range DefaultBrokerURLs {
		if trimmed := strings.TrimSpace(fallback); trimmed != "" {
			add(trimmed)
		}
	}
	return ordered
}
