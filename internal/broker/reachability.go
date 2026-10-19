// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
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
// the advertised endpoint itself. At registration the probe gates admission:
// a conclusive failure is refused with the reason (422), nothing is stored,
// and the relay's own registration retries re-probe it until the port opens
// — the relay logs the refusal verbatim, which is how the volunteer installer
// tells its operator which firewall to open. The refusal deliberately
// carries no Retry-After: the relay would honour one between its three
// silent startup attempts, delaying the logged refusal the installer waits
// for; its supervisor already backs off between attempt rounds. A relay
// that registered reachable is re-probed on a heartbeat once its result is
// older than RelayProbeInterval; one whose latest probe then failed is left
// out of the public directory and of access answers until a later probe
// succeeds, staying registered and visible to operators (inventory, relays
// page) with the result.
//
// Only direct transport is probed: a tunnel relay's advertised endpoint is the
// relay hub, which fronts it precisely because the relay itself is not
// reachable. A probe that cannot be attempted from this broker — no route or
// no local address for the endpoint's family, a transient resolver failure,
// the registering relay hanging up — is inconclusive and changes nothing: the
// relay keeps whatever state it had, which for a fresh registration is
// "unprobed" and listed.
//
// The endpoint is relay-supplied, and registration may be anonymous, so the
// broker dials only addresses a client on the public internet could reach
// itself: the host is resolved first and every resolved address must be
// public unicast and not one of this host's own (see probeAddressPermitted).
// Anything else is not dialled and is inconclusive, so a registration can
// never be used to learn whether a loopback, private, link-local or
// broker-local port accepts connections.
//
// The withholding is a consequence of probing: with probing disabled the
// stored results are ignored, so switching it off relists every relay at
// once even when a retained store still holds earlier failures.

const (
	// defaultRelayProbeTimeout bounds one dial. Reality listeners answer a
	// TCP connect immediately, so anything near this is a black hole.
	defaultRelayProbeTimeout = 5 * time.Second
	// defaultRelayProbeInterval is how old a stored result may be before a
	// heartbeat re-probes. It bounds both how long a relay that went dark
	// stays listed and how long a repaired one stays delisted.
	defaultRelayProbeInterval = 5 * time.Minute
	// maxProbeAddresses caps how many resolved addresses one probe dials, so
	// a name with a very long answer cannot turn a registration into a fan-out.
	maxProbeAddresses = 8
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
	// permit decides which resolved addresses may be dialled; it is
	// probeAddressPermitted outside tests.
	permit func(netip.Addr) bool
	// resolve turns the advertised host into candidate addresses; it is the
	// default resolver outside tests.
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
}

// NewTCPRelayProber returns a prober whose dials time out after timeout
// (defaultRelayProbeTimeout when zero or negative).
func NewTCPRelayProber(timeout time.Duration) *TCPRelayProber {
	if timeout <= 0 {
		timeout = defaultRelayProbeTimeout
	}
	return &TCPRelayProber{
		timeout: timeout,
		permit:  probeAddressPermitted,
		resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
	}
}

// Probe resolves host, drops every address the policy refuses, and dials the
// rest by address (never by name again, so a resolver cannot swap in a
// refused address between the check and the dial). The permitted addresses
// are dialled concurrently under one deadline — the whole probe, resolution
// included, runs inside a relay's request — so a slow or dead first address
// neither hides a working second one nor spends the budget the other family
// needs. The first completed connection is reachable and ends the probe.
// Otherwise the endpoint is unreachable if any dial failed conclusively, and
// inconclusive if nothing could be dialled or every dial failed for a reason
// of this broker's own.
func (p *TCPRelayProber) Probe(ctx context.Context, host string, port int) error {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	addrs, err := p.targets(ctx, host)
	if err != nil {
		return err
	}
	// Buffered to len(addrs): a dial finishing after the probe has returned
	// writes its result and exits instead of blocking forever.
	results := make(chan error, len(addrs))
	for _, addr := range addrs {
		go func(addr netip.Addr) {
			dialer := net.Dialer{}
			conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(addr.String(), strconv.Itoa(port)))
			if err == nil {
				_ = conn.Close()
				results <- nil
				return
			}
			results <- classifyProbeError(err)
		}(addr)
	}
	var failure error
	for range addrs {
		err := <-results
		if err == nil {
			return nil
		}
		if failure == nil || (errors.Is(failure, ErrProbeInconclusive) && !errors.Is(err, ErrProbeInconclusive)) {
			failure = err
		}
	}
	return failure
}

// targets resolves host to the addresses the policy permits dialling, in
// resolver order and capped at maxProbeAddresses. An IP literal resolves to
// itself. No permitted address is inconclusive: the endpoint is one no
// public client could reach either, and dialling it from the broker would
// answer a question about the broker's own network.
func (p *TCPRelayProber) targets(ctx context.Context, host string) ([]netip.Addr, error) {
	var addrs []netip.Addr
	if addr, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{addr}
	} else {
		resolved, err := p.resolve(ctx, host)
		if err != nil {
			return nil, classifyProbeError(err)
		}
		addrs = resolved
	}
	permitted := addrs[:0:0]
	for _, addr := range addrs {
		addr = addr.Unmap()
		if !p.permit(addr) || len(permitted) == maxProbeAddresses {
			continue
		}
		permitted = append(permitted, addr)
	}
	if len(permitted) == 0 {
		return nil, fmt.Errorf("%w: %s resolves to no public address", ErrProbeInconclusive, host)
	}
	return permitted, nil
}

