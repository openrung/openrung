package wssbridge

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

const (
	DefaultFixedTarget          = "127.0.0.1:443"
	DefaultMaxSessions          = 2048
	DefaultMaxPendingHandshakes = 4096
	DefaultMaxStreamsPerSession = 128
	DefaultMaxGlobalStreams     = 16_384
	DefaultDialTimeout          = 5 * time.Second
	DefaultFirstByteTimeout     = 10 * time.Second
	DefaultStreamIdleTimeout    = 5 * time.Minute
	DefaultSessionLifetime      = 6 * time.Hour
	DefaultSidecarPingInterval  = 4 * time.Minute
	DefaultSidecarPingTimeout   = 10 * time.Second

	maxSidecarSessions      = 100_000
	maxSidecarGlobalStreams = 100_000
	maxSessionLifetime      = 24 * time.Hour
	maxConfiguredFronts     = 32
	maxTokensPerFront       = 8
	firstByteBufferSize     = 4096
)

type DialContextFunc func(context.Context, string, string) (net.Conn, error)

type SidecarOptions struct {
	RelayID             string
	FrontOriginTokens   map[string][]string
	ViewerAddressHeader string
	Verifier            *TicketVerifier
	ReplayStore         ReplayStore
	Stats               *SidecarStats
	FixedTarget         string

	MaxSessions                  int
	MaxPendingHandshakes         int
	MaxStreamsPerSession         int
	MaxGlobalStreams             int
	MaxSessionsPerSource         int
	MaxStreamsPerSource          int
	GlobalHandshakeRatePerSecond float64
	GlobalHandshakeBurst         int
	HandshakeRatePerSecond       float64
	HandshakeBurst               int
	MaxTrackedSources            int
	HandshakeTimeout             time.Duration
	DialTimeout                  time.Duration
	FirstByteTimeout             time.Duration
	StreamIdleTimeout            time.Duration
	SessionLifetime              time.Duration
	NoStreamIdleTimeout          time.Duration
	PingInterval                 time.Duration
	PingWriteTimeout             time.Duration
	ReadLimit                    int64

	// DialContext is a test seam. The handler always passes network "tcp" and
	// its one validated FixedTarget; no request or ticket can influence it.
	DialContext DialContextFunc
}

type frontOriginAuth struct {
	frontID string
	hashes  [][sha256.Size]byte
}

type sidecarHandler struct {
	relayID                string
	fronts                 []frontOriginAuth
	viewerAddressHeader    string
	verifier               *TicketVerifier
	replay                 ReplayStore
	stats                  *SidecarStats
	fixedTarget            string
	globalHandshakeLimiter *sourceRateLimiter
	handshakeLimiter       *sourceRateLimiter
	sourceUsage            *sourceUsage

	sessions             chan struct{}
	pendingHandshakes    chan struct{}
	globalStreams        chan struct{}
	maxStreamsPerSession int
	handshakeTimeout     time.Duration
	dialTimeout          time.Duration
	firstByteTimeout     time.Duration
	streamIdleTimeout    time.Duration
	sessionLifetime      time.Duration
	noStreamIdleTimeout  time.Duration
	pingInterval         time.Duration
	pingWriteTimeout     time.Duration
	readLimit            int64
	dialContext          DialContextFunc
	upgrader             websocket.Upgrader
}

