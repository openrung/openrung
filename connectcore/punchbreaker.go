package connectcore

import (
	"math/rand"
	"sync"
	"time"
)

// This file ports the mobile punch recovery circuit breaker (ADR-003 A3):
// android/app/src/main/java/com/openrung/vpn/PunchRecoveryCircuitBreaker.kt
// and ios/PacketTunnel/PunchRecoveryCircuitBreaker.swift, which are
// branch-for-branch identical where it matters. A punched direct path that
// keeps collapsing shortly after connecting is worse than the relay hub it
// bypasses: without damping, every recovery re-punches, and a NAT whose
// mapping dies every few seconds turns the session into a reconnect loop.
// Per relay, rapid failures (a path that lived less than the stable-path
// threshold) escalate a jittered backoff and, at the threshold, open the
// circuit — subsequent connects take the hub path for that relay until a
// user-initiated connect resets the breaker.
//
// Resolved divergences (the two platforms differ; the engine picks a side):
//   - Clocks: Android counts device sleep toward the stable-path uptime
//     (elapsedRealtime), iOS does not (uptimeNanoseconds). The engine uses
//     Go's monotonic clock, whose suspend behavior is platform-defined —
//     acceptable for a 5-minute threshold either way.
//   - Failure exemption: Android actively probes the physical network and
//     exempts an adapter loss only when the probe fails; iOS passively reads
//     the last NWPath snapshot. The engine follows Android: its
//     network-alive gate IS an active probe of the broker fronts (see
//     supervise's punch handling).
//   - iOS additionally exempts network-epoch-triggered punch recoveries;
//     the engine has no epoch-triggered punch retirement at all (an epoch
//     kicks an immediate health sweep and the probe decides), so the
//     exemption arises naturally when the network is down and never fires
//     otherwise — recorded here rather than replicated.

const (
	// punchStablePath: a punched path that survived at least this long was a
	// good path — its loss resets the rapid-failure streak
	// (PunchRecoveryCircuitBreaker.kt STABLE_PATH_MS / .swift
	// stablePathMilliseconds = 5 minutes).
	punchStablePath = 5 * time.Minute
	// punchMaxRapidFailures opens the circuit (.kt MAX_RAPID_FAILURES /
	// .swift maximumRapidFailures = 3).
	punchMaxRapidFailures = 3
	// punchInitialBackoff / punchMaxBackoff bound the escalating recovery
	// delay (.kt INITIAL_BACKOFF_MS 2s, MAX_BACKOFF_MS 30s).
	punchInitialBackoff = 2 * time.Second
	punchMaxBackoff     = 30 * time.Second
	// punchJitterDivisor spreads each delay by ±(nominal/5) = ±20%
	// (.kt JITTER_DIVISOR).
	punchJitterDivisor = 5
)

// punchRecoveryDecision is one onDirectPathLost outcome: whether the relay's
// circuit is now open (use the hub), the jittered backoff to wait before the
// recovery ladder, and the observability fields the punch_fallback telemetry
// carries (mirroring the mobile decision payloads).
type punchRecoveryDecision struct {
	useRelayHub   bool
	delay         time.Duration
	rapidFailures int
	directUptime  time.Duration
	counted       bool
}

// punchBreakerConfig carries the breaker's tunables; the zero value selects
// the mobile constants (tests shorten the backoffs and pin the jitter).
type punchBreakerConfig struct {
	stablePath        time.Duration
	maxRapid          int
	initialBackoff    time.Duration
	maxBackoff        time.Duration
	pickJitteredDelay func(min, max time.Duration) time.Duration
}

func (c punchBreakerConfig) withDefaults() punchBreakerConfig {
	if c.stablePath <= 0 {
		c.stablePath = punchStablePath
	}
	if c.maxRapid <= 0 {
		c.maxRapid = punchMaxRapidFailures
	}
	if c.initialBackoff <= 0 {
		c.initialBackoff = punchInitialBackoff
	}
	if c.maxBackoff < c.initialBackoff {
		c.maxBackoff = punchMaxBackoff
	}
	if c.pickJitteredDelay == nil {
		c.pickJitteredDelay = func(min, max time.Duration) time.Duration {
			if max <= min {
				return min
			}
			return min + time.Duration(rand.Int63n(int64(max-min)+1))
		}
	}
	return c
}

// punchRelayState mirrors the mobile RelayState: a consecutive rapid-failure
// streak (no time window — reset only by a stable path or a fresh breaker),
// the last promotion's timestamp, and the open flag.
type punchRelayState struct {
	rapidFailures  int
	connectedAt    time.Time
	hasConnectedAt bool
	open           bool
}

