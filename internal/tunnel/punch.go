package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/openrung/openrung/punchcore"
)

// punchControlTimeout bounds the hub<->relay punch-control exchange over the
// yamux stream.
const punchControlTimeout = 5 * time.Second

// DefaultPunchTTL is the punch time budget the coordinator hands to both peers.
const DefaultPunchTTL = 6 * time.Second

// hubPolicy: the hub sanitizes with the historical server-side profile.
var hubPolicy = punchcore.DesktopPolicy()

var (
	// ErrRelayNotConnected means no live tunnel is registered for the relay ID
	// (stale/rotated descriptor); the client should re-fetch relays.
	ErrRelayNotConnected = errors.New("relay not connected to hub")
	// ErrPunchUnsupported means the connected relay did not negotiate stream
	// typing and cannot receive punch directives.
	ErrPunchUnsupported = errors.New("relay does not support punch")
)

func (h *Hub) addTunnel(relayID string, t *tunnel) {
	h.registryMu.Lock()
	defer h.registryMu.Unlock()
	if h.registry == nil {
		h.registry = make(map[string]*tunnel)
	}
	h.registry[relayID] = t
}

// removeTunnel deletes the entry only if it is still this tunnel, so a fast
// reconnect that already installed a new tunnel under a new relay ID is never
// evicted by the old tunnel's teardown.
func (h *Hub) removeTunnel(relayID string, t *tunnel) {
	h.registryMu.Lock()
	defer h.registryMu.Unlock()
	if h.registry[relayID] == t {
		delete(h.registry, relayID)
	}
}

func (h *Hub) lookupTunnel(relayID string) *tunnel {
	h.registryMu.RLock()
	defer h.registryMu.RUnlock()
	return h.registry[relayID]
}

// SendPunchDirective pushes a punch directive to the connected relay over its
// existing control connection and returns the relay's ack. It re-looks up the
// live tunnel at send time so a reconnect between coordination steps is handled
// cleanly.
func (h *Hub) SendPunchDirective(ctx context.Context, relayID string, dir punchcore.PunchDirective) (punchcore.PunchAck, error) {
	t := h.lookupTunnel(relayID)
	if t == nil {
		return punchcore.PunchAck{}, ErrRelayNotConnected
	}
	if !t.streamTyping {
		return punchcore.PunchAck{}, ErrPunchUnsupported
	}

	stream, err := t.session.Open()
	if err != nil {
		return punchcore.PunchAck{}, err
	}
	defer stream.Close()

	deadline := time.Now().Add(punchControlTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = stream.SetDeadline(deadline)

	if _, err := stream.Write([]byte{StreamTypeControl}); err != nil {
		return punchcore.PunchAck{}, err
	}
	if err := writeFrame(stream, dir); err != nil {
		return punchcore.PunchAck{}, err
	}
	var ack punchcore.PunchAck
	if err := readFrame(stream, &ack); err != nil {
		return punchcore.PunchAck{}, err
	}
	return ack, nil
}

// PunchCoordinator is the hub's HTTP-facing punch rendezvous. It lives on the
// hub's own listener (not the broker, not Cloudflare-fronted), so it carries its
// own rate limiting and session caps.
type PunchCoordinator struct {
	Hub       *Hub
	Reflector *punchcore.Reflector
	TTL       time.Duration
	Logger    *slog.Logger

	secret   []byte
	limiter  *punchLimiter
	inflight *inflightCap
}

// NewPunchCoordinator builds a coordinator with a fresh per-process HMAC secret.
func NewPunchCoordinator(hub *Hub, reflector *punchcore.Reflector, ttl time.Duration, logger *slog.Logger) (*PunchCoordinator, error) {
	if hub == nil || reflector == nil {
		return nil, errors.New("punch coordinator requires a hub and reflector")
	}
	if ttl <= 0 {
		ttl = DefaultPunchTTL
	}
	if logger == nil {
		logger = slog.Default()
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	return &PunchCoordinator{
		Hub:       hub,
		Reflector: reflector,
		TTL:       ttl,
		Logger:    logger,
		secret:    secret,
		limiter:   newPunchLimiter(5, 10), // ~5 req/s per (ip,relay), burst 10
		inflight:  newInflightCap(4, 8),   // <=4 concurrent per relay, <=8 per client IP
	}, nil
}

// Register mounts the hub punch HTTP routes on mux.
func (c *PunchCoordinator) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+punchcore.PathPunchConfig, c.handleConfig)
	mux.HandleFunc("POST "+punchcore.PathPunchRequest, c.handleRequest)
	mux.HandleFunc("POST "+punchcore.PathPunchResult, c.handleResult)
}

