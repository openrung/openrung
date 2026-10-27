package broker

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"unicode"

	"openrung/internal/relay"
)

// maxRegisterBodyBytes caps relay registration payloads; descriptors are
// small structs, so anything near the cap is malformed or hostile.
const maxRegisterBodyBytes = 64 << 10

// Relay-list page sizes applied when Config leaves them zero.
const (
	DefaultRelayListLimit    = 5
	DefaultRelayListMaxLimit = 20
)

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
	// Version and Revision identify the running broker build in the admin
	// dashboard's footer; cmd/broker passes buildinfo's values. Empty renders
	// "dev" with no revision.
	Version  string
	Revision string
	// APIToken is the bearer credential for the machine-to-machine operational
	// API (GET /admin/api/relays/inventory). It is deliberately separate from
	// DashboardToken — that one buys an interactive browser session over the
	// telemetry corpus, this one buys a signed fleet snapshot — and from the
	// registration tokens, so operational tooling never carries a credential
	// that could write to the directory. Empty leaves the route unregistered,
	// so an unconfigured broker 404s instead of running the endpoint open;
	// cmd/broker refuses to start when it equals any other broker credential.
	APIToken string
	// TrustedProxyCIDRs are the only CIDRs whose forwarded CF-Connecting-IP / X-Forwarded-For
	// headers the broker will trust for the real client IP. Empty trusts no proxy.
	TrustedProxyCIDRs []string
	// MaxNewRelayIDsPerIPPerDay caps how many NEW relay identities one source
	// IP (IPv6: one /64) may successfully register per rolling 24 h, so an
	// anonymous-registration broker cannot be used to mint attested telemetry
	// subjects and directory rows by the thousand. Re-registrations of a known
	// identity and heartbeats never count. Zero applies the default
	// (defaultMaxNewRelayIDsPerDay); a negative value disables the cap.
	MaxNewRelayIDsPerIPPerDay int
	// RegistrationCapExemptCIDRs lists sources exempt from
	// MaxNewRelayIDsPerIPPerDay — infrastructure that legitimately registers
	// many first-time identities from one address, i.e. the relay hub fronting
	// a wave of new tunnel volunteers. Requests presenting the foundation
	// token are always exempt.
	RegistrationCapExemptCIDRs []string
	// GeoIP resolves the city/country of a relay's public endpoint so clients
	// can show where relays are located. Nil disables lookups;
	// descriptors then carry empty geo fields.
	GeoIP GeoIPResolver
	// RelayProber checks that a direct-transport relay's advertised endpoint
	// accepts inbound connections, at registration and again on heartbeat
	// every RelayProbeInterval (zero applies defaultRelayProbeInterval). A
	// relay whose latest probe failed stays registered but is withheld from
	// the public directory and access answers until a probe succeeds. Nil
	// disables probing: every relay is listed on its lease alone.
	RelayProber        RelayProber
	RelayProbeInterval time.Duration
	// SigningSeed is the 32-byte Ed25519 seed that signs every relay-list
	// response body. Required: cmd/broker validates OPENRUNG_RELAY_SIGNING_KEY
	// with ParseSigningSeed and refuses to start without it, because serving
	// unsigned lists is an invisible outage for verifying clients.
	SigningSeed []byte
	// WSSTicketSigningSeed is a dedicated Ed25519 seed for short-lived WSS
	// tickets. Keeping it separate from relay-list signing permits independent
	// rotation; sidecars accept the new and old public keys during overlap.
	WSSTicketSigningSeed []byte
	// WSSTicketTTL and WSSTicketMaxStreams bound each issued ticket
	// (OPENRUNG_WSS_TICKET_TTL, OPENRUNG_WSS_TICKET_MAX_STREAMS); zero applies
	// the defaults.
	WSSTicketTTL        time.Duration
	WSSTicketMaxStreams int
	// RateLimits overrides the per-client budgets of the public endpoints
	// (OPENRUNG_RATE_LIMIT_*); zero fields apply the defaults.
	RateLimits RateLimits
	// RelayListDefaultLimit is the relay-list page size when a request names
	// none, and RelayListMaxLimit the largest a request may ask for
	// (OPENRUNG_RELAY_LIST_DEFAULT_LIMIT, OPENRUNG_RELAY_LIST_MAX_LIMIT). Zero
	// applies the defaults; a request above the maximum is refused with 400.
	RelayListDefaultLimit int
	RelayListMaxLimit     int
	// UserTokenKey is the 32-byte HMAC key behind client access tokens
	// (OPENRUNG_USER_TOKEN_KEY). It enables the user challenge/auth endpoints
	// and the operational user routes; empty leaves all of them unregistered,
	// like APIToken. The store must then implement AccessStore. Rotating the
	// key invalidates every outstanding token at once.
	UserTokenKey []byte
	// AccessTokenTTL bounds how long an access token authenticates; zero
	// applies the default (24 h). User status is re-checked on every
	// authorization, so the TTL is a convenience bound, not a security one.
	AccessTokenTTL time.Duration
	// EnrollmentBudget limits NEW user identities per source address (IPv6:
	// per /64). Zero fields apply the defaults; see EnrollmentBudget.
	EnrollmentBudget EnrollmentBudget
	// SourceClassifier refuses NEW user identities from datacenter networks
	// (source_class.go); nil disables the check. The caller keeps it fresh
	// (SourceClassifier.Run).
	SourceClassifier *SourceClassifier
	// AccessLeaseTTL is how long an issued per-client relay credential stays
	// valid without renewal (OPENRUNG_ACCESS_LEASE_TTL); zero applies the
	// default (1 h). Renewal extends the same credential.
	AccessLeaseTTL time.Duration
	// MaxLeasesPerUser caps one user's live leases across the fleet
	// (OPENRUNG_MAX_LEASES_PER_USER, default 10); at the cap the least
	// recently renewed leases outside a request are revoked to make room.
	// MaxLeasesPerRelay caps one relay's live leases
	// (OPENRUNG_MAX_LEASES_PER_RELAY, default 2000); a relay at its cap is
	// skipped by issuance.
	MaxLeasesPerUser  int
	MaxLeasesPerRelay int
}

