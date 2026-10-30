package broker

import (
	"encoding/json"
	"errors"
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
// withholdUnreachable is the public list's reachability policy (probing
// enabled), so head shares describe the list clients actually get.
func relayInventoryHandler(store RelayStore, apiToken string, s signer, withholdUnreachable bool) http.HandlerFunc {
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
		relays, ranking, err := store.ListRanked(now, 0)
		if err != nil {
			slog.Error("could not list relays for inventory", "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not list relays")
			return
		}
		normalized := normalizeSignedRelayListTimes(relay.ListResponse{
			Count:      len(relays),
			ServerTime: now,
			NotAfter:   now.Add(inventoryNotAfterWindow),
			KeyID:      s.keyID,
			Channel:    relay.ChannelInventory,
			Relays:     sortInventoryByRelayID(relays),
		})
		s.writeSignedJSON(w, inventoryResponse{
			Count:      normalized.Count,
			ServerTime: normalized.ServerTime,
			NotAfter:   normalized.NotAfter,
			KeyID:      normalized.KeyID,
			Channel:    normalized.Channel,
			// No limit field: the inventory is not request-shaped and is never
			// truncated, so there is nothing to echo.
			Relays: decorateInventory(normalized.Relays, ranking, withholdUnreachable),
		})
	}
}

// inventoryResponse is the inventory channel's signed envelope: the public
// relay.ListResponse shape (minus limit, which the inventory never carries)
// with each descriptor decorated by operator-only state. It is a separate type
// rather than extra fields on relay.Descriptor so the public directory's wire
// shape — which every client pins by schema — stays untouched.
type inventoryResponse struct {
	Count      int                   `json:"count"`
	ServerTime time.Time             `json:"server_time"`
	NotAfter   time.Time             `json:"not_after"`
	KeyID      string                `json:"key_id"`
	Channel    string                `json:"channel"`
	Relays     []inventoryDescriptor `json:"relays"`
}

// inventoryDescriptor is a public descriptor plus the operator-only state the
// broker holds about it: the effective ranking weight it is applying right
// now (defaultRankingWeight when no override is stored) and the telemetry
// score breakdown it is ranking with (see rankRelays), the latest inbound
// reachability probe, the operator contact the relay registered with, and its
// access address. All are operational state — they exist so fleet tooling can
// see where an operator has shifted load, which relays the broker is
// withholding from clients, whom to reach about one, and which address the
// access answer hands out instead of the public host — and never appear in
// the client-facing channels.
type inventoryDescriptor struct {
	relay.Descriptor
	RankingWeight float64
	Ranking       relayRanking
}

// inventoryState is the trailing operator-only block of an inventory
// descriptor. reachable is null until the broker has probed the relay (tunnel
// transport, probing disabled, or not yet), then the latest outcome;
// reachability_checked_at is omitted in the same case. operator_contact and
// access_host are omitted when the registration carried none.
type inventoryState struct {
	RankingWeight         float64      `json:"ranking_weight"`
	Ranking               relayRanking `json:"ranking"`
	Reachable             *bool        `json:"reachable"`
	ReachabilityCheckedAt *time.Time   `json:"reachability_checked_at,omitempty"`
	OperatorContact       string       `json:"operator_contact,omitempty"`
	AccessHost            string       `json:"access_host,omitempty"`
}

// MarshalJSON emits the descriptor exactly as the public channels do (through
// relay.Descriptor's own marshaler, deprecated aliases included) and appends
// the operator-only keys after it. Embedding alone would not do: the embedded
// type's MarshalJSON is promoted and would silently drop every field declared
// beside it.
func (d inventoryDescriptor) MarshalJSON() ([]byte, error) {
	body, err := json.Marshal(d.Descriptor)
	if err != nil {
		return nil, err
	}
	if len(body) < 2 || body[len(body)-1] != '}' {
		return nil, errors.New("relay descriptor did not marshal to a JSON object")
	}
	state, err := json.Marshal(inventoryState{
		RankingWeight:         d.RankingWeight,
		Ranking:               d.Ranking,
		Reachable:             reachablePointer(d.Descriptor),
		ReachabilityCheckedAt: timePointer(d.ReachabilityCheckedAt),
		OperatorContact:       d.OperatorContact,
		AccessHost:            d.AccessHost,
	})
	if err != nil {
		return nil, err
	}
	body = append(body[:len(body)-1], ',')
	return append(body, state[1:]...), nil
}

func decorateInventory(relays []relay.Descriptor, inputs RankingInputs, withholdUnreachable bool) []inventoryDescriptor {
	// Keep the wire shape an array for an empty fleet, like every signed channel.
	rankings := rankRelays(relays, inputs, withholdUnreachable)
	decorated := make([]inventoryDescriptor, len(relays))
	for i, desc := range relays {
		decorated[i] = inventoryDescriptor{Descriptor: desc, RankingWeight: rankingWeightFor(inputs.Weights, desc.ID), Ranking: rankings[desc.ID]}
	}
	return decorated
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