func (c *PunchCoordinator) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSONResponse(w, http.StatusOK, punchcore.PunchConfig{
		ReflectorAddrs: c.Hub.ReflectorAddrs,
		ALPN:           punchcore.ALPN,
		TTLMillis:      c.TTL.Milliseconds(),
	})
}

func (c *PunchCoordinator) handleRequest(w http.ResponseWriter, r *http.Request) {
	ip := requestIP(r)

	var req punchcore.PunchRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSONResponse(w, http.StatusBadRequest, punchcore.PunchResponse{OK: false, Error: "invalid request"})
		return
	}
	if req.RelayID == "" || req.ClientNonce == "" {
		writeJSONResponse(w, http.StatusBadRequest, punchcore.PunchResponse{OK: false, Error: "relay_id and client_nonce required"})
		return
	}
	if req.QUICALPN != "" && req.QUICALPN != punchcore.ALPN {
		writeJSONResponse(w, http.StatusBadRequest, punchcore.PunchResponse{OK: false, Error: "unsupported alpn"})
		return
	}

	if !c.limiter.allow(ip + "|" + req.RelayID) {
		writeJSONResponse(w, http.StatusTooManyRequests, punchcore.PunchResponse{OK: false, Error: "rate limited"})
		return
	}
	if !c.inflight.acquire(req.RelayID, ip) {
		writeJSONResponse(w, http.StatusTooManyRequests, punchcore.PunchResponse{OK: false, Error: "too many in-flight punch sessions"})
		return
	}
	defer c.inflight.release(req.RelayID, ip)

	t := c.Hub.lookupTunnel(req.RelayID)
	if t == nil {
		// Stale/rotated relay id: tell the client to re-fetch relays.
		writeJSONResponse(w, http.StatusNotFound, punchcore.PunchResponse{OK: false, Error: "relay not connected"})
		return
	}
	if !t.streamTyping {
		writeJSONResponse(w, http.StatusConflict, punchcore.PunchResponse{OK: false, Error: "relay not punch capable"})
		return
	}

	// Classify the client from the hub's OWN reflector observations (keyed by the
	// client nonce), never trusting the client's self-declared class or reflexive
	// address. A client-declared reflexive could name any victim IP, which the
	// relay would then spray with punch probes — an open UDP reflector. So we
	// forward ONLY reflector-observed reflexive endpoints; when the reflector saw
	// nothing for this nonce we send none (the client falls back to the hub
	// relay). Host candidates are clamped and filtered to non-routable addresses
	// (see punchcore.Policy.SanitizePeers) so they can only reach the relay's
	// own LAN.
	var clientReflexive []punchcore.Endpoint
	clientClass := punchcore.ClassUnknown
	if key, err := punchcore.NonceKey(req.ClientNonce); err == nil {
		if class, reflexive, ok := c.Reflector.Classify(key); ok {
			clientClass = class
			clientReflexive = hubPolicy.SanitizePeers(reflexive)
		}
	}

	sessionID, err := randomHex(16)
	if err != nil {
		writeJSONResponse(w, http.StatusInternalServerError, punchcore.PunchResponse{OK: false, Error: "internal error"})
		return
	}
	token := punchcore.ComputeToken(c.secret, sessionID, req.RelayID, req.ClientNonce)
	tokenHex := punchcore.EncodeToken(token)

	dir := punchcore.PunchDirective{
		SessionID:       sessionID,
		RelayID:         req.RelayID,
		ClientReflexive: clientReflexive,
		ClientLocal:     hubPolicy.SanitizePeers(req.ClientLocal),
		ClientClass:     clientClass,
		PunchToken:      tokenHex,
		ReflectorAddrs:  c.Hub.ReflectorAddrs,
		TTLMillis:       c.TTL.Milliseconds(),
		QUICALPN:        punchcore.ALPN,
		ProtoVersion:    punchcore.ProtoVersion,
	}

	ctx, cancel := context.WithTimeout(r.Context(), punchControlTimeout)
	defer cancel()
	ack, err := c.Hub.SendPunchDirective(ctx, req.RelayID, dir)
	if err != nil {
		if errors.Is(err, ErrRelayNotConnected) {
			writeJSONResponse(w, http.StatusNotFound, punchcore.PunchResponse{OK: false, Error: "relay not connected"})
			return
		}
		if errors.Is(err, ErrPunchUnsupported) {
			writeJSONResponse(w, http.StatusConflict, punchcore.PunchResponse{OK: false, Error: "relay not punch capable"})
			return
		}
		c.Logger.Warn("punch directive failed", "relay_id", req.RelayID, "error", err)
		writeJSONResponse(w, http.StatusOK, punchcore.PunchResponse{OK: false, Error: "relay unreachable"})
		return
	}
	if !ack.OK {
		writeJSONResponse(w, http.StatusOK, punchcore.PunchResponse{OK: false, Error: "relay declined: " + ack.Error})
		return
	}

	// Skip when both ends are symmetric: no port-prediction, so a direct path is
	// not worth the budget — the client stays on the hub relay.
	if clientClass == punchcore.ClassSymmetric && ack.VolunteerClass == punchcore.ClassSymmetric {
		writeJSONResponse(w, http.StatusOK, punchcore.PunchResponse{OK: false, Error: "skip: both symmetric"})
		return
	}

	writeJSONResponse(w, http.StatusOK, punchcore.PunchResponse{
		OK:                 true,
		SessionID:          sessionID,
		VolunteerReflexive: ack.VolunteerReflexive,
		VolunteerLocal:     ack.VolunteerLocal,
		VolunteerClass:     ack.VolunteerClass,
		PunchToken:         tokenHex,
		CertFingerprint:    ack.CertFingerprint,
		TTLMillis:          c.TTL.Milliseconds(),
	})
}

