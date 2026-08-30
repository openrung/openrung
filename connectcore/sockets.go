package connectcore

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/wsscore"

	"github.com/openrung/openrung/connectcore/clienttelemetry"
)

// This file owns the socket-control seam (ADR-003 A2): on a host whose tunnel
// captures the whole device (Android VpnService, and in-process libbox in
// general), every socket the engine itself opens toward the physical network
// must be excluded from that capture, or the engine's own traffic — the
// broker request that discovers relays foremost — is routed into the very
// tunnel it is trying to establish or repair. The hook is
// wsscore.SocketProtector, the fd-level shape the shared modules already
// define for exactly this; desktop and the TUI leave it nil and every
// construction below collapses to the exact client it built before the seam.
//
// Scope: the protector applies to the engine's PHYSICAL-network sockets —
// relay discovery (the connect path and the directory/map listings alike),
// WSS session tickets, telemetry uploads and the geo lookup, hub punch
// coordination, relay TCP reachability and the ranker's latency probes, the
// network-alive gate, and the WSS bridge's CDN dial — including each dial's
// DNS resolution: protected dialers carry wsscore.ProtectedResolver, because
// Control alone covers only the final connection socket while the resolver's
// own query sockets (or the out-of-process getaddrinfo path) would blackhole
// into the live tunnel just the same. It is deliberately NOT applied to
// through-tunnel traffic (the end-to-end internet probe and the mid-session
// health sweeps must ride the tunnel — that is what they verify) or to
// loopback dials, and the punched QUIC data path protects its own sockets
// inside the host's PunchEstablisher.
//
// A refused protection fails the dial closed with
// wsscore.ErrSocketProtectionFailed — an unprotected socket would not error
// on its own, it would silently route into the tunnel.

// isSocketProtectionFailure recognizes the host's refusal to protect a socket
// (wsscore's sentinel, matched across the *net.DNSError boundary a protected
// resolver introduces on hostname dials — see wsscore.IsSocketProtectionFailed;
// a bare errors.Is loses the refusal there). It is a local platform failure —
// the VpnService is gone, the tunnel adapter is mid-teardown: no relay, front,
// or broker is evidence-worthy, no fallback transport or retry can repair it,
// so ladder paths stop through localCandidateError instead of denting relay
// health or minting WSS tickets.
func isSocketProtectionFailure(err error) bool {
	return wsscore.IsSocketProtectionFailed(err)
}

// SetSocketProtector replaces the protector after Start — the one hook that
// legitimately changes mid-life: an adapter that keeps one Engine across a
// VpnService recreate must protect new sockets with the new service, and the
// protector-derived clients rebuild accordingly (see protectedHTTPClients).
// Safe for concurrent use; passing nil removes protection. The initial
// protector may still be assigned to the SocketProtector field before Start,
// like every other hook — but only this setter may change it once the engine
// is running.
func (s *Engine) SetSocketProtector(protector wsscore.SocketProtector) {
	s.protectorMu.Lock()
	s.protectorReplaced = true
	s.protectorOverride = protector
	s.protectorMu.Unlock()
}

// SetDNSServers supplies the physical network's nameservers (host:port or
// bare IP) for the protected resolver: on a host with a protector, hostname
// dials resolve through a pure-Go resolver whose query sockets are protected,
// and that resolver needs a query target the platform must provide (Android:
// LinkProperties DNS — no /etc/resolv.conf exists there). Without servers,
// resolution stays on the platform resolver — working, but with only the
// final connection socket protected. Safe for concurrent use; adapters update
// it from the same connectivity callbacks that feed UpdateNetworkState.
func (s *Engine) SetDNSServers(servers []string) {
	copied := append([]string(nil), servers...)
	s.protectorMu.Lock()
	s.dnsServers = copied
	s.protectorMu.Unlock()
}

// currentProtector resolves the live protector: the SetSocketProtector value
// once one was set, else the construction-time field.
func (s *Engine) currentProtector() wsscore.SocketProtector {
	s.protectorMu.Lock()
	defer s.protectorMu.Unlock()
	if s.protectorReplaced {
		return s.protectorOverride
	}
	return s.SocketProtector
}

func (s *Engine) currentDNSServers() []string {
	s.protectorMu.Lock()
	defer s.protectorMu.Unlock()
	return s.dnsServers
}

// protectedNetDialer returns the dialer for one of the engine's raw TCP
// probes: plain without a protector, else carrying the protector's control
// and — when the host supplied nameservers — its protected resolver.
func (s *Engine) protectedNetDialer(timeout time.Duration) *net.Dialer {
	protector := s.currentProtector()
	return &net.Dialer{
		Timeout:  timeout,
		Control:  wsscore.SocketControl(protector),
		Resolver: wsscore.ProtectedResolver(protector, s.currentDNSServers()),
	}
}

