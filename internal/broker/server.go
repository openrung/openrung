package broker

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"openrung/internal/relay"
)

// maxRegisterBodyBytes caps relay registration payloads; descriptors are
// small structs, so anything near the cap is malformed or hostile.
const maxRegisterBodyBytes = 64 << 10

type Config struct {
	RegistrationToken string
	// FoundationToken is the privileged bearer token that authorizes
	// registrations claiming node_class "foundation". It must differ from
	// RegistrationToken (cmd/broker refuses to start otherwise — a shared
	// value would let every volunteer-token holder self-promote) and its holder
	// can still register volunteer-class relays: the token bounds the maximum
	// class a request may claim, it does not force one. Routine volunteer-class
	// relay and relay-hub traffic should still use RegistrationToken so this
	// credential stays out of the hub path.
	// Empty disables foundation registration entirely.
	FoundationToken string
	RelayLeaseTTL   time.Duration
	TelemetrySink   TelemetrySink
	// TelemetryReader backs the dashboard by aggregating records in Go on
	// every request (the JSONL sink's path). TelemetryQuerier backs it with
	// pre-aggregated queries (the Postgres store) and wins when both are set.
	TelemetryReader  TelemetryReader
	TelemetryQuerier TelemetryQuerier
	DashboardToken   string
	// TrustedProxyCIDRs are additional CIDRs (beyond Cloudflare's published ranges) whose forwarded
	// CF-Connecting-IP / X-Forwarded-For headers the broker will trust for the real client IP.
	TrustedProxyCIDRs []string
	// GeoIP resolves the city/country of a relay's public endpoint so clients
	// can show where relays are located. Nil disables lookups;
	// descriptors then carry empty geo fields.
	GeoIP GeoIPResolver
	// SigningSeed is the 32-byte Ed25519 seed that signs every relay-list
	// response body. Required: cmd/broker validates OPENRUNG_RELAY_SIGNING_KEY
	// with ParseSigningSeed and refuses to start without it, because serving
	// unsigned lists is an invisible outage for verifying clients.
	SigningSeed []byte
}

func NewServer(store RelayStore, cfg Config) http.Handler {
	if cfg.RelayLeaseTTL == 0 {
		cfg.RelayLeaseTTL = 3 * time.Minute
	}
	relaySigner := newSigner(cfg.SigningSeed)
	clientIP := newClientIPResolver(cfg.TrustedProxyCIDRs)
	clientSeen := newClientSeenDeduper(clientSeenDedupWindow, clientSeenDedupMaxEntries)
	relayListLimiter := newIPRateLimiter(relayListRatePerSecond, relayListBurst, rateLimiterMaxTrackedIPs)
	telemetryLimiter := newIPRateLimiter(telemetryRatePerSecond, telemetryBurst, rateLimiterMaxTrackedIPs)
	speedTestLimiter := newIPRateLimiter(speedTestRatePerSecond, speedTestBurst, rateLimiterMaxTrackedIPs)
	relayRegistrationLimiter := newIPRateLimiter(relayRegistrationRatePerSecond, relayRegistrationBurst, rateLimiterMaxTrackedIPs)
	registerRelay := rateLimited(relayRegistrationLimiter, clientIP, 10, registerHandler(store, cfg))
	heartbeatRelay := rateLimited(relayRegistrationLimiter, clientIP, 10, heartbeatHandler(store, cfg))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := store.Ping(r.Context()); err != nil {
			slog.Error("broker health check failed", "error", err)
			writeError(w, http.StatusServiceUnavailable, "broker storage unavailable")
			return
		}
		// signing_key_id is public data (it ships in every relay-list body) and
		// lets the monitor assert the active key without parsing a relay list.
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "signing_key_id": relaySigner.keyID})
	})
	mux.HandleFunc("POST /api/v1/relays/register", registerRelay)
	mux.HandleFunc("POST /api/v1/relays/", heartbeatRelay)
	mux.HandleFunc("GET /api/v1/relays", rateLimited(relayListLimiter, clientIP, 10, listRelaysHandler(store, cfg.TelemetrySink, clientIP, clientSeen, relaySigner)))
	mux.HandleFunc("GET /api/v1/relays.mirror", rateLimited(relayListLimiter, clientIP, 10, listRelaysMirrorHandler(store, relaySigner)))
	mux.HandleFunc("POST /api/v1/telemetry/events", rateLimited(telemetryLimiter, clientIP, 10, telemetryHandler(cfg.TelemetrySink, store, clientIP)))
	mux.HandleFunc("GET /api/v1/speed-test", rateLimited(speedTestLimiter, clientIP, 30, speedTestHandler(speedTestMaxConcurrent)))
	querier := cfg.TelemetryQuerier
	if querier == nil && cfg.TelemetryReader != nil {
		querier = newTelemetryReaderQuerier(cfg.TelemetryReader)
	}
	if cfg.DashboardToken != "" && querier != nil {
		dashboard := newDashboardServer(cfg.DashboardToken, querier)
		dashboard.relayDisplays = relayDisplayResolver(store)
		dashboard.register(mux)
	}

	return mux
}

