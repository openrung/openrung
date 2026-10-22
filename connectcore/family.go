// SPDX-License-Identifier: GPL-3.0-or-later

package connectcore

import (
	"fmt"
	"net"

	"github.com/openrung/openrung/brokerapi"
)

// Address-family fallback for relay candidates. The per-user access answer
// can name a relay by an IPv6 access address (ADR-006.1) while the public
// relay list names the same relay, under the same ID, by its public endpoint
// — usually IPv4. Both addresses reach one relay listener and accept the same
// per-user credential, so a device that cannot use IPv6 keeps the relay by
// dialling its public endpoint instead of losing it:
//
//   - When the device is known to have no usable IPv6, an IPv6 candidate is
//     rewritten to the relay's public endpoint before ranking; one without a
//     known public endpoint (an IPv6-advertised relay in the public list) is
//     moved behind the others.
//   - Whatever the device reports, a failed TCP check against an IPv6
//     candidate is retried once on the relay's public endpoint before the
//     candidate is given up, which covers IPv6 that is routed but broken and
//     a network that changed after the answer arrived.
//
// Only the endpoint changes. The credential, the relay ID and everything else
// in the descriptor stay as the answer gave them.

// ipv6State is what the engine knows about this device's IPv6.
type ipv6State int

const (
	ipv6Unknown ipv6State = iota
	ipv6Available
	ipv6Unavailable
)

// ipv6RouteProbeTarget is the route-lookup destination for the kernel probe.
// Nothing is sent to it: connecting a UDP socket only resolves the route and
// the source address the kernel would use.
const ipv6RouteProbeTarget = "[2606:4700:4700::1111]:53"

// relayEndpoint is a relay's public endpoint as the public list names it.
type relayEndpoint struct {
	host string
	port int
}

// ipv6Availability reports whether this device can use IPv6 right now. A
// mobile host's own report wins (NetworkState.IPv6Known); without one, a
// mobile engine stays unknown, because the platform VPN may own the route
// table. Desktop and the terminal client ask the kernel which source address
// it would use for an IPv6 destination: a global address means available; no
// route, or only a local (unique-local or link-local) source, means
// unavailable; the TUN's own address means the tunnel captured the lookup,
// which says nothing about the physical network, so unknown.
func (s *Engine) ipv6Availability() ipv6State {
	if s.ipv6Probe != nil {
		return s.ipv6Probe()
	}
	s.netMu.Lock()
	reported, known := s.netLast.HasIPv6, s.netBaselined && s.netLast.IPv6Known
	s.netMu.Unlock()
	if known {
		if reported {
			return ipv6Available
		}
		return ipv6Unavailable
	}
	if s.Mobile != nil {
		return ipv6Unknown
	}
	source, err := routeSourceIP("udp6", ipv6RouteProbeTarget)
	return classifyIPv6RouteSource(source, err)
}

func classifyIPv6RouteSource(source net.IP, err error) ipv6State {
	switch {
	case err != nil:
		return ipv6Unavailable
	case source.Equal(tunnelAddressIPv6) || source.Equal(tunnelAddressIPv4):
		return ipv6Unknown
	case source.To4() == nil && source.IsGlobalUnicast() && !source.IsPrivate():
		return ipv6Available
	default:
		return ipv6Unavailable
	}
}

// rememberPublicEndpoints records the public endpoint of every relay in a
// public relay list that does not name it by IPv6, so a later IPv6 candidate
// for the same relay — from an access answer, possibly fetched on a recovery
// that never fetched the public list — has an endpoint to fall back to.
func (c *connection) rememberPublicEndpoints(resp brokerapi.RelayListResponse) {
	for _, relay := range resp.Relays {
		if relay.ID == "" || relay.PublicHost == "" || brokerapi.IsIPv6Host(relay.PublicHost) {
			continue
		}
		if c.publicEndpoints == nil {
			c.publicEndpoints = make(map[string]relayEndpoint)
		}
		c.publicEndpoints[relay.ID] = relayEndpoint{host: relay.PublicHost, port: relay.PublicPort}
	}
}

