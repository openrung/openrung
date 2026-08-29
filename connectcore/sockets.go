package connectcore

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/wsscore"
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
// (wsscore.ErrSocketProtectionFailed, the one sentinel every protected dial
// in this repository fails with). It is a local platform failure — the
// VpnService is gone, the tunnel adapter is mid-teardown: no relay, front, or
// broker is evidence-worthy, no fallback transport or retry can repair it, so
// ladder paths stop through localCandidateError instead of denting relay
// health or minting WSS tickets.
func isSocketProtectionFailure(err error) bool {
	return errors.Is(err, wsscore.ErrSocketProtectionFailed)
}

// protectedNetDialer returns the dialer for one of the engine's raw TCP
// probes: plain without a protector, else carrying the protector's control
// and resolver.
func (s *Engine) protectedNetDialer(timeout time.Duration) *net.Dialer {
	protector := s.SocketProtector
	return &net.Dialer{
		Timeout:  timeout,
		Control:  wsscore.SocketControl(protector),
		Resolver: wsscore.ProtectedResolver(protector),
	}
}

// protectedTransport is a default-shaped HTTP transport over a protected
// dialer, for the engine's plain (non-broker) HTTP: the geo lookup and the
// hub punch coordination.
func protectedTransport(protector wsscore.SocketProtector) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   wsscore.SocketControl(protector),
		Resolver:  wsscore.ProtectedResolver(protector),
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
	broker, _, _ := s.protectedHTTP.ensure(s.SocketProtector, s.PunchInsecure)
	return broker
}

// geoHTTPClient returns the client for the public-IP geo lookup: nil without
// a protector (clienttelemetry.LookupGeoAttributes builds its own 4s-timeout
// default), else the same shape protected.
func (s *Engine) geoHTTPClient() *http.Client {
	_, geo, _ := s.protectedHTTP.ensure(s.SocketProtector, s.PunchInsecure)
	return geo
}

// punchCoordinationClient returns the client for the hub punch coordination
// API: nil without a protector (AttemptPunch then keeps its insecure-aware
// default), else a protected client that honors PunchInsecure — see
// punchHTTPClient for why skipping hub TLS verification stays safe.
func (s *Engine) punchCoordinationClient() *http.Client {
	_, _, punch := s.protectedHTTP.ensure(s.SocketProtector, s.PunchInsecure)
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
	mu        sync.Mutex
	protector wsscore.SocketProtector
	broker    *http.Client
	geo       *http.Client
	punch     *http.Client
}

func (c *protectedHTTPClients) ensure(protector wsscore.SocketProtector, punchInsecure bool) (broker, geo, punch *http.Client) {
	if protector == nil {
		return nil, nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broker != nil && sameProtector(c.protector, protector) {
		return c.broker, c.geo, c.punch
	}
	for _, replaced := range []*http.Client{c.broker, c.geo, c.punch} {
		if replaced != nil {
			replaced.CloseIdleConnections()
		}
	}
	c.protector = protector
	// brokerapi builds its own protected resolver from the control, keeping
	// its ECH/no-SNI dial behavior.
	c.broker = brokerapi.NewHTTPClientWithDialControl(0, wsscore.SocketControl(protector))
	c.geo = &http.Client{
		// LookupGeoAttributes' own default timeout, kept in lockstep.
		Timeout:   4 * time.Second,
		Transport: protectedTransport(protector),
	}
	punchTransport := protectedTransport(protector)
	if punchInsecure {
		punchTransport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // same opt-in as punchHTTPClient; data path independently secured
	}
	c.punch = &http.Client{
		// punchcore.HubClient's own default timeout, kept in lockstep.
		Timeout:   10 * time.Second,
		Transport: punchTransport,
	}
	return c.broker, c.geo, c.punch
}

// sameProtector mirrors sameWriter: comparing interfaces with uncomparable
// dynamic types panics, which must read as "different", not crash the engine.
func sameProtector(a, b wsscore.SocketProtector) (same bool) {
	defer func() { _ = recover() }()
	return a == b
}