func (c *PunchCoordinator) handleResult(w http.ResponseWriter, r *http.Request) {
	var res punchcore.PunchResult
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&res); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	c.Logger.Info("punch result",
		"session", res.SessionID, "ok", res.OK, "reason", res.Reason,
		"rtt_ms", res.RTTMillis, "nat_class", res.NATClass)
	w.WriteHeader(http.StatusNoContent)
}

func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func requestIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// punchLimiter is a per-key token-bucket rate limiter for hub HTTP requests. Keys
// can be partly attacker-controlled (e.g. ip|relay_id), so allow() prunes idle
// buckets to keep the map bounded — otherwise a caller spraying fresh keys grows
// it without limit (an unauthenticated memory-exhaustion DoS).
type punchLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*ratebucket
	rate      float64
	burst     float64
	lastPrune time.Time
}

type ratebucket struct {
	tokens float64
	last   time.Time
}

const (
	limiterPruneInterval = time.Minute
	limiterIdleTTL       = 10 * time.Minute
)

func newPunchLimiter(rate, burst float64) *punchLimiter {
	return &punchLimiter{buckets: make(map[string]*ratebucket), rate: rate, burst: burst}
}

func (l *punchLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	b, ok := l.buckets[key]
	if !ok {
		l.buckets[key] = &ratebucket{tokens: l.burst - 1, last: now}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// pruneLocked drops buckets idle beyond the TTL, at most once per interval. A
// pruned key simply refills from full if it reappears, which is harmless.
func (l *punchLimiter) pruneLocked(now time.Time) {
	if now.Sub(l.lastPrune) < limiterPruneInterval {
		return
	}
	l.lastPrune = now
	cutoff := now.Add(-limiterIdleTTL)
	for key, b := range l.buckets {
		if b.last.Before(cutoff) {
			delete(l.buckets, key)
		}
	}
}

// inflightCap bounds concurrent in-flight punch requests per relay and per client
// IP, so the ≤3s-blocking handler cannot be used to exhaust hub goroutines.
type inflightCap struct {
	mu       sync.Mutex
	perRelay map[string]int
	perIP    map[string]int
	maxRelay int
	maxIP    int
}

func newInflightCap(maxRelay, maxIP int) *inflightCap {
	return &inflightCap{
		perRelay: make(map[string]int),
		perIP:    make(map[string]int),
		maxRelay: maxRelay,
		maxIP:    maxIP,
	}
}

func (f *inflightCap) acquire(relay, ip string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.perRelay[relay] >= f.maxRelay || f.perIP[ip] >= f.maxIP {
		return false
	}
	f.perRelay[relay]++
	f.perIP[ip]++
	return true
}

func (f *inflightCap) release(relay, ip string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.perRelay[relay]--; f.perRelay[relay] <= 0 {
		delete(f.perRelay, relay)
	}
	if f.perIP[ip]--; f.perIP[ip] <= 0 {
		delete(f.perIP, ip)
	}
}
