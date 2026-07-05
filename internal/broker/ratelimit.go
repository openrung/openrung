package broker

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Per-IP limits for the unauthenticated endpoints. They only need to stop a
// single source from monopolizing broker resources; volumetric floods from
// many sources are the edge proxy's (Cloudflare's) job. Buckets are generous
// because carrier-grade NAT can put whole networks behind one client IP.
const (
	relayListRatePerSecond = 2.0
	relayListBurst         = 30

	telemetryRatePerSecond = 1.0
	telemetryBurst         = 20

	// A full speed test streams up to 25 MB, so sustained refills are slow and
	// the real egress bound is the concurrency cap below.
	speedTestRatePerSecond = 1.0 / 30
	speedTestBurst         = 3
	speedTestMaxConcurrent = 4

	// rateLimiterMaxTrackedIPs bounds limiter memory (~100 B per tracked IP).
	rateLimiterMaxTrackedIPs = 100_000
)

// ipRateLimiter is a token-bucket limiter keyed by client IP.
type ipRateLimiter struct {
	ratePerSecond float64
	burst         float64
	maxKeys       int
	now           func() time.Time

	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

type tokenBucket struct {
	tokens   float64
	lastSeen time.Time
}

func newIPRateLimiter(ratePerSecond, burst float64, maxKeys int) *ipRateLimiter {
	return &ipRateLimiter{
		ratePerSecond: ratePerSecond,
		burst:         burst,
		maxKeys:       maxKeys,
		now:           time.Now,
		buckets:       make(map[string]*tokenBucket),
	}
}

func (l *ipRateLimiter) allow(key string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxKeys {
			l.sweepLocked(now)
		}
		if len(l.buckets) >= l.maxKeys {
			// Fail open: an attacker rotating through more source IPs than the
			// table holds would otherwise turn the limiter into a DoS against
			// legitimate clients. Multi-source floods are handled at the edge.
			return true
		}
		bucket = &tokenBucket{tokens: l.burst, lastSeen: now}
		l.buckets[key] = bucket
	}

	if elapsed := now.Sub(bucket.lastSeen).Seconds(); elapsed > 0 {
		bucket.tokens = min(l.burst, bucket.tokens+elapsed*l.ratePerSecond)
		bucket.lastSeen = now
	}
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}

// sweepLocked drops buckets that have been idle long enough to refill
// completely; forgetting those loses nothing because a recreated bucket
// starts full anyway.
func (l *ipRateLimiter) sweepLocked(now time.Time) {
	for key, bucket := range l.buckets {
		if now.Sub(bucket.lastSeen).Seconds()*l.ratePerSecond >= l.burst-bucket.tokens {
			delete(l.buckets, key)
		}
	}
}

// rateLimited rejects requests over the per-IP budget with 429 before they
// reach next. The key comes from the trusted-proxy-aware resolver so limits
// apply to real client IPs, not to Cloudflare's.
func rateLimited(limiter *ipRateLimiter, clientIP *clientIPResolver, retryAfterSeconds int, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !limiter.allow(clientIP.clientIP(r)) {
			// A 429 is per-client state: a shared cache (Cloudflare's edge)
			// storing one would replay it to every client behind the edge.
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded, retry later")
			return
		}
		next(w, r)
	}
}