func registerHandler(store RelayStore, cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		maxClass, ok := credentialNodeClass(r, cfg)
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing or invalid relay registration token")
			return
		}

		var req relay.RegisterRequest
		r.Body = http.MaxBytesReader(w, r.Body, maxRegisterBodyBytes)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON request")
			return
		}

		label, err := relay.NormalizeLabel(req.Label)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Label = label

		nodeClass, err := relay.NormalizeNodeClass(req.NodeClass)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.NodeClass = nodeClass
		// Fail loudly instead of clamping: a foundation relay that lost its
		// token should crash-loop where the operator sees it, not silently
		// serve as a volunteer-class relay.
		if req.NodeClass == relay.NodeClassFoundation && maxClass != relay.NodeClassFoundation {
			writeError(w, http.StatusForbidden, "node_class foundation requires the foundation registration token")
			return
		}

		if err := validateRegisterRequest(req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		desc, err := store.Register(req, time.Now().UTC(), cfg.RelayLeaseTTL)
		// A malformed or stale identity proof fails the registration loudly
		// instead of silently falling back to a random relay ID — a relay that
		// believes it has a stable identity should crash-loop where its
		// operator can see it, mirroring the foundation-token posture above.
		// The exact expired message is a contract: the relay hub matches it to
		// recycle a tunnel session whose stored proof has aged out.
		if errors.Is(err, relay.ErrIdentityProofExpired) || errors.Is(err, relay.ErrIdentityProofInvalid) || errors.Is(err, relay.ErrIdentityIncomplete) {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		if errors.Is(err, ErrNodeClassForbidden) {
			// The endpoint is held by a live foundation relay; a
			// non-foundation registration may not seize it.
			writeError(w, http.StatusForbidden, "public_host:public_port is reserved by a foundation relay")
			return
		}
		if err != nil {
			slog.Error("could not register relay", "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not register relay")
			return
		}
		resolveRelayGeo(r.Context(), store, cfg.GeoIP, &desc)
		slog.Info("relay registered", "relay_id", desc.ID, "node_class", desc.NodeClass, "public", desc.PublicHost, "port", desc.PublicPort, "city", desc.City, "country", desc.Country, "max_sessions", desc.MaxSessions, "version", desc.RelayVersion)

		writeJSON(w, http.StatusCreated, desc)
	}
}

func heartbeatHandler(store RelayStore, cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// A registration credential may heartbeat a relay at or below its
		// authorized node class, so foundation relays can present the foundation
		// token on both calls.
		maxClass, ok := credentialNodeClass(r, cfg)
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing or invalid relay registration token")
			return
		}

		id, ok := heartbeatRelayID(r.URL.Path)
		if !ok {
			writeError(w, http.StatusNotFound, "unknown relay endpoint")
			return
		}

		desc, err := store.Heartbeat(id, maxClass, time.Now().UTC(), cfg.RelayLeaseTTL)
		if errors.Is(err, ErrRelayNotFound) {
			writeError(w, http.StatusNotFound, "relay not found")
			return
		}
		if errors.Is(err, ErrNodeClassForbidden) {
			writeError(w, http.StatusForbidden, "heartbeat for a foundation relay requires the foundation registration token")
			return
		}
		if err != nil {
			slog.Error("could not update relay heartbeat", "relay_id", id, "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not update relay heartbeat")
			return
		}

		// Backfill relays whose registration-time lookup failed (or that
		// registered before the broker resolved locations at all).
		resolveRelayGeo(r.Context(), store, cfg.GeoIP, &desc)

		writeJSON(w, http.StatusOK, relay.HeartbeatResponse{OK: true, ExpiresAt: desc.ExpiresAt})
	}
}