// probeAddressPermitted is the dial policy: public unicast only — not
// loopback, private, link-local (the cloud metadata address included),
// carrier-grade NAT, multicast, unspecified, documentation, benchmark,
// reserved or IPv4-mapped-in-IPv6 space — and not an address of this host
// itself, whose firewall may trust loopback-originated connections that an
// outside client never gets to make.
func probeAddressPermitted(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	for _, prefix := range refusedProbePrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return !isLocalAddress(addr)
}

// refusedProbePrefixes are the non-routable or special-purpose ranges that
// netip's IsGlobalUnicast and IsPrivate do not already exclude.
var refusedProbePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),  // documentation
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),  // reserved, includes broadcast
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 well-known prefix
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), // 6to4 embeds an arbitrary IPv4
}

// isLocalAddress reports whether addr is assigned to one of this host's own
// interfaces. Interfaces are read on every call; probes are rare and the
// set must not go stale when an address is added after start-up.
func isLocalAddress(addr netip.Addr) bool {
	interfaceAddrs, err := net.InterfaceAddrs()
	if err != nil {
		// Unknown is treated as local: refusing a dial is the safe failure.
		return true
	}
	for _, interfaceAddr := range interfaceAddrs {
		ipNet, ok := interfaceAddr.(*net.IPNet)
		if !ok {
			continue
		}
		local, ok := netip.AddrFromSlice(ipNet.IP)
		if ok && local.Unmap() == addr {
			return true
		}
	}
	return false
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

// probeRegistration runs the registration-time probe of req's advertised
// endpoint. probed reports that a conclusive probe succeeded (the caller
// records it); a non-nil err is a conclusive failure the registration must
// be refused for. No prober, a non-direct transport or an inconclusive probe
// yield (false, nil): the relay registers unprobed, as before probing.
func probeRegistration(ctx context.Context, prober RelayProber, req relay.RegisterRequest) (probed bool, err error) {
	if prober == nil || normalizeTransport(req.Transport) != relay.TransportDirect {
		return false, nil
	}
	err = prober.Probe(ctx, req.PublicHost, req.PublicPort)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrProbeInconclusive) {
		slog.Warn("relay reachability probe inconclusive", "public", req.PublicHost, "port", req.PublicPort, "error", err)
		return false, nil
	}
	return false, err
}

// unreachableRegistrationMessage is the error body of a rejected
// registration. The relay logs it verbatim, and deploy/relay/volunteer-up.sh
// reads the leading phrase from the container log to tell the volunteer
// which firewall to open, so "public endpoint unreachable" is a contract.
func unreachableRegistrationMessage(req relay.RegisterRequest) string {
	endpoint := net.JoinHostPort(req.PublicHost, strconv.Itoa(req.PublicPort))
	return fmt.Sprintf("public endpoint unreachable: the broker could not connect to %s; allow inbound TCP %d in the provider firewall (security group) and any host firewall, the relay retries on its own", endpoint, req.PublicPort)
}

// recordReachability persists a probe outcome for a registered relay and
// mirrors it on desc. A store error leaves the stored result as it was and
// never fails the surrounding request.
func recordReachability(store RelayStore, desc *relay.Descriptor, reachable bool, now time.Time) {
	if err := store.UpdateReachability(desc.ID, desc.LeaseToken, reachable, now); err != nil {
		if !errors.Is(err, ErrRelayNotFound) {
			slog.Warn("could not store relay reachability", "relay_id", desc.ID, "error", err)
		}
		return
	}
	desc.Reachable = reachable
	desc.ReachabilityCheckedAt = now
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
	recordReachability(store, desc, reachable, now)
	if reachable {
		return
	}
	slog.Warn("relay endpoint unreachable, withheld from clients", "relay_id", desc.ID, "node_class", desc.NodeClass, "public", desc.PublicHost, "port", desc.PublicPort, "error", err)
}

// relayUnreachable reports whether desc's most recent probe failed. A relay
// that has never been probed (tunnel transport, or a result not yet written)
// is not unreachable.
func relayUnreachable(desc relay.Descriptor) bool {
	return !desc.ReachabilityCheckedAt.IsZero() && !desc.Reachable
}

// withoutUnreachable drops relays whose latest probe failed, preserving the
// ranking order of the rest, when probing is enabled; with it disabled the
// stored results are stale by definition (nothing will refresh them) and
// every relay is kept. The client-facing channels apply it after ranking and
// before page shaping; the operator views never do.
func withoutUnreachable(relays []relay.Descriptor, probing bool) []relay.Descriptor {
	if !probing {
		return relays
	}
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
