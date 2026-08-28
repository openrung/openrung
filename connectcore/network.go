package connectcore

// This file owns the platform network-signal seam (ADR-003 A2): mobile hosts
// feed OS connectivity callbacks into the engine, which consumes them IN
// ADDITION TO its own probing. The epoch semantics are the mobile
// PhysicalNetworkEpochMonitor's, read from its Kotlin and Swift sources as
// the spec and encoded here once so the Track B adapters stay thin:
//
//   - Change detection is by fingerprint of the PHYSICAL (non-tunnel) path
//     set, not by counting callbacks: platforms deliver noisy, repeated
//     callbacks for one state, and only a changed snapshot is an epoch
//     boundary.
//   - The first observation is the baseline and is absorbed silently.
//   - Identical consecutive observations are ignored.
//   - An epoch never fires into a session it predates: each promoted
//     candidate re-baselines, the engine equivalent of Android creating the
//     monitor fresh per WSS session.
//
// Consumption mirrors the platforms where they have semantics, and stays
// probe-driven where they do not:
//
//   - A live WSS session is retired by an epoch boundary and recovered with a
//     fresh direct-first ladder (Android: "a WSS socket is bound to one
//     physical-network epoch; any physical route/interface/DNS change retires
//     the whole session — recovery always reruns signed discovery and direct
//     Reality first"). The retirement is transport-scoped and orderly: it
//     dents neither the relay nor the front.
//   - A direct or punched session gets an immediate health sweep instead of
//     being retired: mobile has no epoch monitor on those paths, and the
//     engine's own end-to-end probe is the authority on whether the tunnel
//     survived the change. (Deliberate difference, recorded in ADR-003: the
//     signal accelerates the engine's probing, it never replaces it.)
//   - During recovery, a signal wakes the network-outage gate immediately
//     instead of waiting out its poll interval (iOS waitUntilSatisfied's
//     event-driven wait). The dial probe remains the authority on "alive":
//     a stale down-signal can therefore never wedge recovery. (Second
//     recorded difference: iOS additionally exempts adapter losses from its
//     circuit breaker while the path is unsatisfied; the engine's equivalent
//     gate stays probe-driven.)

// NetworkState is one platform observation of the host's physical
// (non-tunnel) network.
type NetworkState struct {
	// Up reports whether any internet-capable physical network exists right
	// now (Android: a non-VPN network with validated internet capability;
	// iOS: NWPath satisfied).
	Up bool
	// Fingerprint identifies the physical path set — interfaces, addresses,
	// DNS, routes, transports — in whatever encoding the platform monitor
	// produces. It is opaque to the engine: equality is the only operation,
	// and ANY change (with Up folded in) is an epoch boundary.
	Fingerprint string
}

// UpdateNetworkState feeds one platform network observation into the engine.
// Safe to call at any time from any goroutine, connected or not; signals that
// arrive with no live session only advance the tracker. Mobile adapters call
// it from their connectivity callbacks; desktop and the TUI never call it and
// keep the engine purely probe-driven.
func (s *Engine) UpdateNetworkState(state NetworkState) {
	s.netMu.Lock()
	if !s.netBaselined {
		s.netBaselined = true
		s.netLast = state
		s.netMu.Unlock()
		s.appendLog("physical network baseline recorded")
		return
	}
	if state == s.netLast {
		s.netMu.Unlock()
		return
	}
	wasUp := s.netLast.Up
	s.netLast = state
	s.netEpoch++
	s.netMu.Unlock()

	reason := "physical network epoch changed"
	switch {
	case wasUp && !state.Up:
		reason = "physical network lost"
	case !wasUp && state.Up:
		reason = "physical network restored"
	}
	s.appendLog(reason)
	s.notify(Notice{Kind: NoticeNetworkEpoch, Reason: reason})

	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		select {
		case conn.netNotify <- struct{}{}:
		default: // a wake is already pending; the epoch counter carries the rest
		}
	}
}

// networkEpoch returns the tracker's current epoch counter. Epochs before a
// session's own baseline are settled history: consumers compare against the
// value they captured when their transport was established.
func (s *Engine) networkEpoch() uint64 {
	s.netMu.Lock()
	defer s.netMu.Unlock()
	return s.netEpoch
}