func NewServer(store RelayStore, cfg Config) http.Handler {
	if cfg.RelayLeaseTTL == 0 {
		cfg.RelayLeaseTTL = 3 * time.Minute
	}
	if cfg.RelayProbeInterval == 0 {
		cfg.RelayProbeInterval = defaultRelayProbeInterval
	}
	if cfg.RelayListMaxLimit == 0 {
		cfg.RelayListMaxLimit = max(DefaultRelayListMaxLimit, cfg.RelayListDefaultLimit)
	}
	if cfg.RelayListDefaultLimit == 0 {
		cfg.RelayListDefaultLimit = min(DefaultRelayListLimit, cfg.RelayListMaxLimit)
	}
	relaySigner := newSigner(cfg.SigningSeed)
	wssIssuer := newWSSTicketIssuer(cfg)
	clientIP := newClientIPResolver(cfg.TrustedProxyCIDRs)
	clientSeen := newClientSeenDeduper(clientSeenDedupWindow, clientSeenDedupMaxEntries)
	rateLimits := cfg.RateLimits.withDefaults()
	relayListLimiter := rateLimits.RelayList.limiter()
	telemetryLimiter := rateLimits.Telemetry.limiter()
	speedTestLimiter := rateLimits.SpeedTest.limiter()
	relayRegistrationLimiter := rateLimits.RelayRegistration.limiter()
	wssTicketLimiter := rateLimits.WSSTicket.limiter()
	relayLedger := newRelayIDLedger(relayLedgerTTL, relayLedgerMaxEntries)
	seedRelayLedger(relayLedger, store)
	newIDCap := newNewRelayCap(cfg.MaxNewRelayIDsPerIPPerDay, newRelayCapWindow, cfg.RegistrationCapExemptCIDRs)
	registerRelay := rateLimited(relayRegistrationLimiter, clientIP, 10, registerHandler(store, cfg, clientIP, relayLedger, newIDCap))
	heartbeatRelay := rateLimited(relayRegistrationLimiter, clientIP, 10, heartbeatHandler(store, cfg, relayLedger))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := store.Ping(r.Context()); err != nil {
			slog.Error("broker health check failed", "error", err)
			writeError(w, http.StatusServiceUnavailable, "broker storage unavailable")
			return
		}
		// signing_key_id is public data (it ships in every relay-list body) and
		// lets the monitor assert the active key without parsing a relay list.
		response := map[string]any{"ok": true, "signing_key_id": relaySigner.keyID}
		if wssIssuer != nil {
			response["wss_ticket_key_id"] = wssIssuer.signer.KeyID()
		}
		if cfg.SourceClassifier != nil {
			response["source_classification_available"] = cfg.SourceClassifier.Available(time.Now())
		}
		writeJSON(w, http.StatusOK, response)
	})
	mux.HandleFunc("POST /api/v1/relays/register", registerRelay)
	mux.HandleFunc("POST /api/v1/relays/", heartbeatRelay)
	// Relay lease synchronisation authenticates with the registration's lease
	// token, not the registration bearer, so it needs no token configuration;
	// it exists whenever the store can hold leases. The ack pattern is more
	// specific than the heartbeat prefix pattern above, so ServeMux routes it
	// here rather than to the heartbeat handler.
	access := newAccessService(store, cfg)
	if access != nil {
		mux.HandleFunc("GET /api/v1/relays/{id}/leases", rateLimited(relayRegistrationLimiter, clientIP, 10, access.leasePollHandler()))
		mux.HandleFunc("POST /api/v1/relays/{id}/leases/ack", rateLimited(relayRegistrationLimiter, clientIP, 10, access.leaseAckHandler()))
	}
	mux.HandleFunc("GET /api/v1/relays", rateLimited(relayListLimiter, clientIP, 10, listRelaysHandler(store, cfg.TelemetrySink, clientIP, clientSeen, relaySigner, cfg.RelayProber != nil, cfg.RelayListDefaultLimit, cfg.RelayListMaxLimit)))
	if wssIssuer != nil {
		mux.HandleFunc("POST /api/v1/wss/tickets", rateLimitedBy(wssTicketLimiter, wssTicketRateKey(clientIP), 10, wssTicketHandler(store, wssIssuer)))
	}
	// The operational API exists only when its dedicated token does, so an
	// unconfigured broker runs no handler, holds no limiter state, and can
	// answer nothing but 404 on these paths — the same posture as the
	// dashboard routes. The patterns deliberately carry no method: each
	// handler screens the method itself, after the credential check, so
	// ServeMux cannot answer ahead of it (see relayInventoryHandler). One
	// limiter spans the whole operational surface: it is one credential
	// driven by one operator's tooling, and the budget is per token holder,
	// not per route.
	users := newUserAuthService(store, cfg, clientIP)
	if cfg.APIToken != "" {
		operationalLimiter := newIPRateLimiter(inventoryRatePerSecond, inventoryBurst, rateLimiterMaxTrackedIPs)
		operational := func(next http.HandlerFunc) http.HandlerFunc {
			return rateLimited(operationalLimiter, clientIP, inventoryRetryAfterSeconds, next)
		}
		mux.HandleFunc("/admin/api/relays/inventory", operational(relayInventoryHandler(store, cfg.APIToken, relaySigner)))
		mux.HandleFunc("/admin/api/relays/weights", operational(relayWeightsListHandler(store, cfg.APIToken)))
		mux.HandleFunc("/admin/api/relays/{id}/weight", operational(relayWeightHandler(store, cfg.APIToken, clientIP.clientIP)))
		if users != nil {
			if directory, ok := store.(userDirectory); ok {
				mux.HandleFunc("/admin/api/users", operational(adminUserListHandler(directory, cfg.APIToken)))
			}
			mux.HandleFunc("/admin/api/users/{id}", operational(adminUserHandler(users.store, cfg.APIToken)))
			mux.HandleFunc("/admin/api/users/{id}/status", operational(adminUserStatusHandler(access.setUserStatus, cfg.APIToken, clientIP.clientIP)))
		}
		if access != nil {
			mux.HandleFunc("/admin/api/leases/{id}", operational(access.adminLeaseHandler(cfg.APIToken)))
		}
	}
	// Client access issues per-user relay credentials; it needs both the
	// token key (users) and a store that can hold leases (access).
	if clientAccess := newClientAccessHandler(store, users, access, relaySigner); clientAccess != nil {
		clientAccess.withholdUnreachable = cfg.RelayProber != nil
		clientAccess.clientIP = clientIP.clientIP
		accessLimiter := rateLimits.Access.limiter()
		mux.HandleFunc("POST /api/v1/access", clientAccess.counted(rateLimitedBy(accessLimiter, clientAccess.rateKey(clientIP), 10, clientAccess.handler())))
	}
	// Client user endpoints exist only with an access token key configured.
	// One limiter spans both: a challenge is worthless without the auth call
	// that consumes it, so budgeting them together bounds the pair.
	if users != nil {
		userAuthLimiter := rateLimits.UserAuth.limiter()
		mux.HandleFunc("POST /api/v1/users/challenge", rateLimited(userAuthLimiter, clientIP, 10, users.challengeHandler()))
		mux.HandleFunc("POST /api/v1/users/auth", rateLimited(userAuthLimiter, clientIP, 10, users.authHandler()))
	}
	mux.HandleFunc("POST /api/v1/telemetry/events", rateLimited(telemetryLimiter, clientIP, 10, telemetryHandler(cfg.TelemetrySink, store, clientIP, relayLedger)))
	mux.HandleFunc("GET /api/v1/speed-test", rateLimited(speedTestLimiter, clientIP, 30, speedTestHandler(speedTestMaxConcurrent)))
	querier := cfg.TelemetryQuerier
	if querier == nil && cfg.TelemetryReader != nil {
		querier = newTelemetryReaderQuerier(cfg.TelemetryReader)
	}
	if cfg.DashboardToken != "" && querier != nil {
		dashboard := newDashboardServer(cfg.DashboardToken, querier)
		dashboard.version, dashboard.revision = cfg.Version, cfg.Revision
		dashboard.relayDisplays = relayDisplayResolver(store)
		dashboard.relayDirectory = store
		dashboard.relayWeights = store
		dashboard.clientIP = clientIP.clientIP
		// The users page follows the operational user routes: it exists
		// with client users (an access token key) and shares their store
		// and status implementation.
		if directory, ok := store.(userDirectory); ok && users != nil {
			dashboard.users = directory
			dashboard.userStatus = access.setUserStatus
		}
		dashboard.register(mux)
	}

	return mux
}