// NewSidecarHandler builds the relay-local origin handler. It has no routing
// table: every accepted stream is dialed to one validated loopback address.
func NewSidecarHandler(opts SidecarOptions) (http.Handler, error) {
	if opts.Verifier == nil {
		return nil, errors.New("WSS sidecar ticket verifier is required")
	}
	if opts.RelayID == "" || opts.RelayID != opts.Verifier.LocalRelayID() {
		return nil, errors.New("WSS sidecar relay ID must match the ticket verifier")
	}
	fronts, err := validateFrontOriginTokens(opts.FrontOriginTokens)
	if err != nil {
		return nil, err
	}
	fixedTarget, err := NormalizeLoopbackTarget(opts.FixedTarget)
	if err != nil {
		return nil, err
	}
	if opts.ViewerAddressHeader == "" {
		opts.ViewerAddressHeader = DefaultViewerAddressHeader
	}
	if !validHTTPHeaderName(opts.ViewerAddressHeader) || strings.EqualFold(opts.ViewerAddressHeader, OriginTokenHeader) {
		return nil, errors.New("WSS sidecar viewer-address header is invalid")
	}
	if opts.ReplayStore == nil {
		return nil, errors.New("WSS sidecar replay store is required")
	}
	if opts.Stats == nil {
		opts.Stats = &SidecarStats{}
	}
	if opts.MaxSessions, err = boundedDefault(opts.MaxSessions, DefaultMaxSessions, maxSidecarSessions, "max sessions"); err != nil {
		return nil, err
	}
	if opts.MaxPendingHandshakes, err = boundedDefault(opts.MaxPendingHandshakes, DefaultMaxPendingHandshakes, maxSidecarSessions, "max pending handshakes"); err != nil {
		return nil, err
	}
	if opts.MaxStreamsPerSession, err = boundedDefault(opts.MaxStreamsPerSession, DefaultMaxStreamsPerSession, MaxTicketStreams, "max streams per session"); err != nil {
		return nil, err
	}
	if opts.MaxGlobalStreams, err = boundedDefault(opts.MaxGlobalStreams, DefaultMaxGlobalStreams, maxSidecarGlobalStreams, "max global streams"); err != nil {
		return nil, err
	}
	if opts.MaxSessionsPerSource, err = boundedDefault(opts.MaxSessionsPerSource, DefaultMaxSessionsPerSource, maxSidecarSessions, "max sessions per source"); err != nil {
		return nil, err
	}
	if opts.MaxStreamsPerSource, err = boundedDefault(opts.MaxStreamsPerSource, DefaultMaxStreamsPerSource, maxSidecarGlobalStreams, "max streams per source"); err != nil {
		return nil, err
	}
	if opts.GlobalHandshakeRatePerSecond == 0 {
		opts.GlobalHandshakeRatePerSecond = DefaultGlobalHandshakeRatePerSecond
	}
	if opts.GlobalHandshakeRatePerSecond < 0 || math.IsNaN(opts.GlobalHandshakeRatePerSecond) || math.IsInf(opts.GlobalHandshakeRatePerSecond, 0) || opts.GlobalHandshakeRatePerSecond > 1_000_000 {
		return nil, errors.New("global handshake rate must be within (0, 1000000]")
	}
	if opts.GlobalHandshakeBurst, err = boundedDefault(opts.GlobalHandshakeBurst, DefaultGlobalHandshakeBurst, 1_000_000, "global handshake burst"); err != nil {
		return nil, err
	}
	if opts.HandshakeRatePerSecond == 0 {
		opts.HandshakeRatePerSecond = DefaultHandshakeRatePerSecond
	}
	if opts.HandshakeRatePerSecond < 0 || math.IsNaN(opts.HandshakeRatePerSecond) || math.IsInf(opts.HandshakeRatePerSecond, 0) || opts.HandshakeRatePerSecond > 1_000_000 {
		return nil, errors.New("handshake rate must be within (0, 1000000]")
	}
	if opts.HandshakeBurst, err = boundedDefault(opts.HandshakeBurst, DefaultHandshakeBurst, 1_000_000, "handshake burst"); err != nil {
		return nil, err
	}
	if opts.MaxTrackedSources, err = boundedDefault(opts.MaxTrackedSources, DefaultMaxTrackedSources, maxTrackedSources, "max tracked sources"); err != nil {
		return nil, err
	}
	if opts.HandshakeTimeout, err = durationDefault(opts.HandshakeTimeout, defaultHandshakeTimeout, time.Minute, "handshake timeout"); err != nil {
		return nil, err
	}
	if opts.DialTimeout, err = durationDefault(opts.DialTimeout, DefaultDialTimeout, time.Minute, "dial timeout"); err != nil {
		return nil, err
	}
	if opts.FirstByteTimeout, err = durationDefault(opts.FirstByteTimeout, DefaultFirstByteTimeout, time.Minute, "first-byte timeout"); err != nil {
		return nil, err
	}
	if opts.StreamIdleTimeout, err = durationDefault(opts.StreamIdleTimeout, DefaultStreamIdleTimeout, maxSessionLifetime, "stream idle timeout"); err != nil {
		return nil, err
	}
	if opts.SessionLifetime, err = durationDefault(opts.SessionLifetime, DefaultSessionLifetime, maxSessionLifetime, "session lifetime"); err != nil {
		return nil, err
	}
	if opts.NoStreamIdleTimeout, err = durationDefault(opts.NoStreamIdleTimeout, DefaultNoStreamIdleTimeout, maxSessionLifetime, "no-stream idle timeout"); err != nil {
		return nil, err
	}
	if opts.PingInterval == 0 {
		opts.PingInterval = DefaultSidecarPingInterval
	}
	if opts.PingInterval < 0 {
		opts.PingInterval = 0
	}
	if opts.PingInterval > 10*time.Minute {
		return nil, errors.New("ping interval must not exceed 10 minutes")
	}
	if opts.PingWriteTimeout <= 0 {
		opts.PingWriteTimeout = DefaultSidecarPingTimeout
	}
	if opts.PingWriteTimeout > time.Minute || (opts.PingInterval > 0 && opts.PingWriteTimeout >= opts.PingInterval) {
		return nil, errors.New("ping write timeout must be at most one minute and shorter than the ping interval")
	}
	if opts.ReadLimit <= 0 {
		opts.ReadLimit = defaultWebSocketReadMax
	}
	if opts.ReadLimit > 16<<20 {
		return nil, errors.New("WebSocket message read limit must not exceed 16 MiB")
	}
	if opts.DialContext == nil {
		opts.DialContext = (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext
	}

	h := &sidecarHandler{
		relayID: opts.RelayID, fronts: fronts,
		viewerAddressHeader: http.CanonicalHeaderKey(opts.ViewerAddressHeader),
		verifier:            opts.Verifier, replay: opts.ReplayStore, stats: opts.Stats,
		fixedTarget:            fixedTarget,
		globalHandshakeLimiter: newSourceRateLimiter(opts.GlobalHandshakeRatePerSecond, opts.GlobalHandshakeBurst, 1),
		handshakeLimiter:       newSourceRateLimiter(opts.HandshakeRatePerSecond, opts.HandshakeBurst, opts.MaxTrackedSources),
		sourceUsage:            newSourceUsage(opts.MaxSessionsPerSource, opts.MaxStreamsPerSource),
		sessions:               make(chan struct{}, opts.MaxSessions),
		pendingHandshakes:      make(chan struct{}, opts.MaxPendingHandshakes),
		globalStreams:          make(chan struct{}, opts.MaxGlobalStreams),
		maxStreamsPerSession:   opts.MaxStreamsPerSession,
		handshakeTimeout:       opts.HandshakeTimeout, dialTimeout: opts.DialTimeout,
		firstByteTimeout: opts.FirstByteTimeout, streamIdleTimeout: opts.StreamIdleTimeout,
		sessionLifetime: opts.SessionLifetime, noStreamIdleTimeout: opts.NoStreamIdleTimeout,
		pingInterval: opts.PingInterval, pingWriteTimeout: opts.PingWriteTimeout,
		readLimit: opts.ReadLimit, dialContext: opts.DialContext,
	}
	h.upgrader = websocket.Upgrader{
		HandshakeTimeout:  h.handshakeTimeout,
		EnableCompression: false,
		Subprotocols:      []string{Subprotocol},
		CheckOrigin:       func(*http.Request) bool { return true },
	}
	return h, nil
}

func (h *sidecarHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case HealthPath:
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	case BridgePath:
		h.handleBridge(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *sidecarHandler) handleBridge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		h.stats.protocolRejections.Add(1)
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Origin authentication is intentionally first. Only after it succeeds may
	// the otherwise-spoofable viewer-address header influence source controls.
	frontID, ok := h.authenticateOrigin(r.Header.Values(OriginTokenHeader))
	if !ok {
		h.stats.originAuthRejections.Add(1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if allowed, _ := h.globalHandshakeLimiter.allow("global"); !allowed {
		h.stats.globalHandshakeRateRejections.Add(1)
		tooManyRequests(w)
		return
	}
	sourceIP, ok := parseViewerAddress(r.Header.Values(h.viewerAddressHeader))
	if !ok {
		h.stats.viewerAddressRejections.Add(1)
		http.Error(w, "trusted viewer address required", http.StatusBadRequest)
		return
	}
	allowed, failOpen := h.handshakeLimiter.allow(sourceIP)
	if failOpen {
		h.stats.handshakeLimiterFailOpen.Add(1)
	}
	if !allowed {
		h.stats.handshakeRateRejections.Add(1)
		tooManyRequests(w)
		return
	}
	if !websocket.IsWebSocketUpgrade(r) || !requestsSubprotocol(r, Subprotocol) {
		h.stats.protocolRejections.Add(1)
		http.Error(w, "WebSocket upgrade with required subprotocol expected", http.StatusBadRequest)
		return
	}
	ticket, ok := bearerTicket(r.Header.Values("Authorization"))
	if !ok {
		h.stats.ticketRejections.Add(1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	select {
	case h.pendingHandshakes <- struct{}{}:
	case <-r.Context().Done():
		return
	default:
		h.stats.handshakeConcurrencyRejections.Add(1)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "sidecar busy", http.StatusServiceUnavailable)
		return
	}
	handshakeSlotHeld := true
	defer func() {
		if handshakeSlotHeld {
			<-h.pendingHandshakes
		}
	}()
	claims, err := h.verifier.Verify(ticket)
	if err != nil {
		h.stats.ticketRejections.Add(1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if claims.FrontID != frontID {
		h.stats.frontRejections.Add(1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	replayUntil := claims.Expiry().Add(h.verifier.opts.ClockSkew)
	consumed, err := h.replay.Consume(r.Context(), claims.JTI, replayUntil)
	if err != nil {
		h.stats.replayStoreFailures.Add(1)
		http.Error(w, "authorization unavailable", http.StatusServiceUnavailable)
		return
	}
	if !consumed {
		h.stats.replayRejections.Add(1)
		http.Error(w, "ticket already used", http.StatusConflict)
		return
	}
	if !h.sourceUsage.acquireSession(sourceIP) {
		h.stats.sourceSessionRejections.Add(1)
		tooManyRequests(w)
		return
	}
	defer h.sourceUsage.releaseSession(sourceIP)
	select {
	case h.sessions <- struct{}{}:
		defer func() { <-h.sessions }()
	default:
		h.stats.sessionLimitRejections.Add(1)
		w.Header().Set("Retry-After", "5")
		http.Error(w, "sidecar busy", http.StatusServiceUnavailable)
		return
	}

	ws, err := h.upgrader.Upgrade(w, r, http.Header{"Cache-Control": []string{"no-store"}})
	<-h.pendingHandshakes
	handshakeSlotHeld = false
	if err != nil {
		h.stats.upgradeFailures.Add(1)
		return
	}
	if ws.Subprotocol() != Subprotocol {
		h.stats.upgradeFailures.Add(1)
		_ = ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseProtocolError, "subprotocol required"),
			time.Now().Add(time.Second))
		_ = ws.Close()
		return
	}
	h.stats.acceptedSessions.Add(1)
	h.stats.currentSessions.Add(1)
	defer h.stats.currentSessions.Add(-1)
	h.serveSession(r.Context(), ws, claims, sourceIP)
}

func (h *sidecarHandler) authenticateOrigin(values []string) (string, bool) {
	provided := ""
	if len(values) == 1 {
		provided = values[0]
	}
	presentedHash := sha256.Sum256([]byte(provided))
	matchedFront, matches := "", 0
	for _, front := range h.fronts {
		frontMatch := 0
		for i := range front.hashes {
			frontMatch |= subtle.ConstantTimeCompare(presentedHash[:], front.hashes[i][:])
		}
		if frontMatch == 1 {
			matchedFront = front.frontID
			matches++
		}
	}
	return matchedFront, len(values) == 1 && matches == 1
}

func (h *sidecarHandler) serveSession(parent context.Context, ws *websocket.Conn, claims Claims, sourceIP string) {
	ctx, cancel := context.WithTimeout(parent, h.sessionLifetime)
	defer cancel()
	streamConn := newWebsocketStreamConn(ws, h.readLimit)
	streamConn.startPings(ctx, h.pingInterval, h.pingWriteTimeout)
	session, err := yamux.Server(streamConn, bridgeYamuxConfig())
	if err != nil {
		_ = streamConn.Close()
		return
	}
	defer session.Close()
	defer streamConn.Close()
	idle := newSessionIdleGuard(h.noStreamIdleTimeout, func() {
		h.stats.idleSessionCloses.Add(1)
		cancel()
		_ = session.Close()
		_ = streamConn.Close()
	})
	defer idle.close()
	go func() {
		select {
		case <-ctx.Done():
			_ = session.Close()
			_ = streamConn.Close()
		case <-session.CloseChan():
			cancel()
		}
	}()

	perSessionMax := min(claims.MaxStreams, h.maxStreamsPerSession)
	perSession := make(chan struct{}, perSessionMax)
	remainingStreams := perSessionMax
	var wg sync.WaitGroup
	for {
		stream, err := session.AcceptStreamWithContext(ctx)
		if err != nil {
			break
		}
		// max_streams is a ticket-lifetime authorization budget, not merely a
		// concurrency hint. Never replenish it when a stream closes: one
		// consumed ticket can cause at most this many loopback Reality dials.
		if remainingStreams == 0 {
			h.stats.streamLimitRejections.Add(1)
			_ = stream.Close()
			break
		}
		remainingStreams--
		select {
		case perSession <- struct{}{}:
		default:
			h.stats.streamLimitRejections.Add(1)
			_ = stream.Close()
			continue
		}
		select {
		case h.globalStreams <- struct{}{}:
		default:
			h.stats.streamLimitRejections.Add(1)
			<-perSession
			_ = stream.Close()
			continue
		}
		if !h.sourceUsage.acquireStream(sourceIP) {
			h.stats.sourceStreamRejections.Add(1)
			<-h.globalStreams
			<-perSession
			_ = stream.Close()
			continue
		}
		if !idle.startStream() {
			h.sourceUsage.releaseStream(sourceIP)
			<-h.globalStreams
			<-perSession
			_ = stream.Close()
			continue
		}
		h.stats.acceptedStreams.Add(1)
		h.stats.currentStreams.Add(1)
		wg.Add(1)
		go func(stream net.Conn) {
			defer wg.Done()
			defer idle.endStream()
			defer h.stats.currentStreams.Add(-1)
			defer h.sourceUsage.releaseStream(sourceIP)
			defer func() { <-perSession }()
			defer func() { <-h.globalStreams }()
			h.handleStream(ctx, stream)
		}(stream)
	}
	cancel()
	_ = session.Close()
	wg.Wait()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		h.stats.lifetimeSessionCloses.Add(1)
	}
}

func (h *sidecarHandler) handleStream(ctx context.Context, stream net.Conn) {
	defer stream.Close()
	_ = stream.SetReadDeadline(time.Now().Add(h.firstByteTimeout))
	first := make([]byte, firstByteBufferSize)
	n, err := stream.Read(first)
	if err != nil && n == 0 {
		return
	}
	_ = stream.SetReadDeadline(time.Time{})
	dialCtx, cancel := context.WithTimeout(ctx, h.dialTimeout)
	target, dialErr := h.dialContext(dialCtx, "tcp", h.fixedTarget)
	cancel()
	if dialErr != nil {
		h.stats.dialFailures.Add(1)
		return
	}
	defer target.Close()
	if _, err := writeAll(target, first[:n]); err != nil {
		return
	}
	copyOpaque(ctx, stream, target, h.streamIdleTimeout)
}

// NormalizeLoopbackTarget validates and canonicalizes the only address the
// sidecar is permitted to dial. Hostnames and non-loopback literals are denied.
func NormalizeLoopbackTarget(value string) (string, error) {
	if value == "" {
		value = DefaultFixedTarget
	}
	if strings.TrimSpace(value) != value {
		return "", errors.New("WSS sidecar fixed target must not contain whitespace")
	}
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return "", errors.New("WSS sidecar fixed target must be a loopback IP literal and port")
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || !ip.IsLoopback() {
		return "", errors.New("WSS sidecar fixed target must use a loopback IP literal")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", errors.New("WSS sidecar fixed target port must be within 1..65535")
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
}

func validateFrontOriginTokens(configured map[string][]string) ([]frontOriginAuth, error) {
	if len(configured) == 0 || len(configured) > maxConfiguredFronts {
		return nil, fmt.Errorf("WSS sidecar requires 1..%d front origin-token sets", maxConfiguredFronts)
	}
	fronts := make([]frontOriginAuth, 0, len(configured))
	owners := make(map[[sha256.Size]byte]string)
	for frontID, tokens := range configured {
		if !validFrontID(frontID) {
			return nil, fmt.Errorf("WSS sidecar front ID %q is invalid", frontID)
		}
		if len(tokens) == 0 || len(tokens) > maxTokensPerFront {
			return nil, fmt.Errorf("WSS sidecar front %q requires 1..%d origin tokens", frontID, maxTokensPerFront)
		}
		front := frontOriginAuth{frontID: frontID}
		seen := make(map[[sha256.Size]byte]struct{}, len(tokens))
		for _, token := range tokens {
			if !validOriginToken(token) {
				return nil, errors.New("WSS sidecar origin tokens must be 32..512 non-whitespace bytes without line breaks")
			}
			hash := sha256.Sum256([]byte(token))
			if _, duplicate := seen[hash]; duplicate {
				continue
			}
			if owner, exists := owners[hash]; exists && owner != frontID {
				return nil, errors.New("WSS sidecar origin token cannot authenticate more than one front")
			}
			seen[hash] = struct{}{}
			owners[hash] = frontID
			front.hashes = append(front.hashes, hash)
		}
		fronts = append(fronts, front)
	}
	return fronts, nil
}

func validOriginToken(token string) bool {
	return len(token) >= 32 && len(token) <= 512 && strings.TrimSpace(token) == token && !strings.ContainsAny(token, "\x00\r\n")
}

func requestsSubprotocol(r *http.Request, wanted string) bool {
	for _, protocol := range websocket.Subprotocols(r) {
		if protocol == wanted {
			return true
		}
	}
	return false
}

func bearerTicket(values []string) (string, bool) {
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return "", false
	}
	ticket := strings.TrimPrefix(values[0], "Bearer ")
	if ticket == "" || strings.TrimSpace(ticket) != ticket || len(ticket) > MaxTicketBytes {
		return "", false
	}
	return ticket, true
}

func validHTTPHeaderName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func tooManyRequests(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "1")
	http.Error(w, "rate limited", http.StatusTooManyRequests)
}

func boundedDefault(value, fallback, hardMax int, name string) (int, error) {
	if value == 0 {
		value = fallback
	}
	if value < 1 || value > hardMax {
		return 0, fmt.Errorf("%s must be within [1, %d]", name, hardMax)
	}
	return value, nil
}

func durationDefault(value, fallback, hardMax time.Duration, name string) (time.Duration, error) {
	if value == 0 {
		value = fallback
	}
	if value < 0 || value > hardMax {
		return 0, fmt.Errorf("%s must be within (0, %s]", name, hardMax)
	}
	return value, nil
}
