package connectcore

// Engine connection and discovery tuning, moved verbatim from desktop/config
// (ADR-001 PR A1) so the engine carries its own policy. Shared broker
// endpoints and candidate ordering come from brokerapi.

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

	// MinDirectoryRefreshInterval throttles automatic map refreshes so the GUI
	// cannot trip the broker's per-IP rate limit on its own (see broker PR #5).
	MinDirectoryRefreshInterval = 30 * time.Second

	// RelayTCPTimeout bounds the pre-connect TCP reachability check against a
	// relay's public endpoint (it feeds relay_tcp_ms). Must stay in sync with
	// the mobile RelayReachability.checkTcp timeout so both clients judge
	// reachability identically.
	RelayTCPTimeout = 5 * time.Second

	// Relay ranking: before the ladder runs, the client probes TCP connect
	// latency to the head of the candidate list and reorders it by latency
	// bucket (see ranker.go). Must stay in sync with the mobile RelayRanker
	// constants so all three clients rank identically.
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
	// sing-box gets between interrupt and hard kill. os.Interrupt is
	// unsupported on Windows, so without this every failed candidate's
	// teardown would cost the engine's full 5s default. Proxy mode holds no TUN
	// device, so a hard kill is safe.
	LadderKillGrace = 500 * time.Millisecond

	// TUNKillGrace replaces LadderKillGrace in TUN mode. A TUN candidate holds
	// a tunnel device plus the routes and DNS settings sing-box installed for
	// it, and only sing-box's own interrupt handling puts those back. A hard
	// kill after half a second would leave the host routing traffic at an
	// interface that no longer exists, so the interrupt gets the room it needs.
	//
	// This grace is only worth anything where the interrupt is delivered at
	// all. A host on a platform where it cannot stop the tunnel process
	// gracefully must refuse TUN mode in its Elevation hook rather than rely on
	// this window — see the terminal client's Windows implementation.
	TUNKillGrace = 5 * time.Second

	// TunnelReadyTimeout bounds how long a candidate's sing-box has to bind its
	// mixed inbound before the rung is judged failed (a config or bind error).
	// It replaces a fixed post-launch grace, so a healthy engine that binds in
	// tens of ms is not made to wait the whole window.
	TunnelReadyTimeout = 5 * time.Second

	// TUNTunnelReadyTimeout replaces TunnelReadyTimeout in TUN mode, where
	// readiness is a higher bar than binding a socket: sing-box has to create
	// the tunnel device and install the routes (on Linux, policy rules and a
	// routing table) that make it the default path, and only then does the
	// engine consider the rung up. See tunInterfaceReady for why the routes,
	// not the device, are what readiness tests.
	TUNTunnelReadyTimeout = 15 * time.Second

	// MaxRecoveryBackoff caps the Retry-After a rate-limited broker can impose on
	// a mid-session recovery fetch, so a misbehaving or hostile front cannot
	// suspend reconnection (and leave traffic on the normal network) for long.
	MaxRecoveryBackoff = 60 * time.Second

	// Mid-session health monitor; mobile clients own separate native health
	// and recovery policies. One probe sweep through the tunnel runs every
	// HealthProbeInterval. After HealthFailureThreshold consecutive failures AND
	// proof the local network is alive (some known relay answers a TCP dial), the
	// tunnel is declared dead and an automatic failover re-ladder runs. The
	// network-alive gate is what keeps a wifi blip or laptop sleep from churning
	// relays.
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

// DefaultBrokerURLs are the ordered discovery candidates. Exact-endpoint fronts
// are raced with a staggered start — each entry gets a stagger head start over
// the next, and the first to return relays wins. An endpoint-unbound front is
// a separate fallback phase and does not start until every stronger candidate
// has failed (see discovery.FirstReachable).
//
// Every entry MUST be HTTPS. The relay list is Ed25519-signed (see
// brokerapi/signing.go), which detaches its authenticity from the transport — but
// discovery still runs BEFORE the tunnel and the client-identity headers ride
// these requests, so a cleartext or bare-IP entry would expose them to an
// on-path censor. EnforceSecureBrokerURL rejects non-HTTPS hosts.
//
// Three independent fronts are deployed. The Cloudflare Worker
// (broker.openrung.org) and AWS CloudFront distribution both authenticate the
// exact endpoint and race first. Azure Front Door authenticates only a shared
// Azure edge, so it is attempted only after both stronger fronts fail. All
// three proxy the one signing origin and serve signed lists. brokerapi owns both
// this list and its two-phase trust policy; mobile's native binding consumes the
// same functions rather than duplicating either in platform AppConfig.
var DefaultBrokerURLs = brokerapi.DefaultBrokerURLs()