func registerHandler(store RelayStore, cfg Config, clientIP *clientIPResolver, ledger *relayIDLedger, newIDCap *newRelayCap) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Stable registrations return a bearer-like lease token. Keep it out of
		// browser, intermediary, and edge caches on success and every error path.
		w.Header().Set("Cache-Control", "no-store")
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

		contact, err := relay.NormalizeOperatorContact(req.OperatorContact)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.OperatorContact = contact

		accessHost, err := relay.NormalizeAccessHost(req.AccessHost)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.AccessHost = accessHost

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
		if len(req.WSSFronts) > 0 && len(cfg.WSSTicketSigningSeed) == 0 {
			writeError(w, http.StatusServiceUnavailable, "WSS ticket issuance is not configured")
			return
		}

		if err := validateRegisterRequest(req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		now := time.Now().UTC()
		// A registration that re-proves a known identity is a reconnect (tunnel
		// relays do this on most rejoins), not a new relay; only genuinely new
		// identities draw on the per-source budget. The proof is checked again
		// inside the store — this early pass only pre-derives the would-be ID,
		// and on an invalid proof leaves the store's error mapping (including
		// the expired-proof message the hub matches on) untouched. Legacy
		// requests without identity fields mint a random ID, so each successful
		// one is by definition a new identity. Foundation credentials are
		// operator infrastructure: they neither hit the cap nor consume any of
		// the address's anonymous budget.
		newIdentity := true
		if identityKey, err := relay.VerifyIdentity(req, now); err == nil && identityKey != nil {
			_, known := ledger.lookup(relay.DeriveRelayID(identityKey), now)
			newIdentity = !known
		}
		// The budget unit is reserved atomically up front — a check here with a
		// recording after the store call would let concurrent registrations all
		// pass the check and finish over the limit — and handed back on every
		// failure path, so only registrations the store accepts consume it (a
		// relay crash-looping on a rejected request cannot exhaust its own cap).
		keepReservation := false
		if newIdentity && maxClass != relay.NodeClassFoundation {
			release, allowed := newIDCap.reserve(clientIP.clientIP(r), now)
			if !allowed {
				w.Header().Set("Retry-After", strconv.Itoa(newRelayCapRetryAfterSeconds))
				writeError(w, http.StatusTooManyRequests, "too many new relay registrations from this address, retry later")
				return
			}
			defer func() {
				if !keepReservation {
					release()
				}
			}()
		}

		// The relay listens before it registers, so its endpoint is probed in
		// the same request, before anything is stored: a port nothing outside
		// the host can reach is rejected with the reason, and the relay's own
		// retry loop re-registers (and is re-probed) until it opens. See
		// reachability.go. An inconclusive probe registers the relay unprobed.
		probed, probeErr := probeRegistration(r.Context(), cfg.RelayProber, req)
		if probeErr != nil {
			slog.Warn("relay registration rejected: endpoint unreachable", "node_class", maxClass, "public", req.PublicHost, "port", req.PublicPort, "version", req.RelayVersion, "error", probeErr)
			writeError(w, http.StatusUnprocessableEntity, unreachableRegistrationMessage(req))
			return
		}

		desc, err := store.Register(req, now, cfg.RelayLeaseTTL)
		// A malformed or stale identity proof fails the registration loudly
		// instead of silently falling back to a random relay ID — a relay that
		// believes it has a stable identity should crash-loop where its
		// operator can see it, mirroring the foundation-token posture above.
		// The exact expired message is a contract: the relay hub matches it to
		// recycle a tunnel session whose stored proof has aged out.
		if errors.Is(err, relay.ErrIdentityProofExpired) || errors.Is(err, relay.ErrIdentityProofInvalid) || errors.Is(err, relay.ErrIdentityIncomplete) ||
			errors.Is(err, relay.ErrWSSCapabilityExpired) || errors.Is(err, relay.ErrWSSCapabilityInvalid) || errors.Is(err, relay.ErrWSSCapabilityIncomplete) {
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
		// Commit the identity to the ledger, and keep the reserved unit only if
		// this request was the commit that established it: concurrent first
		// registrations of the same stable identity all reserve (they check
		// newIdentity before the store call), but they mint one relay between
		// them — the losers of the commit race hand their units back so one
		// identity can never drain more than one unit of an address's budget.
		keepReservation = ledger.rememberIfNew(desc.ID, desc.NodeClass, now)
		resolveRelayGeo(r.Context(), store, cfg.GeoIP, &desc)
		if probed {
			recordReachability(store, &desc, true, now)
		}
		slog.Info("relay registered", "relay_id", desc.ID, "node_class", desc.NodeClass, "public", desc.PublicHost, "port", desc.PublicPort, "city", desc.City, "country", desc.Country, "max_sessions", desc.MaxSessions, "version", desc.RelayVersion, "reachable", reachabilityLogValue(desc))

		writeJSON(w, http.StatusCreated, relay.RegisterResponse{Descriptor: desc})
	}
}