// protectedTransport is a default-shaped HTTP transport over a protected
// dialer, for the engine's plain (non-broker) HTTP: the geo lookup and the
// hub punch coordination.
func protectedTransport(protector wsscore.SocketProtector, dnsServers []string) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   wsscore.SocketControl(protector),
		Resolver:  wsscore.ProtectedResolver(protector, dnsServers),
	}).DialContext
	return transport
}

// brokerHTTPClient returns the client for the engine's broker traffic —
// discovery (connect and directory listings), WSS session tickets, telemetry
// uploads. Nil without a protector: each consumer then selects brokerapi's
// shared ECH-capable default exactly as before. With one, it is the same
// default shape over a protected dialer and resolver, cached so the engine's
// broker requests share a connection pool the way the package default does.
func (s *Engine) brokerHTTPClient() *http.Client {
	broker, _, _ := s.protectedHTTP.ensure(s.currentProtector(), s.currentDNSServers(), s.PunchInsecure)
	return broker
}

// geoHTTPClient returns the client for the public-IP geo lookup: nil without
// a protector (clienttelemetry.LookupGeoAttributes builds its own 4s-timeout
// default), else the same shape protected.
func (s *Engine) geoHTTPClient() *http.Client {
	_, geo, _ := s.protectedHTTP.ensure(s.currentProtector(), s.currentDNSServers(), s.PunchInsecure)
	return geo
}

// punchCoordinationClient returns the client for the hub punch coordination
// API: nil without a protector (AttemptPunch then keeps its insecure-aware
// default), else a protected client that honors PunchInsecure — see
// punchHTTPClient for why skipping hub TLS verification stays safe.
func (s *Engine) punchCoordinationClient() *http.Client {
	_, _, punch := s.protectedHTTP.ensure(s.currentProtector(), s.currentDNSServers(), s.PunchInsecure)
	return punch
}

// protectedHTTPClients caches the protector-derived HTTP clients so pooled
// connections are reused across calls and sessions, rebuilt — releasing the
// previous set's idle connections — when the protector changes: an adapter
// that keeps one Engine across a VpnService recreate hands it a new
// protector, and a latched first one would protect sockets with a dead
// service forever. (PunchInsecure is read at build time; like every engine
// option it must be set before Start.)
type protectedHTTPClients struct {
	mu         sync.Mutex
	protector  wsscore.SocketProtector
	dnsServers string // the built set's servers, joined, for change detection
	broker     *http.Client
	geo        *http.Client
	punch      *http.Client
}

func (c *protectedHTTPClients) ensure(protector wsscore.SocketProtector, dnsServers []string, punchInsecure bool) (broker, geo, punch *http.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if protector == nil {
		// Back to the module defaults — and release the replaced set's pools
		// rather than pinning clients built for a protector that is gone.
		c.releaseLocked()
		return nil, nil, nil
	}
	serversKey := strings.Join(dnsServers, ",")
	if c.broker != nil && sameProtector(c.protector, protector) && c.dnsServers == serversKey {
		return c.broker, c.geo, c.punch
	}
	c.releaseLocked()
	c.protector = protector
	c.dnsServers = serversKey
	// brokerapi keeps its ECH/no-SNI dial behavior over the hook and the
	// prebuilt protected resolver (it cannot import its sibling module).
	c.broker = brokerapi.NewHTTPClientWithDialControl(
		0,
		wsscore.SocketControl(protector),
		wsscore.ProtectedResolver(protector, dnsServers),
	)
	c.geo = &http.Client{
		Timeout:   clienttelemetry.GeoLookupHTTPTimeout,
		Transport: protectedTransport(protector, dnsServers),
	}
	// punchHTTPClient owns the timeout and the (audit-sensitive) insecure TLS
	// shape; only the transport is ours.
	c.punch = punchHTTPClient(punchInsecure, protectedTransport(protector, dnsServers))
	return c.broker, c.geo, c.punch
}

// releaseLocked closes the cached set's idle pools and clears it. Caller
// holds mu.
func (c *protectedHTTPClients) releaseLocked() {
	for _, replaced := range []*http.Client{c.broker, c.geo, c.punch} {
		if replaced != nil {
			replaced.CloseIdleConnections()
		}
	}
	c.protector = nil
	c.dnsServers = ""
	c.broker, c.geo, c.punch = nil, nil, nil
}

// sameProtector mirrors sameWriter: comparing interfaces with uncomparable
// dynamic types panics, which must read as "different", not crash the engine.
func sameProtector(a, b wsscore.SocketProtector) (same bool) {
	defer func() { _ = recover() }()
	return a == b
}
