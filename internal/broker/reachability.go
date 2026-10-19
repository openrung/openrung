// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"syscall"
	"time"

	"openrung/internal/relay"
)

// Inbound reachability probe.
//
// A direct-transport relay registers and heartbeats over outbound connections
// it opens itself, so a relay whose advertised public_host:public_port accepts
// no inbound connection at all (a firewall, a NAT without a port map, a
// listener bound to the wrong address) still looks healthy to the broker
// while every client attempt against it times out. The broker therefore dials
// the advertised endpoint itself: once when the relay registers, and again on
// a heartbeat once the previous result is older than RelayProbeInterval. The
// result is stored on the descriptor; a relay whose latest probe failed is
// left out of the public directory and of access answers until a later probe
// succeeds. It stays registered, heartbeats normally, and remains visible to
// operators (inventory, relays page) with the result.
//
// Only direct transport is probed: a tunnel relay's advertised endpoint is the
// relay hub, which fronts it precisely because the relay itself is not
// reachable. A probe that cannot be attempted from this broker — no route or
// no local address for the endpoint's family, a transient resolver failure,
// the registering relay hanging up — is inconclusive and changes nothing: the
// relay keeps whatever state it had, which for a fresh registration is
// "unprobed" and listed.

const (
	// defaultRelayProbeTimeout bounds one dial. Reality listeners answer a
	// TCP connect immediately, so anything near this is a black hole.
	defaultRelayProbeTimeout = 5 * time.Second
	// defaultRelayProbeInterval is how old a stored result may be before a
	// heartbeat re-probes. It bounds both how long a relay that went dark
	// stays listed and how long a repaired one stays delisted.
	defaultRelayProbeInterval = 5 * time.Minute
)

// ErrProbeInconclusive marks a probe that could not be attempted from this
// broker, as opposed to one the endpoint failed. Probers wrap it so the
// handler leaves the stored result untouched instead of recording a failure.
var ErrProbeInconclusive = errors.New("reachability probe inconclusive")

// RelayProber checks that a relay's advertised endpoint accepts inbound
// connections. A nil error means reachable; an error wrapping
// ErrProbeInconclusive means the probe could not be attempted; any other
// error means the endpoint refused or ignored the connection.
type RelayProber interface {
	Probe(ctx context.Context, host string, port int) error
}

// TCPRelayProber dials the endpoint with a plain bounded TCP connect. A
// completed handshake is the whole test: it proves the path and the listener
// exist, which is exactly what the failure mode above lacks, without the
// broker having to speak Reality.
type TCPRelayProber struct {
	timeout time.Duration
}

// NewTCPRelayProber returns a prober whose dials time out after timeout
// (defaultRelayProbeTimeout when zero or negative).
func NewTCPRelayProber(timeout time.Duration) *TCPRelayProber {
	if timeout <= 0 {
		timeout = defaultRelayProbeTimeout
	}
	return &TCPRelayProber{timeout: timeout}
}

func (p *TCPRelayProber) Probe(ctx context.Context, host string, port int) error {
	dialer := net.Dialer{Timeout: p.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return classifyProbeError(err)
	}
	_ = conn.Close()
	return nil
}

// classifyProbeError separates "the endpoint did not answer" from "this
// broker could not ask". The latter is the broker's own condition — no route
// or local address for the endpoint's address family (a broker without IPv6
// egress must not delist every IPv6 relay), a transient resolver failure, or
// the caller's context being cancelled — and is reported as inconclusive.
func classifyProbeError(err error) error {
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, context.Canceled),
		errors.Is(err, syscall.ENETUNREACH),
		errors.Is(err, syscall.EADDRNOTAVAIL),
		errors.As(err, &dnsErr) && (dnsErr.IsTemporary || dnsErr.IsTimeout):
		return fmt.Errorf("%w: %w", ErrProbeInconclusive, err)
	default:
		return err
	}
}

// relayProbeDue reports whether desc should be probed now: never for a
// non-direct transport, always when it has no result yet, otherwise once the
// stored result is interval old.
func relayProbeDue(desc relay.Descriptor, now time.Time, interval time.Duration) bool {
	if desc.Transport != relay.TransportDirect {
		return false
	}
	if desc.ReachabilityCheckedAt.IsZero() {
		return true
	}
	return now.Sub(desc.ReachabilityCheckedAt) >= interval
}

// probeRelayReachability runs a due probe against desc's advertised endpoint
// and persists the outcome, updating desc in place. Like resolveRelayGeo it
// never fails the surrounding request: a store error or an inconclusive probe
// leaves the stored result as it was. The write is scoped to this
// registration's lease token so a result landing after a re-registration
// cannot describe the replacement descriptor.
func probeRelayReachability(ctx context.Context, store RelayStore, prober RelayProber, interval time.Duration, desc *relay.Descriptor, now time.Time) {
	if prober == nil || !relayProbeDue(*desc, now, interval) {
		return
	}
	err := prober.Probe(ctx, desc.PublicHost, desc.PublicPort)
	if errors.Is(err, ErrProbeInconclusive) {
		slog.Warn("relay reachability probe inconclusive", "relay_id", desc.ID, "public", desc.PublicHost, "port", desc.PublicPort, "error", err)
		return
	}
	reachable := err == nil
	if err := store.UpdateReachability(desc.ID, desc.LeaseToken, reachable, now); err != nil {
		if !errors.Is(err, ErrRelayNotFound) {
			slog.Warn("could not store relay reachability", "relay_id", desc.ID, "error", err)
		}
		return
	}
	desc.Reachable = reachable
	desc.ReachabilityCheckedAt = now
	if reachable {
		return
	}
	slog.Warn("relay endpoint unreachable, withheld from clients", "relay_id", desc.ID, "node_class", desc.NodeClass, "public", desc.PublicHost, "port", desc.PublicPort, "error", err)
}

// relayUnreachable reports whether desc's most recent probe failed. A relay
// that has never been probed (tunnel transport, probing disabled, or a result
// not yet written) is not unreachable.
func relayUnreachable(desc relay.Descriptor) bool {
	return !desc.ReachabilityCheckedAt.IsZero() && !desc.Reachable
}

// withoutUnreachable drops relays whose latest probe failed, preserving the
// ranking order of the rest. The client-facing channels apply it after
// ranking and before page shaping; the operator views never do.
func withoutUnreachable(relays []relay.Descriptor) []relay.Descriptor {
	out := relays[:0:0]
	for _, desc := range relays {
		if relayUnreachable(desc) {
			continue
		}
		out = append(out, desc)
	}
	return out
}

// reachablePointer is the three-valued operator view of a probe result: nil
// when the relay has not been probed, otherwise the latest outcome.
func reachablePointer(desc relay.Descriptor) *bool {
	if desc.ReachabilityCheckedAt.IsZero() {
		return nil
	}
	reachable := desc.Reachable
	return &reachable
}

// reachabilityLogValue renders the same three states for a log attribute.
func reachabilityLogValue(desc relay.Descriptor) any {
	if reachable := reachablePointer(desc); reachable != nil {
		return *reachable
	}
	return "unprobed"
}