func heartbeatHandler(store RelayStore, cfg Config, ledger *relayIDLedger) http.HandlerFunc {
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
		// Every real relay ID is broker-minted and well-formed; anything else
		// (including raw bytes smuggled through percent-encoded path segments,
		// which some stores cannot even compare against) is answered like any
		// other unknown relay.
		if !relay.WellFormedRelayID(id) {
			writeError(w, http.StatusNotFound, "relay not found")
			return
		}
		var heartbeat relay.HeartbeatRequest
		r.Body = http.MaxBytesReader(w, r.Body, maxRegisterBodyBytes)
		if err := json.NewDecoder(r.Body).Decode(&heartbeat); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid JSON request")
			return
		}

		// A rotated credential is bounded and checked exactly like the
		// registration-time one (it lands in the same column and the same
		// signed directory): opaque to the broker, but storable printable text.
		if len(heartbeat.ClientID) > maxRegisterFieldBytes || !storableField(heartbeat.ClientID) {
			writeError(w, http.StatusBadRequest, "client_id must be at most 128 characters of printable text")
			return
		}

		now := time.Now().UTC()
		desc, err := store.Heartbeat(id, heartbeat.LeaseToken, maxClass, heartbeat.ClientID, now, cfg.RelayLeaseTTL)
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
		// Refresh the ledger on every renewal: a long-lived relay that only ever
		// heartbeats must keep its post-expiry telemetry coverage no matter how
		// long ago it last registered.
		ledger.remember(id, desc.NodeClass, now)

		// Backfill relays whose registration-time lookup failed (or that
		// registered before the broker resolved locations at all).
		resolveRelayGeo(r.Context(), store, cfg.GeoIP, &desc)
		// Re-probe the endpoint once the stored result is old enough, so a
		// relay that went dark is withheld and a repaired one is relisted
		// without waiting for a re-registration.
		probeRelayReachability(r.Context(), store, cfg.RelayProber, cfg.RelayProbeInterval, &desc, now)

		// Echo the credential now being served so a rotating relay can retire
		// its predecessor only once the directory has moved on (a legacy
		// identityless registration sees its credential unchanged here).
		writeJSON(w, http.StatusOK, relay.HeartbeatResponse{OK: true, ExpiresAt: desc.ExpiresAt, ClientID: desc.ClientID})
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
	if err := store.UpdateGeo(desc.ID, desc.LeaseToken, geo); err != nil {
		if errors.Is(err, ErrRelayNotFound) {
			// A newer stable-ID registration replaced this descriptor while the
			// lookup was in flight. Its own handler will resolve the current exit.
			return
		}
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

// withholdUnreachable is whether reachability probing is enabled, and so
// whether a stored failed probe keeps a relay off the page.
func listRelaysHandler(store RelayStore, telemetrySink TelemetrySink, clientIP *clientIPResolver, clientSeen *clientSeenDeduper, s signer, withholdUnreachable bool, defaultLimit, maxLimit int) http.HandlerFunc {
	limitError := "limit must be between 1 and " + strconv.Itoa(maxLimit)
	return func(w http.ResponseWriter, r *http.Request) {
		// Relay availability is real-time: shared caches (Cloudflare's edge in
		// production) must never store a copy, and clients cannot bust an edge
		// cache from their side. Set on every path so errors aren't cached
		// either; the signed success path upgrades it to no-store, no-transform.
		w.Header().Set("Cache-Control", "no-store")
		recordClientSeen(r, telemetrySink, clientIP, clientSeen)
		limit := defaultLimit
		if raw := r.URL.Query().Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > maxLimit {
				writeError(w, http.StatusBadRequest, limitError)
				return
			}
			limit = parsed
		}

		now := time.Now().UTC()
		// Ask both stores for the ranked set, then reserve one already-advertised
		// per-relay WSS-capable Foundation descriptor in a short page. This never
		// attaches a shared URL or changes ordering when the page already has one.
		relays, weights, err := store.ListRanked(now, 0)
		if err != nil {
			slog.Error("could not list relays", "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not list relays")
			return
		}
		relays = reserveWSSCandidate(withoutUnreachable(relays, withholdUnreachable), limit, weights)
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

// seedRelayLedger pre-populates the ledger with the store's active relays so a
// broker restart (where the Postgres store keeps its fleet) does not forget
// which IDs are real. Best-effort: on error the ledger simply refills from the
// fleet's own heartbeats within one lease TTL.
func seedRelayLedger(ledger *relayIDLedger, store RelayStore) {
	now := time.Now().UTC()
	descriptors, err := store.List(now, 0)
	if err != nil {
		slog.Warn("could not seed relay ledger from store", "error", err)
		return
	}
	for _, desc := range descriptors {
		ledger.remember(desc.ID, desc.NodeClass, now)
	}
}

// Length caps for registration fields. Every descriptor field is served to all
// clients in the signed relay list, so oversized values inflate every list
// response; the caps are generous multiples of the largest legitimate values
// (hostnames, UUIDs, base64 keys, semver strings).
const (
	maxRegisterHostBytes  = 253 // DNS name length limit; IP literals are far shorter
	maxRegisterFieldBytes = 128
)

func validateRegisterRequest(req relay.RegisterRequest) error {
	switch {
	case req.PublicHost == "":
		return errors.New("public_host is required")
	case !validEndpointHost(req.PublicHost):
		return errors.New("public_host must be a DNS hostname or IP address")
	case req.PublicPort < 1 || req.PublicPort > 65535:
		return errors.New("public_port must be between 1 and 65535")
	case req.Protocol != relay.ProtocolVLESSRealityVision:
		return errors.New("protocol must be vless-reality-vision")
	case req.ClientID == "" || len(req.ClientID) > maxRegisterFieldBytes:
		return errors.New("client_id is required and must be at most 128 characters")
	case req.RealityPublicKey == "" || len(req.RealityPublicKey) > maxRegisterFieldBytes:
		return errors.New("reality_public_key is required and must be at most 128 characters")
	case req.ShortID == "" || len(req.ShortID) > maxRegisterFieldBytes:
		return errors.New("short_id is required and must be at most 128 characters")
	case req.ServerName == "" || !validEndpointHost(req.ServerName):
		return errors.New("server_name must be a DNS hostname or IP address")
	case req.Flow != relay.FlowVision:
		return errors.New("flow must be xtls-rprx-vision")
	case req.ExitMode != relay.ExitModeDirect && req.ExitMode != relay.ExitModeDedicated:
		return errors.New("exit_mode must be direct or dedicated")
	case req.Transport != "" && req.Transport != relay.TransportDirect && req.Transport != relay.TransportTunnel:
		return errors.New("transport must be direct or tunnel")
	case req.ExitHost != "" && req.Transport != relay.TransportTunnel:
		return errors.New("exit_host is only allowed for tunnel transport")
	case req.ExitHost != "" && !validEndpointHost(req.ExitHost):
		return errors.New("exit_host must be a DNS hostname or IP address")
	case req.AccessHost != "" && normalizeTransport(req.Transport) != relay.TransportDirect:
		return errors.New("access_host is only allowed for direct transport")
	case req.AccessHost != "" && sameIPAddress(req.AccessHost, req.PublicHost):
		return errors.New("access_host must differ from public_host")
	case req.MaxSessions < 1:
		return errors.New("max_sessions must be at least 1")
	case req.MaxMbps < 1:
		return errors.New("max_mbps must be at least 1")
	case len(req.RelayVersion) > maxRegisterFieldBytes:
		return errors.New("relay_version must be at most 128 characters")
	case len(req.PunchEndpoint) > 2*maxRegisterFieldBytes:
		return errors.New("punch_endpoint must be at most 256 characters")
	case !registerTextValid(req):
		// NUL bytes and invalid UTF-8 are unstorable in Postgres text/jsonb
		// columns and can never appear in a legitimate registration.
		return errors.New("registration fields must be valid UTF-8 without control characters")
	default:
		return nil
	}
}

// sameIPAddress reports whether two hosts are IP literals naming the same
// address, however they are spelled.
func sameIPAddress(a, b string) bool {
	addrA, errA := netip.ParseAddr(a)
	addrB, errB := netip.ParseAddr(b)
	return errA == nil && errB == nil && addrA.Unmap() == addrB.Unmap()
}

// validEndpointHost accepts an IP literal or a syntactically valid DNS name.
// Directory hygiene, not reachability: these values are served verbatim to
// every client, so free-form strings must not be storable as relay endpoints.
func validEndpointHost(host string) bool {
	if len(host) > maxRegisterHostBytes {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 ||
			label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range []byte(label) {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

// registerTextValid vets the free-form registration fields that are not
// already constrained to a safe charset by their own validation. Unlike
// telemetry attributes (where multi-line error detail is plausible), no
// registration field legitimately contains any control character —
// unicode.IsControl covers the whole Cc category, C1 (U+0080–U+009F, e.g.
// CSI) included, not just C0 and DEL.
func registerTextValid(req relay.RegisterRequest) bool {
	for _, value := range []string{req.ClientID, req.RealityPublicKey, req.ShortID, req.RelayVersion, req.PunchEndpoint} {
		if !storableField(value) {
			return false
		}
	}
	return true
}

// storableField reports whether a relay-supplied text field can be stored
// and served: valid UTF-8 without NUL (Postgres text/jsonb refuse it) and
// without control characters. Registration and heartbeat share it.
func storableField(value string) bool {
	if !storableText(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
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