// punchRecoveryBreaker holds per-relay punch recovery state for ONE
// connection: a user-initiated connect builds a fresh connection (and so a
// fresh breaker — the mobile reset-on-ACTION_CONNECT semantics), while
// automatic recovery re-ladders share the connection and deliberately keep
// the streak. Safe for concurrent use.
type punchRecoveryBreaker struct {
	mu     sync.Mutex
	config punchBreakerConfig
	relays map[string]*punchRelayState
}

func (b *punchRecoveryBreaker) stateLocked(relayID string) *punchRelayState {
	if b.relays == nil {
		b.relays = make(map[string]*punchRelayState)
	}
	state, ok := b.relays[relayID]
	if !ok {
		state = &punchRelayState{}
		b.relays[relayID] = state
	}
	return state
}

// markDirectConnected stamps a punched path's promotion time. It deliberately
// does not clear the failure streak or close the circuit — only a path that
// then SURVIVES the stable-path threshold earns that, judged at the next
// loss (the mobile markDirectConnected).
func (b *punchRecoveryBreaker) markDirectConnected(relayID string, now time.Time) {
	if relayID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.stateLocked(relayID)
	state.connectedAt = now
	state.hasConnectedAt = true
}

// allowsDirectPunch reports whether the relay's circuit permits a punch
// attempt. An unknown relay is allowed.
func (b *punchRecoveryBreaker) allowsDirectPunch(relayID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	state, ok := b.relays[relayID]
	return !ok || !state.open
}

// onDirectPathLost records one punched-path loss and decides the recovery: a
// stable path (uptime >= threshold, evaluated BEFORE the loss is counted)
// resets the streak first; an exempt loss (countTowardBreaker false — the
// physical network itself was down) changes nothing and retries with no
// delay; a counted loss escalates the jittered backoff and, at the
// rapid-failure threshold, opens the circuit.
func (b *punchRecoveryBreaker) onDirectPathLost(relayID string, now time.Time, countTowardBreaker bool) punchRecoveryDecision {
	b.mu.Lock()
	defer b.mu.Unlock()
	config := b.config.withDefaults()
	state := b.stateLocked(relayID)

	var uptime time.Duration
	if state.hasConnectedAt && now.After(state.connectedAt) {
		// A clock regression reads as zero uptime, never negative (the
		// mobile coerceAtLeast(0) / now >= connectedAt guards).
		uptime = now.Sub(state.connectedAt)
	}
	state.hasConnectedAt = false

	if uptime >= config.stablePath {
		// The boundary is inclusive: exactly the threshold qualifies
		// (.kt/.swift ">=" — their tests pin STABLE_PATH_MS-1 as not enough).
		state.rapidFailures = 0
		state.open = false
	}
	if !countTowardBreaker {
		return punchRecoveryDecision{
			rapidFailures: state.rapidFailures,
			directUptime:  uptime,
		}
	}

	state.rapidFailures++
	delay := config.backoffDelay(state.rapidFailures)
	if state.rapidFailures >= config.maxRapid {
		state.open = true
		return punchRecoveryDecision{
			useRelayHub:   true,
			delay:         delay,
			rapidFailures: state.rapidFailures,
			directUptime:  uptime,
			counted:       true,
		}
	}
	return punchRecoveryDecision{
		delay:         delay,
		rapidFailures: state.rapidFailures,
		directUptime:  uptime,
		counted:       true,
	}
}

// backoffDelay is the mobile ladder verbatim: double the nominal per failure
// (jumping straight to the cap once past half of it), then jitter by
// ±(nominal/5), clamped into [1ns floor, maxBackoff].
func (c punchBreakerConfig) backoffDelay(failureCount int) time.Duration {
	nominal := c.initialBackoff
	for i := 1; i < failureCount; i++ {
		if nominal >= c.maxBackoff/2 {
			nominal = c.maxBackoff
		} else {
			nominal *= 2
			if nominal > c.maxBackoff {
				nominal = c.maxBackoff
			}
		}
	}
	jitter := nominal / punchJitterDivisor
	min := nominal - jitter
	if min < 1 {
		min = 1
	}
	max := nominal + jitter
	if max > c.maxBackoff {
		max = c.maxBackoff
	}
	delay := c.pickJitteredDelay(min, max)
	if delay < min {
		delay = min
	}
	if delay > max {
		delay = max
	}
	return delay
}
