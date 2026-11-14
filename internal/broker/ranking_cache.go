// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"sync"
	"sync/atomic"
	"time"

	"openrung/internal/relay"
)

// rankingCacheTTL bounds how stale the Postgres store's ranking inputs may be.
// Every relay-list and access request needs the live descriptors plus the
// metric aggregates over rankingWindow (30 minutes), so a few seconds of reuse
// changes no ranking materially while collapsing the per-request queries into
// one load per TTL. Each caller still draws its own sampling keys, so request
// orders stay independent.
const rankingCacheTTL = 5 * time.Second

// rankingCache holds the last loaded ranking inputs. Loads are coalesced:
// concurrent misses wait for one load instead of each querying Postgres.
// Writers that change what the directory serves bump the generation, which
// invalidates the cached load (and any load already in flight) without
// waiting on the load lock.
type rankingCache struct {
	ttl  time.Duration
	gen  atomic.Uint64
	snap atomic.Pointer[rankingSnapshot]
	mu   sync.Mutex // serializes loads
}

type rankingSnapshot struct {
	gen      uint64
	now      time.Time // the store time the load filtered and aggregated at
	loadedAt time.Time // wall-clock load time, for the TTL
	relays   []relay.Descriptor
	inputs   RankingInputs
}

// fresh reports whether snap can answer a request at store time now. A
// request earlier than the load could need rows the load already treated as
// expired, and one further than the TTL past it wants a newer window.
func (c *rankingCache) fresh(snap *rankingSnapshot, now time.Time) bool {
	if snap == nil || snap.gen != c.gen.Load() {
		return false
	}
	age := now.Sub(snap.now)
	return age >= 0 && age < c.ttl && time.Since(snap.loadedAt) < c.ttl
}

// get returns unexpired relays (a fresh slice the caller may reorder) and the
// ranking inputs at store time now, loading through load on a miss. The
// returned inputs are shared and must be treated as read-only.
func (c *rankingCache) get(now time.Time, load func(time.Time) ([]relay.Descriptor, RankingInputs, error)) ([]relay.Descriptor, RankingInputs, error) {
	snap := c.snap.Load()
	if !c.fresh(snap, now) {
		c.mu.Lock()
		defer c.mu.Unlock()
		snap = c.snap.Load() // another caller may have loaded while this one waited
	}
	if !c.fresh(snap, now) {
		gen := c.gen.Load()
		relays, inputs, err := load(now)
		if err != nil {
			return nil, RankingInputs{}, err
		}
		snap = &rankingSnapshot{gen: gen, now: now, loadedAt: time.Now(), relays: relays, inputs: inputs}
		c.snap.Store(snap)
	}
	out := make([]relay.Descriptor, 0, len(snap.relays))
	for _, desc := range snap.relays {
		if desc.ExpiresAt.After(now) {
			out = append(out, desc)
		}
	}
	return out, snap.inputs, nil
}

// invalidate drops the cached load so the next request reads Postgres.
func (c *rankingCache) invalidate() {
	c.gen.Add(1)
}

// invalidateIfChanged invalidates when the cache holds relay id and changed
// reports that a write made the cached copy serve differently. Frequent writes
// that change nothing clients see (lease renewals, repeated probe outcomes)
// keep the cache; a relay the cache does not hold cannot be served stale.
func (c *rankingCache) invalidateIfChanged(id string, changed func(cached relay.Descriptor) bool) {
	snap := c.snap.Load()
	if snap == nil {
		return
	}
	for _, cached := range snap.relays {
		if cached.ID == id {
			if changed(cached) {
				c.invalidate()
			}
			return
		}
	}
}