// resolveRelayGeo best-effort resolves and persists the location of a relay's
// public endpoint. It only performs a lookup when the descriptor has no geo
// yet, and never fails the surrounding request: on lookup or store errors the
// descriptor simply keeps its empty geo until a later attempt succeeds. The
// resolver caches failures, so heartbeat-driven retries stay rate-limited.
func resolveRelayGeo(ctx context.Context, store RelayStore, resolver GeoIPResolver, desc *relay.Descriptor) {
	if resolver == nil || desc.GeoLocation != (relay.GeoLocation{}) {
		return
	}
	host := geoLookupHost(desc)
	geo, err := resolver.Lookup(ctx, host)
	if err != nil {
		slog.Warn("could not resolve relay location", "relay_id", desc.ID, "host", host, "error", err)
		return
	}
	if err := store.UpdateGeo(desc.ID, geo); err != nil {
		slog.Warn("could not store relay location", "relay_id", desc.ID, "error", err)
		return
	}
	desc.GeoLocation = geo
	slog.Info("relay location resolved", "relay_id", desc.ID, "city", geo.City, "country", geo.Country)
}

// geoLookupHost picks the address whose location clients care about: the
// relay's observed exit IP when the hub reported one (tunnel transport), otherwise
// the advertised public endpoint (direct transport, where they coincide).
func geoLookupHost(desc *relay.Descriptor) string {
	if desc.ExitHost != "" {
		return desc.ExitHost
	}
	return desc.PublicHost
}

func listRelaysHandler(store RelayStore, telemetrySink TelemetrySink, clientIP *clientIPResolver, clientSeen *clientSeenDeduper, s signer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Relay availability is real-time: shared caches (Cloudflare's edge in
		// production) must never store a copy, and clients cannot bust an edge
		// cache from their side. Set on every path so errors aren't cached
		// either; the signed success path upgrades it to no-store, no-transform.
		w.Header().Set("Cache-Control", "no-store")
		recordClientSeen(r, telemetrySink, clientIP, clientSeen)
		limit := 5
		if raw := r.URL.Query().Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 20 {
				writeError(w, http.StatusBadRequest, "limit must be between 1 and 20")
				return
			}
			limit = parsed
		}

		now := time.Now().UTC()
		relays, err := store.List(now, limit)
		if err != nil {
			slog.Error("could not list relays", "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not list relays")
			return
		}
		s.writeSigned(w, relay.ListResponse{
			Count:      len(relays),
			ServerTime: now,
			NotAfter:   now.Add(apiNotAfterWindow),
			KeyID:      s.keyID,
			Channel:    relay.ChannelAPI,
			// Echo the effective limit so clients can reject a signed body
			// replayed from a differently-shaped request.
			Limit:  limit,
			Relays: relays,
		})
	}
}

// mirrorRelayLimit is the mirror channel's page size: the API's maximum page
// (the desktop directory's full-list fetch), so a mirror artifact carries
// every relay a client could see through the API.
const mirrorRelayLimit = 20

// listRelaysMirrorHandler serves the mirror-channel relay list: the full
// directory page with a 24 h validity window, signed exactly like the API
// channel. An hourly cron on the broker host fetches it and publishes the
// exact body bytes plus the signature header value to static mirrors, which
// clients try only after every API candidate fails. The body carries no limit
// field — the mirror is not request-shaped, so there is nothing to echo.
func listRelaysMirrorHandler(store RelayStore, s signer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Same caching rule as the API list: errors must not be cached either.
		w.Header().Set("Cache-Control", "no-store")
		now := time.Now().UTC()
		relays, err := store.List(now, mirrorRelayLimit)
		if err != nil {
			slog.Error("could not list relays for mirror", "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not list relays")
			return
		}
		s.writeSigned(w, relay.ListResponse{
			Count:      len(relays),
			ServerTime: now,
			NotAfter:   now.Add(mirrorNotAfterWindow),
			KeyID:      s.keyID,
			Channel:    relay.ChannelMirror,
			Relays:     relays,
		})
	}
}

