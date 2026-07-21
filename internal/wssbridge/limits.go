package wssbridge

import (
	"math"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// The global limiter is an overload backstop independent of source
	// attribution. The per-source defaults remain deliberately generous for
	// carrier-grade NAT, while this cap bounds aggregate handshake work.
	DefaultGlobalHandshakeRatePerSecond = 2_000.0
	DefaultGlobalHandshakeBurst         = 10_000
	DefaultHandshakeRatePerSecond       = 20.0
	DefaultHandshakeBurst               = 200
	DefaultMaxTrackedSources            = 100_000
	// Defaults are intentionally generous for carrier-grade NAT. Operators can
	// tune them, while global caps remain the primary overload boundary.
	DefaultMaxSessionsPerSource = 512
	DefaultMaxStreamsPerSource  = 4096
	DefaultNoStreamIdleTimeout  = 2 * time.Minute

	maxTrackedSources   = 1_000_000
	maxTokenRefillDelay = 100 * 365 * 24 * time.Hour
)

// SidecarStats contains label-free aggregates only. It never stores source
// addresses, front IDs, relay IDs, ticket IDs, targets, or payload data.
type SidecarStats struct {
	acceptedSessions atomic.Uint64
	currentSessions  atomic.Int64
	acceptedStreams  atomic.Uint64
	currentStreams   atomic.Int64

	originAuthRejections           atomic.Uint64
	viewerAddressRejections        atomic.Uint64
	protocolRejections             atomic.Uint64
	globalHandshakeRateRejections  atomic.Uint64
	handshakeRateRejections        atomic.Uint64
	handshakeConcurrencyRejections atomic.Uint64
	handshakeLimiterFailOpen       atomic.Uint64
	ticketRejections               atomic.Uint64
	frontRejections                atomic.Uint64
	replayRejections               atomic.Uint64
	replayStoreFailures            atomic.Uint64
	sessionLimitRejections         atomic.Uint64
	sourceSessionRejections        atomic.Uint64
	streamLimitRejections          atomic.Uint64
	sourceStreamRejections         atomic.Uint64
	upgradeFailures                atomic.Uint64
	dialFailures                   atomic.Uint64
	idleSessionCloses              atomic.Uint64
	lifetimeSessionCloses          atomic.Uint64
}

type SidecarSnapshot struct {
	AcceptedSessions uint64
	CurrentSessions  int64
	AcceptedStreams  uint64
	CurrentStreams   int64

	OriginAuthRejections           uint64
	ViewerAddressRejections        uint64
	ProtocolRejections             uint64
	GlobalHandshakeRateRejections  uint64
	HandshakeRateRejections        uint64
	HandshakeConcurrencyRejections uint64
	HandshakeLimiterFailOpen       uint64
	TicketRejections               uint64
	FrontRejections                uint64
	ReplayRejections               uint64
	ReplayStoreFailures            uint64
	SessionLimitRejections         uint64
	SourceSessionRejections        uint64
	StreamLimitRejections          uint64
	SourceStreamRejections         uint64
	UpgradeFailures                uint64
	DialFailures                   uint64
	IdleSessionCloses              uint64
	LifetimeSessionCloses          uint64
}

func (s *SidecarStats) Snapshot() SidecarSnapshot {
	if s == nil {
		return SidecarSnapshot{}
	}
	return SidecarSnapshot{
		AcceptedSessions: s.acceptedSessions.Load(), CurrentSessions: s.currentSessions.Load(),
		AcceptedStreams: s.acceptedStreams.Load(), CurrentStreams: s.currentStreams.Load(),
		OriginAuthRejections:           s.originAuthRejections.Load(),
		ViewerAddressRejections:        s.viewerAddressRejections.Load(),
		ProtocolRejections:             s.protocolRejections.Load(),
		GlobalHandshakeRateRejections:  s.globalHandshakeRateRejections.Load(),
		HandshakeRateRejections:        s.handshakeRateRejections.Load(),
		HandshakeConcurrencyRejections: s.handshakeConcurrencyRejections.Load(),
		HandshakeLimiterFailOpen:       s.handshakeLimiterFailOpen.Load(),
		TicketRejections:               s.ticketRejections.Load(), FrontRejections: s.frontRejections.Load(),
		ReplayRejections: s.replayRejections.Load(), ReplayStoreFailures: s.replayStoreFailures.Load(),
		SessionLimitRejections:  s.sessionLimitRejections.Load(),
		SourceSessionRejections: s.sourceSessionRejections.Load(),
		StreamLimitRejections:   s.streamLimitRejections.Load(),
		SourceStreamRejections:  s.sourceStreamRejections.Load(),
		UpgradeFailures:         s.upgradeFailures.Load(), DialFailures: s.dialFailures.Load(),
		IdleSessionCloses:     s.idleSessionCloses.Load(),
		LifetimeSessionCloses: s.lifetimeSessionCloses.Load(),
	}
}

type sourceRateLimiter struct {
	rate    float64
	burst   float64
	maxKeys int
	now     func() time.Time

	mu        sync.Mutex
	buckets   map[string]*sourceTokenBucket
	nextSweep time.Time
}

type sourceTokenBucket struct {
	tokens   float64
	lastSeen time.Time
}

func newSourceRateLimiter(rate float64, burst, maxKeys int) *sourceRateLimiter {
	return &sourceRateLimiter{
		rate: rate, burst: float64(burst), maxKeys: maxKeys, now: time.Now,
		buckets: make(map[string]*sourceTokenBucket),
	}
}

