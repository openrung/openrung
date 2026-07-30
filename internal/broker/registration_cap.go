package broker

import (
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	// defaultMaxNewRelayIDsPerDay is the per-source budget of NEW relay
	// identities per newRelayCapWindow when Config leaves the cap unset. It is
	// sized for the worst legitimate shape — a flapping legacy relay that mints
	// a fresh random ID on every successful re-registration — while shrinking
	// what one address can register from millions per day (the short-window
	// rate limit alone) to dozens. Reconnects of a known identity (tunnel
	// relays via the hub, stable-identity restarts) never count.
	defaultMaxNewRelayIDsPerDay = 64
	newRelayCapWindow           = 24 * time.Hour

	// newRelayCapRetryAfterSeconds is deliberately long: the budget refills on
	// a day-scale window, so steering the engine's Retry-After-honouring
	// backoff to the hour scale avoids a day of pointless retries.
	newRelayCapRetryAfterSeconds = 3600
)

// newRelayCap bounds how many distinct NEW relay identities one source may
// successfully register per fixed window. It exists because with anonymous
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
	buckets map[string]*newRelayCapBucket
}

type newRelayCapBucket struct {
	count       int
	windowStart time.Time
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
		buckets: make(map[string]*newRelayCapBucket),
	}
}

// allows reports whether source may register one more new relay identity now.
// It never consumes budget: the caller records the registration only after the
// store accepts it, so a relay crash-looping on a rejected request (bad proof,
// invalid endpoint, WSS misconfiguration) cannot burn through its own budget
// before the operator fixes it.
func (c *newRelayCap) allows(source string, now time.Time) bool {
	if c.limit < 0 {
		return true
	}
	key, exempt := c.key(source)
	if exempt {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	bucket, ok := c.buckets[key]
	if !ok || now.Sub(bucket.windowStart) >= c.window {
		return c.limit > 0
	}
	return bucket.count < c.limit
}

// record consumes one unit of source's budget for a successfully registered
// new identity. Concurrent in-flight registrations can finish slightly over
// the limit; the cap is a volume bound, not an exact quota.
func (c *newRelayCap) record(source string, now time.Time) {
	if c.limit < 0 {
		return
	}
	key, exempt := c.key(source)
	if exempt {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	bucket, ok := c.buckets[key]
	if ok && now.Sub(bucket.windowStart) < c.window {
		bucket.count++
		return
	}
	if !ok && len(c.buckets) >= c.maxKeys {
		c.sweepLocked(now)
		if len(c.buckets) >= c.maxKeys {
			// Fail open like ipRateLimiter: refusing every registration because
			// an attacker rotated through more sources than the table holds
			// would be a worse outage than the flood itself.
			return
		}
	}
	c.buckets[key] = &newRelayCapBucket{count: 1, windowStart: now}
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

func (c *newRelayCap) sweepLocked(now time.Time) {
	for key, bucket := range c.buckets {
		if now.Sub(bucket.windowStart) >= c.window {
			delete(c.buckets, key)
		}
	}
}
