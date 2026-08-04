package broker

import (
	"log/slog"
	"net/http"
	"sort"
	"time"

	"openrung/internal/relay"
)

// The operational relay inventory is the broker's machine-to-machine view of
// its own fleet: every currently active relay, in a stable order, for the
// operator's own tooling (fleet audits, deployment checks, capacity reports).
//
// It deliberately is NOT the client directory. GET /api/v1/relays answers
// "which relays should this client try, best first" and truncates a ranked
// page; this endpoint answers "which relays exist right now" and truncates
// nothing. Conflating the two is how a fleet audit silently misses relays that
// simply ranked below the page cut — the exact failure this endpoint exists to
// prevent.

const (
	// inventoryNotAfterWindow bounds replay of a signed inventory snapshot.
	// It is deliberately far shorter than the API channel's 30 minutes: a
	// relay lease lives ~3 minutes, so an inventory older than a few minutes
	// describes a fleet that no longer exists, and an operator acting on one
	// is worse off than an operator who saw an error.
	inventoryNotAfterWindow = 5 * time.Minute

	// The inventory is polled by the operator's own tooling, not by clients:
	// a handful of requests per minute is normal, while each one costs a full
	// unbounded store read plus a signature over the whole fleet. Keep the
	// budget well under the client relay-list limiter — a credentialed
	// endpoint still gets a limiter, because a leaked token must not become an
	// amplification lever against the broker's database.
	inventoryRatePerSecond     = 1.0
	inventoryBurst             = 10
	inventoryRetryAfterSeconds = 10
)

// relayInventoryHandler serves the complete active relay set to a caller
// holding the operational API token. The route is only registered when that
// token is configured (see NewServer), so an unconfigured broker answers 404
// rather than exposing the endpoint in an open state.
func relayInventoryHandler(store RelayStore, apiToken string, s signer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// A credentialed snapshot of live fleet state: no browser, proxy, or
		// CDN edge may retain any part of it. Set before anything else so the
		// 401 and the 503 are uncacheable too — an edge that cached either
		// would replay one operator's error to every other. The signed success
		// path upgrades this to no-store, no-transform.
		w.Header().Set("Cache-Control", "no-store")
		// bearerMatches, not authorized: an empty token must match nothing.
		// The route should never be registered without a token, and if that
		// ever regresses this endpoint fails closed instead of open.
		if !bearerMatches(r, apiToken) {
			writeError(w, http.StatusUnauthorized, "missing or invalid operational API token")
			return
		}
		// The method check follows the credential check on purpose. Registered
		// as a "GET /..." pattern this would be ServeMux's job, but ServeMux
		// answers a method mismatch itself — before the handler and before the
		// rate limiter — with an uncacheable-by-default 405 that also tells an
		// anonymous prober the route exists (405 here versus 404 on a broker
		// with no token configured). Answering it inside the handler keeps the
		// whole surface behind one credential, one limiter, and one no-store.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		now := time.Now().UTC()
		// limit 0 is the store's unbounded read. Operators need the whole
		// fleet; a page would make "relay missing from the inventory" and
		// "relay ranked below the cut" indistinguishable.
		relays, err := store.List(now, 0)
		if err != nil {
			slog.Error("could not list relays for inventory", "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not list relays")
			return
		}
		inventory := sortInventoryByRelayID(relays)
		s.writeSigned(w, relay.ListResponse{
			Count:      len(inventory),
			ServerTime: now,
			NotAfter:   now.Add(inventoryNotAfterWindow),
			KeyID:      s.keyID,
			Channel:    relay.ChannelInventory,
			// No limit field: the inventory is not request-shaped and is never
			// truncated, so there is nothing to echo.
			Relays: inventory,
		})
	}
}

// sortInventoryByRelayID returns the descriptors ordered by relay ID, leaving
// the store's slice untouched.
//
// The copy is not optional. List's slice belongs to the store — the Postgres
// implementation hands back its own freshly built one today, but a future
// caching store need not — and sorting in place would reorder the ranked
// candidate list every client request depends on.
//
// Relay ID is the right key because ranking order changes with live telemetry:
// two inventories taken a minute apart would otherwise differ everywhere and
// diff to noise even when the fleet is unchanged. It is the best key available,
// not a perfect one — a relay registering with a stable identity keeps its ID
// across reconnects, but a legacy identity-less registration mints a fresh
// random ID each time (see Store.Register), so those rows still move between
// snapshots. The ordering is stable with respect to the IDs it is given either
// way, which is what makes two snapshots comparable at all.
func sortInventoryByRelayID(relays []relay.Descriptor) []relay.Descriptor {
	// Keep the wire shape an array for a store that returns nil (Postgres with
	// zero rows), matching the public relay-list channels.
	sorted := make([]relay.Descriptor, len(relays))
	copy(sorted, relays)
	// Relay IDs are unique per active descriptor (map key in the memory store,
	// primary key in Postgres), so this ordering is total.
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	return sorted
}