func validateRegisterRequest(req relay.RegisterRequest) error {
	switch {
	case req.PublicHost == "":
		return errors.New("public_host is required")
	case req.PublicPort < 1 || req.PublicPort > 65535:
		return errors.New("public_port must be between 1 and 65535")
	case req.Protocol != relay.ProtocolVLESSRealityVision:
		return errors.New("protocol must be vless-reality-vision")
	case req.ClientID == "":
		return errors.New("client_id is required")
	case req.RealityPublicKey == "":
		return errors.New("reality_public_key is required")
	case req.ShortID == "":
		return errors.New("short_id is required")
	case req.ServerName == "":
		return errors.New("server_name is required")
	case req.Flow != relay.FlowVision:
		return errors.New("flow must be xtls-rprx-vision")
	case req.ExitMode != relay.ExitModeDirect && req.ExitMode != relay.ExitModeDedicated:
		return errors.New("exit_mode must be direct or dedicated")
	case req.Transport != "" && req.Transport != relay.TransportDirect && req.Transport != relay.TransportTunnel:
		return errors.New("transport must be direct or tunnel")
	case req.ExitHost != "" && req.Transport != relay.TransportTunnel:
		return errors.New("exit_host is only allowed for tunnel transport")
	case req.MaxSessions < 1:
		return errors.New("max_sessions must be at least 1")
	case req.MaxMbps < 1:
		return errors.New("max_mbps must be at least 1")
	default:
		return nil
	}
}

func heartbeatRelayID(path string) (string, bool) {
	const (
		relayPrefix = "/api/v1/relays/"
		suffix      = "/heartbeat"
	)

	if !strings.HasPrefix(path, relayPrefix) {
		return "", false
	}
	remainder := strings.TrimPrefix(path, relayPrefix)

	id, ok := strings.CutSuffix(remainder, suffix)
	if !ok || id == "" || strings.ContainsRune(id, '/') {
		return "", false
	}
	return id, true
}

func authorized(r *http.Request, token string) bool {
	if token == "" {
		return true
	}
	return bearerMatches(r, token)
}

// bearerMatches reports whether the request's Authorization header carries
// exactly "Bearer <token>". Unlike authorized it has no open-mode bypass: an
// empty token matches nothing.
func bearerMatches(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	// Constant-time compare so a network attacker cannot recover the token one
	// byte at a time from response-timing differences.
	expected := "Bearer " + token
	presented := r.Header.Get("Authorization")
	return subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) == 1
}

// credentialNodeClass resolves the relay-registration credential to the
// highest node class it may vouch for, or ok=false when the request is not
// authorized at all. The foundation token is checked first: with anonymous
// registration enabled (RegistrationToken empty) every request already passes
// the volunteer-class check, and the foundation credential must still win.
func credentialNodeClass(r *http.Request, cfg Config) (string, bool) {
	if bearerMatches(r, cfg.FoundationToken) {
		return relay.NodeClassFoundation, true
	}
	if authorized(r, cfg.RegistrationToken) {
		return relay.NodeClassVolunteer, true
	}
	return "", false
}

// writeJSON streams v via Encode, which appends a trailing newline. Signed
// relay-list responses must use signer.writeSigned instead, so the bytes on
// the wire are exactly the bytes that were signed.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, relay.ErrorResponse{Error: message})
}

// relayDisplayResolver returns a function that maps active relay IDs to their
// operator-supplied label and broker-attested node class, for decorating the
// admin dashboard. Unlabeled relays are included too: their views fall back to
// the relay ID for the name but still show the class beside it.
func relayDisplayResolver(store RelayStore) func() map[string]relayDisplay {
	return func() map[string]relayDisplay {
		descriptors, err := store.List(time.Now().UTC(), 0)
		if err != nil {
			return nil
		}
		displays := make(map[string]relayDisplay, len(descriptors))
		for _, desc := range descriptors {
			displays[desc.ID] = relayDisplay{Label: desc.Label, NodeClass: desc.NodeClass}
		}
		return displays
	}
}
