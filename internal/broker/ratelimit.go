package broker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Default per-IP limits for the unauthenticated endpoints. They only need to
// stop a single source from monopolizing broker resources; volumetric floods
// from many sources are the edge proxy's (Cloudflare's) job. Buckets are
// generous because carrier-grade NAT can put whole networks behind one client
// IP. Config.RateLimits overrides each one (cmd/broker:
// OPENRUNG_RATE_LIMIT_*).
const (
	defaultRelayListRatePerSecond = 2.0
	defaultRelayListBurst         = 30
	defaultWSSTicketRatePerSecond = 2.0
	defaultWSSTicketBurst         = 30

	defaultTelemetryRatePerSecond = 1.0
	defaultTelemetryBurst         = 20

	// User challenge/auth: a legitimate installation authenticates about once
	// a day, so even a carrier-grade NAT needs little sustained rate; the
	// burst covers a building's worth of first launches after a release.
	defaultUserAuthRatePerSecond = 1.0
	defaultUserAuthBurst         = 20

	// Client access: a connect makes one or two requests (one more per
	// recovery), a connected client renews about twice per lease lifetime.
	// Keyed per address and user, so the bucket is one installation's.
	defaultAccessRatePerSecond = 0.5
	defaultAccessBurst         = 10

	// Relay registration/heartbeat are token-gated in production; this limiter
	// is a backstop against a flood if the token leaks or the broker is
	// deliberately run open. Generous: one relay hub re-registers and heartbeats
	// every relay it fronts (a whole port range) from a single origin IP.
	defaultRelayRegistrationRatePerSecond = 20.0
	defaultRelayRegistrationBurst         = 256

	// A full speed test streams up to 25 MB, so sustained refills are slow and
	// the real egress bound is the concurrency cap below.
	defaultSpeedTestRatePerSecond = 1.0 / 30
	defaultSpeedTestBurst         = 3
	speedTestMaxConcurrent        = 4

	// rateLimiterMaxTrackedIPs bounds limiter memory (~100 B per tracked IP).
	rateLimiterMaxTrackedIPs = 100_000
)

// RateLimit is one endpoint's per-client token bucket: Burst requests at
// once, refilled at PerSecond. The zero value applies the endpoint's default.
type RateLimit struct {
	PerSecond float64
	Burst     int
}

// RateLimits holds the per-client budgets of the rate-limited public
// endpoints; each zero field applies that endpoint's default.
type RateLimits struct {
	RelayList         RateLimit
	WSSTicket         RateLimit
	Telemetry         RateLimit
	UserAuth          RateLimit
	Access            RateLimit
	RelayRegistration RateLimit
	SpeedTest         RateLimit
}

func (l RateLimits) withDefaults() RateLimits {
	return RateLimits{
		RelayList:         l.RelayList.or(defaultRelayListRatePerSecond, defaultRelayListBurst),
		WSSTicket:         l.WSSTicket.or(defaultWSSTicketRatePerSecond, defaultWSSTicketBurst),
		Telemetry:         l.Telemetry.or(defaultTelemetryRatePerSecond, defaultTelemetryBurst),
		UserAuth:          l.UserAuth.or(defaultUserAuthRatePerSecond, defaultUserAuthBurst),
		Access:            l.Access.or(defaultAccessRatePerSecond, defaultAccessBurst),
		RelayRegistration: l.RelayRegistration.or(defaultRelayRegistrationRatePerSecond, defaultRelayRegistrationBurst),
		SpeedTest:         l.SpeedTest.or(defaultSpeedTestRatePerSecond, defaultSpeedTestBurst),
	}
}

func (l RateLimit) or(perSecond float64, burst int) RateLimit {
	if l == (RateLimit{}) {
		return RateLimit{PerSecond: perSecond, Burst: burst}
	}
	return l
}

func (l RateLimit) limiter() *ipRateLimiter {
	return newIPRateLimiter(l.PerSecond, float64(l.Burst), rateLimiterMaxTrackedIPs)
}

// ParseRateLimit reads a "<per-second rate>:<burst>" budget such as "2:30";
// empty means the zero RateLimit (the endpoint's default).
func ParseRateLimit(value string) (RateLimit, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return RateLimit{}, nil
	}
	rawRate, rawBurst, ok := strings.Cut(value, ":")
	if !ok {
		return RateLimit{}, errors.New("must be <per-second rate>:<burst>, like 2:30")
	}
	rate, err := strconv.ParseFloat(strings.TrimSpace(rawRate), 64)
	if err != nil || !(rate > 0) || math.IsInf(rate, 0) {
		return RateLimit{}, errors.New("rate must be a positive number of requests per second")
	}
	burst, err := strconv.Atoi(strings.TrimSpace(rawBurst))
	if err != nil || burst < 1 {
		return RateLimit{}, errors.New("burst must be a positive integer")
	}
	return RateLimit{PerSecond: rate, Burst: burst}, nil
}

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
	allowed, _ := l.take(key)
	return allowed
}

// take spends one token from key's bucket. When the bucket is empty it
// reports how long until the next token refills.
func (l *ipRateLimiter) take(key string) (bool, time.Duration) {
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
			return true, 0
		}
		bucket = &tokenBucket{tokens: l.burst, lastSeen: now}
		l.buckets[key] = bucket
	}

	if elapsed := now.Sub(bucket.lastSeen).Seconds(); elapsed > 0 {
		bucket.tokens = min(l.burst, bucket.tokens+elapsed*l.ratePerSecond)
		bucket.lastSeen = now
	}
	if bucket.tokens < 1 {
		return false, time.Duration((1 - bucket.tokens) / l.ratePerSecond * float64(time.Second))
	}
	bucket.tokens--
	return true, 0
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
	return rateLimitedBy(limiter, func(r *http.Request) string { return clientIP.clientIP(r) }, retryAfterSeconds, next)
}

func rateLimitedBy(limiter *ipRateLimiter, key func(*http.Request) string, retryAfterSeconds int, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if allowed, refill := limiter.take(key(r)); !allowed {
			// A 429 is per-client state: a shared cache (Cloudflare's edge)
			// storing one would replay it to every client behind the edge.
			w.Header().Set("Cache-Control", "no-store")
			// retryAfterSeconds is a floor; a slow configured refill must not
			// send clients back before their next token exists.
			w.Header().Set("Retry-After", strconv.Itoa(max(retryAfterSeconds, int(math.Ceil(refill.Seconds())))))
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded, retry later")
			return
		}
		next(w, r)
	}
}

// wssTicketRateKey separates well-formed pseudonymous clients that share a
// carrier-grade NAT address. The identifier is not authentication and may be
// rotated by an attacker, so sidecar/CDN admission controls remain mandatory.
// Hashing avoids retaining the raw client identifier in broker memory.
func wssTicketRateKey(clientIP *clientIPResolver) func(*http.Request) string {
	return func(r *http.Request) string {
		source := clientIP.clientIP(r)
		values := r.Header.Values("X-OpenRung-Client-ID")
		if len(values) != 1 {
			return source
		}
		clientID := values[0]
		if len(clientID) < 1 || len(clientID) > 128 || strings.TrimSpace(clientID) != clientID {
			return source
		}
		for _, b := range []byte(clientID) {
			if b < 0x21 || b > 0x7e {
				return source
			}
		}
		digest := sha256.Sum256([]byte(clientID))
		return source + "\x00" + hex.EncodeToString(digest[:16])
	}
}