// snapshotEndpoints copies the public endpoints known now, so a refresh can
// tell which relays it gave a new endpoint.
func (c *connection) snapshotEndpoints() map[string]relayEndpoint {
	snapshot := make(map[string]relayEndpoint, len(c.publicEndpoints))
	for id, endpoint := range c.publicEndpoints {
		snapshot[id] = endpoint
	}
	return snapshot
}

// leasedEndpointRetries returns the tried candidates a refresh gave an
// endpoint they have not been tried on, rewritten to it with their WSS fronts
// removed (the ladder already tried those). A candidate qualifies when the
// ladder could only have reached it by IPv6 or at the endpoint known before
// the refresh (the fallback's own retry target, or the no-IPv6 rewrite) and
// the refreshed endpoint differs from that one. Relay IDs survive endpoint
// changes, so the comparison is on host and port, never on the ID alone.
func (c *connection) leasedEndpointRetries(tried []brokerapi.RelayDescriptor, before map[string]relayEndpoint) []brokerapi.RelayDescriptor {
	var retry []brokerapi.RelayDescriptor
	for _, cand := range tried {
		current, ok := c.publicEndpoints[cand.ID]
		if !ok {
			continue
		}
		previous, hadPrevious := before[cand.ID]
		if hadPrevious && previous == current {
			continue // nothing new: the ladder already had this endpoint
		}
		triedEndpoint := relayEndpoint{host: cand.PublicHost, port: cand.PublicPort}
		fromPrevious := hadPrevious && triedEndpoint == previous
		if !brokerapi.IsIPv6Host(cand.PublicHost) && !fromPrevious {
			continue // an endpoint the answer itself named, not one learned here
		}
		if triedEndpoint == current {
			continue
		}
		cand.PublicHost, cand.PublicPort = current.host, current.port
		cand.WSSFronts = nil
		retry = append(retry, cand)
	}
	return retry
}

// publicEndpointFor returns the public endpoint to retry an IPv6 candidate
// on, when one is known and differs from the candidate's own.
func (c *connection) publicEndpointFor(cand brokerapi.RelayDescriptor) (relayEndpoint, bool) {
	if !brokerapi.IsIPv6Host(cand.PublicHost) {
		return relayEndpoint{}, false
	}
	endpoint, ok := c.publicEndpoints[cand.ID]
	if !ok || endpoint.host == cand.PublicHost {
		return relayEndpoint{}, false
	}
	return endpoint, true
}

// adaptToAddressFamily applies the no-IPv6 rewrite and reordering to a
// candidate list before it is ranked. It never drops a candidate and leaves
// the list untouched unless the device is known to lack IPv6.
func (s *Engine) adaptToAddressFamily(conn *connection, cands []brokerapi.RelayDescriptor) []brokerapi.RelayDescriptor {
	hasIPv6Candidate := false
	for _, cand := range cands {
		if brokerapi.IsIPv6Host(cand.PublicHost) {
			hasIPv6Candidate = true
			break
		}
	}
	if !hasIPv6Candidate || s.ipv6Availability() != ipv6Unavailable {
		return cands
	}
	out := make([]brokerapi.RelayDescriptor, 0, len(cands))
	var deferred []brokerapi.RelayDescriptor
	rewritten := 0
	for _, cand := range cands {
		if endpoint, ok := conn.publicEndpointFor(cand); ok {
			cand.PublicHost, cand.PublicPort = endpoint.host, endpoint.port
			out = append(out, cand)
			rewritten++
			continue
		}
		if brokerapi.IsIPv6Host(cand.PublicHost) {
			deferred = append(deferred, cand)
			continue
		}
		out = append(out, cand)
	}
	if rewritten > 0 {
		s.appendLog(fmt.Sprintf("no IPv6 on this network: using the public IPv4 endpoint for %d relay(s)", rewritten))
	}
	if len(deferred) > 0 {
		s.appendLog(fmt.Sprintf("no IPv6 on this network: trying %d IPv6-only relay(s) last", len(deferred)))
	}
	return append(out, deferred...)
}
