package broker

import (
	"sync"
	"time"
)

const (
	// relayLedgerTTL matches telemetryRetention: any relay registered at some
	// point covered by the dashboard window may legitimately still be referenced
	// by late telemetry (failure reports about a relay that just died, uploads
	// from a client that reconnected after days offline).
	relayLedgerTTL = telemetryRetention

	// relayLedgerMaxEntries bounds ledger memory (~60 B per entry). It is far
	// above any plausible fleet, and with registrations capped per IP an
	// attacker needs hundreds of IP-days to approach it.
	relayLedgerMaxEntries = 16384
)

// relayIDLedger remembers every relay ID that completed a registration or
// heartbeat recently — a superset of the currently-active fleet that keeps
// covering a relay for relayLedgerTTL after its short lease expires.
//
// Telemetry ingestion consults it for events whose relay_id failed live
// attestation: an ID the broker never leased within the retention window
// cannot appear in any client's signed relay list, so such events are
// fabricated and are discarded before they reach storage, dashboards, or
// ranking. Registration uses it the other way around: a request re-proving a
// known identity is a reconnect, not a new relay, and stays outside the
// per-IP new-identity cap.
//
// The ledger is process-local. After a broker restart it reseeds from the
// store's active descriptors, so the only loss is coverage of relays that
// died before the restart and never returned — their late failure reports are
// dropped until they register again.
type relayIDLedger struct {
	ttl        time.Duration
	maxEntries int

	mu   sync.Mutex
	seen map[string]time.Time
}

func newRelayIDLedger(ttl time.Duration, maxEntries int) *relayIDLedger {
	return &relayIDLedger{ttl: ttl, maxEntries: maxEntries, seen: make(map[string]time.Time)}
}

// remember records that id holds (or held) a broker lease at now. At capacity
// it first drops expired entries; if the ledger is still full the new entry is
// skipped rather than evicting an older one — under cardinality pressure the
// long-lived fleet keeps its coverage and the newcomer (most likely the
// attacker's own mint) loses only post-expiry telemetry attribution.
func (l *relayIDLedger) remember(id string, now time.Time) {
	if id == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.seen[id]; !ok && len(l.seen) >= l.maxEntries {
		l.sweepLocked(now)
		if len(l.seen) >= l.maxEntries {
			return
		}
	}
	l.seen[id] = now
}

// knows reports whether id completed a registration or heartbeat within the
// TTL. Expired entries are dropped on sight.
func (l *relayIDLedger) knows(id string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	seenAt, ok := l.seen[id]
	if !ok {
		return false
	}
	if now.Sub(seenAt) > l.ttl {
		delete(l.seen, id)
		return false
	}
	return true
}

func (l *relayIDLedger) sweepLocked(now time.Time) {
	for id, seenAt := range l.seen {
		if now.Sub(seenAt) > l.ttl {
			delete(l.seen, id)
		}
	}
}
