package broker

import (
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	// defaultMaxNewRelayIDsPerDay is the per-source budget of NEW relay
	// identities per trailing newRelayCapWindow when Config leaves the cap
	// unset. It is sized for the worst legitimate shape — a flapping legacy
	// relay that mints a fresh random ID on every successful re-registration —
	// while shrinking what one address can register from millions per day (the
	// short-window rate limit alone) to dozens. Reconnects of a known identity
	// (tunnel relays via the hub, stable-identity restarts) never count.
	defaultMaxNewRelayIDsPerDay = 64
	newRelayCapWindow           = 24 * time.Hour

	// newRelayCapRetryAfterSeconds is deliberately long: the budget refills on
	// a day-scale window, so steering the engine's Retry-After-honouring
	// backoff to the hour scale avoids a day of pointless retries.
	newRelayCapRetryAfterSeconds = 3600
)

// newRelayCap bounds how many distinct NEW relay identities one source may
// successfully register per trailing window. It exists because with anonymous
// registration enabled the short-window rate limiter still allows a single
// address to mint relay identities by the thousand, each of which becomes an
// attested telemetry subject and a public directory row. The cap keys on the
// source IP (IPv6 grouped by /64, since one host trivially holds a whole /64);
// infrastructure that legitimately fronts many first-time registrants — the
// relay hub above all — is exempted by CIDR via Config.
type newRelayCap struct {
	limit  int
	window time.Duration
	exempt []netip.Prefix

	mu      sync.Mutex
	maxKeys int
	// sources holds, per accounting key, the UnixMilli stamps of the window's
	// reserved registrations — an exact sliding window, so a trailing-window
	// count can never exceed the limit (a fixed window that resets would admit
	// up to twice the limit inside one trailing interval). Each source stores
	// at most `limit` live stamps; worst-case memory is
	// maxKeys × limit × 8 B, and reaching it requires that many *successful*
	// registrations inside one window.
	sources map[string][]int64
}

func newNewRelayCap(limit int, window time.Duration, exemptCIDRs []string) *newRelayCap {
	if limit == 0 {
		limit = defaultMaxNewRelayIDsPerDay
	}
	exempt := make([]netip.Prefix, 0, len(exemptCIDRs))
	for _, raw := range exemptCIDRs {
		if p, err := netip.ParsePrefix(strings.TrimSpace(raw)); err == nil {
			exempt = append(exempt, p)
		}
	}
	return &newRelayCap{
		limit:   limit,
		window:  window,
		exempt:  exempt,
		maxKeys: rateLimiterMaxTrackedIPs,
		sources: make(map[string][]int64),
	}
}

// reserve atomically claims one unit of source's budget, or reports the
// budget exhausted. Claiming before the store call (rather than checking
// first and recording after) is what makes the limit hold under concurrent
// registrations. The returned release hands the unit back — the caller
// invokes it when the registration ultimately fails, so a relay crash-looping
// on a rejected request (bad proof, invalid endpoint, WSS misconfiguration)
// cannot burn through its own budget before the operator fixes it. release is
// idempotent and always non-nil.
func (c *newRelayCap) reserve(source string, now time.Time) (release func(), ok bool) {
	noop := func() {}
	if c.limit < 0 {
		return noop, true
	}
	key, exempt := c.key(source)
	if exempt {
		return noop, true
	}
	stamp := now.UnixMilli()
	cutoff := stamp - c.window.Milliseconds()

	c.mu.Lock()
	defer c.mu.Unlock()
	entries, tracked := c.sources[key]
	kept := entries[:0]
	for _, at := range entries {
		if at > cutoff {
			kept = append(kept, at)
		}
	}
	if len(kept) >= c.limit {
		c.sources[key] = kept
		return noop, false
	}
	if !tracked && len(c.sources) >= c.maxKeys {
		c.sweepLocked(cutoff)
		if len(c.sources) >= c.maxKeys {
			// Fail open like ipRateLimiter: refusing every registration because
			// an attacker rotated through more sources than the table holds
			// would be a worse outage than the flood itself.
			return noop, true
		}
	}
	c.sources[key] = append(kept, stamp)

	released := false
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if released {
			return
		}
		released = true
		// Return one unit carrying this reservation's stamp. Every unit is
		// interchangeable, so removing any equal stamp is equivalent; if the
		// window already expired it, there is nothing to return.
		remaining := c.sources[key]
		for i := len(remaining) - 1; i >= 0; i-- {
			if remaining[i] == stamp {
				c.sources[key] = append(remaining[:i], remaining[i+1:]...)
				return
			}
		}
	}, true
}

// key collapses the source to its accounting bucket, or reports it exempt.
// Unparseable sources (unexpected from the client-IP resolver, but possible
// with unusual proxy configurations) are bucketed by their raw string so they
// are still capped rather than waved through.
func (c *newRelayCap) key(source string) (string, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(source))
	if err != nil {
		return source, false
	}
	addr = addr.Unmap()
	for _, prefix := range c.exempt {
		if prefix.Contains(addr) {
			return "", true
		}
	}
	if addr.Is6() {
		grouped, err := addr.Prefix(64)
		if err == nil {
			return grouped.String(), false
		}
	}
	return addr.String(), false
}

// sweepLocked drops sources whose every stamp has aged out of the window.
func (c *newRelayCap) sweepLocked(cutoff int64) {
	for key, entries := range c.sources {
		live := false
		for _, at := range entries {
			if at > cutoff {
				live = true
				break
			}
		}
		if !live {
			delete(c.sources, key)
		}
	}
}
