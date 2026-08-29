package connectcore

import (
	"crypto/tls"
	"errors"
	"math"
	"net"
	"net/http"
	"sync"
	"syscall"
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
// relay discovery, WSS session tickets, telemetry uploads and the geo lookup,
// hub punch coordination, relay TCP reachability and the ranker's latency
// probes, the network-alive gate, and the WSS bridge's CDN dial. It is
// deliberately NOT applied to through-tunnel traffic (the end-to-end internet
// probe and the mid-session health sweeps must ride the tunnel — that is what
// they verify) or to loopback dials, and the punched QUIC data path protects
// its own sockets inside the host's PunchEstablisher.

// errSocketProtectionFailed fails a dial whose socket the host refused to
// protect. Failing closed is the point: an unprotected socket would not error
// on its own — it would silently route into the tunnel.
var errSocketProtectionFailed = errors.New("socket protection failed")

// isSocketProtectionFailure recognizes the host's refusal to protect a socket
// — the engine's own sentinel on raw dials, wsscore's on the WSS path. It is
// a local platform failure (the VpnService is gone, the tunnel adapter is
// mid-teardown): no relay or front is evidence-worthy, no fallback transport
// can repair it, so the ladder must stop through localCandidateError instead
// of denting relay health or minting WSS tickets.
func isSocketProtectionFailure(err error) bool {
	return errors.Is(err, errSocketProtectionFailed) || errors.Is(err, wsscore.ErrSocketProtectionFailed)
}

// protectorDialControl adapts the fd-level protector into a net.Dialer
// Control, with wsscore's semantics: an fd the protector cannot represent or
// refuses fails the dial.
func protectorDialControl(protector wsscore.SocketProtector) func(network, address string, conn syscall.RawConn) error {
	if protector == nil {
		return nil
	}
	return func(network, address string, raw syscall.RawConn) error {
		var protectErr error
		if err := raw.Control(func(fd uintptr) {
			if fd > math.MaxInt32 || !protector.Protect(int32(fd)) {
				protectErr = errSocketProtectionFailed
			}
		}); err != nil {
			return err
		}
		return protectErr
	}
}

// dialControl is the engine's resolved dial control: nil without a protector,
// so every default below stays byte-identical for desktop and the TUI.
func (s *Engine) dialControl() func(network, address string, conn syscall.RawConn) error {
	return protectorDialControl(s.SocketProtector)
}

// protectedTransport is a default-shaped HTTP transport over a protected
// dialer, for the engine's plain (non-broker) HTTP: the geo lookup and the
// hub punch coordination.
func protectedTransport(control func(network, address string, conn syscall.RawConn) error) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   control,
	}).DialContext
	return transport
}

// brokerHTTPClient returns the client for the engine's broker traffic —
// discovery, WSS session tickets, telemetry uploads. Nil without a protector:
// each consumer then selects brokerapi's shared ECH-capable default exactly
// as before. With one, it is the same default shape over a protected dialer,
// built once so the engine's broker requests share a connection pool the way
// the package default does.
func (s *Engine) brokerHTTPClient() *http.Client {
	control := s.dialControl()
	if control == nil {
		return nil
	}
	s.protectedBrokerOnce.Do(func() {
		s.protectedBroker = brokerapi.NewHTTPClientWithDialControl(0, control)
	})
	return s.protectedBroker
}

// geoHTTPClient returns the client for the public-IP geo lookup: nil without
// a protector (clienttelemetry.LookupGeoAttributes builds its own 4s-timeout
// default), else the same shape over a protected dialer.
func (s *Engine) geoHTTPClient() *http.Client {
	control := s.dialControl()
	if control == nil {
		return nil
	}
	return &http.Client{
		// LookupGeoAttributes' own default timeout, kept in lockstep.
		Timeout:   4 * time.Second,
		Transport: protectedTransport(control),
	}
}

// punchCoordinationClient returns the client for the hub punch coordination
// API: nil without a protector (AttemptPunch then keeps its insecure-aware
// default), else a protected client that still honors PunchInsecure — see
// punchHTTPClient for why skipping hub TLS verification stays safe.
func (s *Engine) punchCoordinationClient() *http.Client {
	control := s.dialControl()
	if control == nil {
		return nil
	}
	transport := protectedTransport(control)
	if s.PunchInsecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // same opt-in as punchHTTPClient; data path independently secured
	}
	return &http.Client{
		// punchcore.HubClient's own default timeout, kept in lockstep.
		Timeout:   10 * time.Second,
		Transport: transport,
	}
}

// protectedBrokerState carries the lazily built shared broker client (see
// brokerHTTPClient). Separate struct so Engine embeds one field.
type protectedBrokerState struct {
	protectedBrokerOnce sync.Once
	protectedBroker     *http.Client
}
