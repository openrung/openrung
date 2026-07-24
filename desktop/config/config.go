// Package config holds desktop-specific connection and discovery tuning.
// Shared broker endpoints and candidate ordering come from brokerapi.
package config

import (
	"time"

	"github.com/openrung/openrung/brokerapi"
)

const (
	// DefaultBrokerURL is the HTTPS, Cloudflare-fronted discovery endpoint.
	// Discovery runs BEFORE the tunnel is up, so it must be TLS: the relay list
	// seeds the entire VPN config and the request carries the client identity, so
	// a cleartext endpoint would hand both to an on-path censor.
	DefaultBrokerURL = brokerapi.DefaultBrokerURL

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
	DiscoveryStagger = brokerapi.DefaultDiscoveryStagger

	// RelayTCPTimeout bounds the pre-connect TCP reachability check against a
	// relay's public endpoint (it feeds relay_tcp_ms). Must stay in sync with
	// the mobile RelayReachability.checkTcp timeout so both clients judge
	// reachability identically.
	RelayTCPTimeout = 5 * time.Second

	// Relay ranking: before the ladder runs, the client probes TCP connect
	// latency to the head of the candidate list and reorders it by latency
	// bucket (see vpnservice/ranker.go). Must stay in sync with the mobile
	// RelayRanker constants so all three clients rank identically.
	//
	// RelayRankMaxProbes caps how many relays are probed; the rest keep broker
	// order behind the probed head. RelayRankProbeTimeout is deliberately far
	// shorter than RelayTCPTimeout: this probe only ranks, so it may give up
	// early where the ladder's own 5s reachability gate would still succeed.
	// RelayRankBucketMS is the width, in milliseconds, of the latency bucket
	// within which broker order (and with it the broker's load balancing) still
	// decides.
	RelayRankMaxProbes    = 8
	RelayRankProbeTimeout = 1500 * time.Millisecond
	RelayRankBucketMS     = int64(30)

	// Internet probe: a connect is reported CONNECTED only after an end-to-end
	// HTTP probe through the tunnel succeeds. Sweeps of InternetProbeURLs are
	// retried every InternetProbeRetryDelay until InternetProbeOverallTimeout,
	// each request bounded by InternetProbeRequestTimeout. Must stay in sync
	// with the mobile InternetProbe constants.
	InternetProbeOverallTimeout = 12 * time.Second
	InternetProbeRequestTimeout = 3 * time.Second
	InternetProbeRetryDelay     = 500 * time.Millisecond

	// LadderKillGrace bounds how long a failed connect-ladder candidate's
	// sing-box gets between interrupt and hard kill. Desktop-only: os.Interrupt
	// is unsupported on Windows, so without this every failed candidate's
	// teardown would cost the engine's full 5s default. Proxy mode holds no TUN
	// device, so a hard kill is safe.
	LadderKillGrace = 500 * time.Millisecond

	// TunnelReadyTimeout bounds how long a candidate's sing-box has to bind its
	// mixed inbound before the rung is judged failed (a config or bind error).
	// It replaces a fixed post-launch grace, so a healthy engine that binds in
	// tens of ms is not made to wait the whole window.
	TunnelReadyTimeout = 5 * time.Second

	// MaxRecoveryBackoff caps the Retry-After a rate-limited broker can impose on
	// a mid-session recovery fetch, so a misbehaving or hostile front cannot
	// suspend reconnection (and leave traffic on the normal network) for long.
	MaxRecoveryBackoff = 60 * time.Second

	// Mid-session health monitor (desktop-only; mobile has no equivalent yet):
	// one probe sweep through the tunnel every HealthProbeInterval. After
	// HealthFailureThreshold consecutive failures AND proof the local network
	// is alive (some known relay answers a TCP dial), the tunnel is declared
	// dead and an automatic failover re-ladder runs. The network-alive gate is
	// what keeps a wifi blip or laptop sleep from churning relays.
	HealthProbeInterval    = 30 * time.Second
	HealthFailureThreshold = 3
)

// InternetProbeURLs are the through-tunnel connectivity endpoints, tried in
// order each sweep. Must stay in sync with the mobile InternetProbe ENDPOINTS
// so every client's probe traffic looks identical.
var InternetProbeURLs = []string{
	"https://www.gstatic.com/generate_204",
	"https://cp.cloudflare.com/generate_204",
}

// DefaultBrokerURLs are the ordered discovery candidates. They are raced with
// a staggered start — each entry gets a DiscoveryStagger head start over the
// next, and the first to return relays wins (see discovery.FirstReachable).
//
// Every entry MUST be HTTPS. The relay list is Ed25519-signed (see
// brokerapi/signing.go), which detaches its authenticity from the transport — but
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
var DefaultBrokerURLs = brokerapi.DefaultBrokerURLs()

// Candidates are the ordered discovery endpoints for one request, plus
// whether URLs[0] is a genuine user override. Built by BrokerCandidates and
// consumed by discovery.FirstReachable; carrying the flag alongside the list
// keeps the two from being computed inconsistently.
type Candidates = brokerapi.Candidates

// BrokerCandidates returns the ordered, de-duplicated discovery candidates for
// a request: a genuine primary override first (with OverrideFirst set), then
// the built-in defaults.
//
// The shared policy matches the mobile app's candidates() behavior: a
// non-blank primary is tried FIRST only when it is a genuine override, i.e.
// not already one of the defaults — and only such an override sets
// OverrideFirst, giving it the strict head phase described on Candidates. A
// persisted value that merely echoes a default must not reorder the defaults'
// HTTPS-first preference (or claim the override phase), otherwise an upgrader
// whose last-used default was the raw IP would keep hitting the IP before the
// Cloudflare-fronted endpoint.
func BrokerCandidates(primary string) Candidates {
	return brokerapi.BrokerCandidates(primary)
}
