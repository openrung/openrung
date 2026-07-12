package punchcore

import "net"

// Policy selects between the trust profiles of the two first-party consumers.
// DesktopPolicy preserves the historical openrung/internal/punch behavior and is
// also what the hub and volunteers run; MobilePolicy preserves the hardened
// behavior of the Android punchbridge. Zero values are NOT a valid policy; always
// start from a preset.
type Policy struct {
	// MaxPeersPerKind caps how many candidates of each kind (host, srflx) a peer
	// may advertise before SanitizePeers stops accepting them. Desktop 8, mobile 4.
	MaxPeersPerKind int
	// RequireGlobalSrflx drops srflx candidates whose IP is not globally routable.
	// Desktop false (srflx provenance is enforced upstream by the coordinator and
	// loopback srflx is load-bearing for tests/LAN dev), mobile true.
	RequireGlobalSrflx bool
	// SingleSrflxIP pins all srflx candidates to the first accepted srflx IP
	// (ports may differ; a second IP is dropped). Desktop false, mobile true.
	SingleSrflxIP bool
	// StrictReflectorAddrs requires each reflector address to be a literal,
	// globally routable IPv4 host:port; non-conforming entries are skipped without
	// DNS resolution. Desktop false (net.ResolveUDPAddr, DNS names allowed),
	// mobile true (udp4 socket + signed literal tuples + avoids uncancelable DNS
	// lookups blocking Close).
	StrictReflectorAddrs bool
	// FailGatherOnCancel makes Gather return ctx.Err() when the context is
	// cancelled between rounds instead of classifying the partial observations
	// (desktop behavior, relied on by RespondToDirective which ignores Gather's
	// error and proceeds with whatever was gathered). Desktop false, mobile true.
	FailGatherOnCancel bool
}

// DesktopPolicy is the historical openrung/internal/punch profile, used by the
// hub coordinator, the CLI/desktop clients, and volunteers.
func DesktopPolicy() Policy { return Policy{MaxPeersPerKind: 8} }

// MobilePolicy is the hardened Android punchbridge profile.
func MobilePolicy() Policy {
	return Policy{MaxPeersPerKind: 4, RequireGlobalSrflx: true, SingleSrflxIP: true,
		StrictReflectorAddrs: true, FailGatherOnCancel: true}
}

// SanitizePeers filters and clamps peer-advertised punch candidates so the probe
// spray in Attempt can never be aimed at an arbitrary third party:
//
//   - A "host" (LAN) candidate must NOT be globally routable. A public IP tagged
//     as a same-subnet address is an attempt to make the volunteer flood a
//     victim, never a real LAN peer, so it is dropped.
//   - Multicast/unspecified addresses and invalid ports are always dropped.
//   - Each kind (host, srflx) is independently capped at MaxPeersPerKind.
//
// Under DesktopPolicy, reflexive ("srflx") provenance is enforced upstream by
// the punch coordinator, which forwards only reflector-observed reflexive
// endpoints; a public srflx reaching here has therefore already been proven to
// belong to a real peer. MobilePolicy does not extend that trust to the
// coordinator: RequireGlobalSrflx rejects private srflx values so an
// unauthenticated coordinator cannot relabel a LAN target and turn the mobile
// client into a local-network UDP probe source, and SingleSrflxIP pins srflx
// candidates to one public IP — reflectors may observe different ports for a
// symmetric NAT, but a volunteer session is expected to have one public egress
// address, so a client's authenticated probes are never fanned out across
// public IPs.
func (p Policy) SanitizePeers(in []Endpoint) []Endpoint {
	out := make([]Endpoint, 0, len(in))
	var hostN, srflxN int
	var srflxIP net.IP
	for _, e := range dedupeEndpoints(in) {
		ip := net.ParseIP(e.IP)
		if ip == nil || e.Port < 1 || e.Port > 65535 || ip.IsMulticast() || ip.IsUnspecified() {
			continue
		}
		switch e.Kind {
		case KindHost:
			if isGloballyRoutable(ip) || hostN >= p.MaxPeersPerKind {
				continue
			}
			hostN++
		case KindSrflx:
			if (p.RequireGlobalSrflx && !isGloballyRoutable(ip)) || srflxN >= p.MaxPeersPerKind {
				continue
			}
			if p.SingleSrflxIP {
				if srflxIP == nil {
					srflxIP = append(net.IP(nil), ip...)
				} else if !srflxIP.Equal(ip) {
					continue
				}
			}
			srflxN++
		default:
			continue // unknown kind: never spray at it
		}
		out = append(out, e)
	}
	return out
}