func (l *sourceRateLimiter) allow(key string) (allowed, failOpen bool) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxKeys && (l.nextSweep.IsZero() || !now.Before(l.nextSweep)) {
			l.sweepLocked(now)
		}
		if len(l.buckets) >= l.maxKeys {
			return true, true
		}
		bucket = &sourceTokenBucket{tokens: l.burst, lastSeen: now}
		l.buckets[key] = bucket
	}
	if elapsed := now.Sub(bucket.lastSeen).Seconds(); elapsed > 0 {
		bucket.tokens = math.Min(l.burst, bucket.tokens+elapsed*l.rate)
		bucket.lastSeen = now
	}
	if bucket.tokens < 1 {
		return false, false
	}
	bucket.tokens--
	if len(l.buckets) >= l.maxKeys {
		l.noteRefillLocked(bucket)
	}
	return true, false
}

func (l *sourceRateLimiter) sweepLocked(now time.Time) {
	l.nextSweep = time.Time{}
	for key, bucket := range l.buckets {
		if now.Sub(bucket.lastSeen).Seconds()*l.rate >= l.burst-bucket.tokens {
			delete(l.buckets, key)
			continue
		}
		l.noteRefillLocked(bucket)
	}
}

func (l *sourceRateLimiter) noteRefillLocked(bucket *sourceTokenBucket) {
	missing := l.burst - bucket.tokens
	delaySeconds := missing / l.rate
	var delay time.Duration
	if delaySeconds >= maxTokenRefillDelay.Seconds() {
		delay = maxTokenRefillDelay
	} else {
		delay = time.Duration(math.Ceil(delaySeconds * float64(time.Second)))
	}
	ready := bucket.lastSeen.Add(delay)
	if l.nextSweep.IsZero() || ready.Before(l.nextSweep) {
		l.nextSweep = ready
	}
}

type sourceUsage struct {
	mu          sync.Mutex
	entries     map[string]*sourceUsageEntry
	maxSessions int
	maxStreams  int
}

type sourceUsageEntry struct{ sessions, streams int }

func newSourceUsage(maxSessions, maxStreams int) *sourceUsage {
	return &sourceUsage{entries: make(map[string]*sourceUsageEntry), maxSessions: maxSessions, maxStreams: maxStreams}
}

func (u *sourceUsage) acquireSession(key string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	entry := u.entries[key]
	if entry == nil {
		entry = &sourceUsageEntry{}
		u.entries[key] = entry
	}
	if entry.sessions >= u.maxSessions {
		u.deleteEmptyLocked(key, entry)
		return false
	}
	entry.sessions++
	return true
}

func (u *sourceUsage) releaseSession(key string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	entry := u.entries[key]
	if entry == nil || entry.sessions == 0 {
		return
	}
	entry.sessions--
	u.deleteEmptyLocked(key, entry)
}

func (u *sourceUsage) acquireStream(key string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	entry := u.entries[key]
	if entry == nil {
		entry = &sourceUsageEntry{}
		u.entries[key] = entry
	}
	if entry.streams >= u.maxStreams {
		u.deleteEmptyLocked(key, entry)
		return false
	}
	entry.streams++
	return true
}

func (u *sourceUsage) releaseStream(key string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	entry := u.entries[key]
	if entry == nil || entry.streams == 0 {
		return
	}
	entry.streams--
	u.deleteEmptyLocked(key, entry)
}

func (u *sourceUsage) deleteEmptyLocked(key string, entry *sourceUsageEntry) {
	if entry.sessions == 0 && entry.streams == 0 {
		delete(u.entries, key)
	}
}

type sessionIdleGuard struct {
	mu         sync.Mutex
	timeout    time.Duration
	active     int
	expired    bool
	closed     bool
	generation uint64
	timer      *time.Timer
	onExpire   func()
}

func newSessionIdleGuard(timeout time.Duration, onExpire func()) *sessionIdleGuard {
	g := &sessionIdleGuard{timeout: timeout, onExpire: onExpire}
	g.scheduleLocked()
	return g
}

func (g *sessionIdleGuard) startStream() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.expired {
		return false
	}
	if g.active == 0 {
		g.generation++
		if g.timer != nil {
			g.timer.Stop()
		}
	}
	g.active++
	return true
}

func (g *sessionIdleGuard) endStream() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.active == 0 {
		return
	}
	g.active--
	if g.active == 0 {
		g.scheduleLocked()
	}
}

func (g *sessionIdleGuard) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	g.generation++
	if g.timer != nil {
		g.timer.Stop()
	}
}

func (g *sessionIdleGuard) scheduleLocked() {
	g.generation++
	generation := g.generation
	g.timer = time.AfterFunc(g.timeout, func() { g.expire(generation) })
}

func (g *sessionIdleGuard) expire(generation uint64) {
	g.mu.Lock()
	if g.closed || g.expired || g.active != 0 || generation != g.generation {
		g.mu.Unlock()
		return
	}
	g.expired = true
	onExpire := g.onExpire
	g.mu.Unlock()
	if onExpire != nil {
		onExpire()
	}
}

func parseViewerAddress(values []string) (string, bool) {
	if len(values) != 1 || values[0] == "" {
		return "", false
	}
	address, err := netip.ParseAddrPort(values[0])
	if err != nil || address.Port() == 0 || address.Addr().Zone() != "" {
		return "", false
	}
	return address.Addr().Unmap().String(), true
}
