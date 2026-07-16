package broker

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	dashboardCookieName = "openrung_admin_session"
	dashboardSessionTTL = 12 * time.Hour

	// The dedicated sessions endpoint pages through the full window; the
	// overview no longer embeds a session preview.
	sessionsDefaultPageSize = 25
	sessionsMaxPageSize     = 100
)

//go:embed dashboard.html
var dashboardHTML []byte

// relayDisplay is how the dashboard identifies an active relay: its
// operator-supplied label (may be empty, in which case views fall back to the
// relay ID) and its broker-attested node class, which every view renders
// alongside the name.
type relayDisplay struct {
	Label     string
	NodeClass string
}

type dashboardServer struct {
	tokenHash     [32]byte
	querier       TelemetryQuerier
	relayDisplays func() map[string]relayDisplay
	now           func() time.Time
	mu            sync.Mutex
	sessions      map[string]time.Time
}

func newDashboardServer(token string, querier TelemetryQuerier) *dashboardServer {
	return &dashboardServer{
		tokenHash: sha256.Sum256([]byte(token)),
		querier:   querier,
		now:       time.Now,
		sessions:  make(map[string]time.Time),
	}
}

func (d *dashboardServer) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/telemetry/login", d.loginPage)
	mux.HandleFunc("POST /admin/telemetry/login", d.login)
	mux.HandleFunc("POST /admin/telemetry/logout", d.logout)
	mux.HandleFunc("GET /admin/telemetry", d.requireAuth(d.dashboard))
	mux.HandleFunc("GET /admin/api/telemetry/overview", d.requireAuth(d.overview))
	mux.HandleFunc("GET /admin/api/telemetry/sessions", d.requireAuth(d.listSessions))
}

func (d *dashboardServer) loginPage(w http.ResponseWriter, r *http.Request) {
	if d.authenticated(r) {
		http.Redirect(w, r, "/admin/telemetry", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>OpenRung telemetry login</title><style>body{margin:0;background:#07100b;color:#e9f7ee;font:16px ui-monospace,SFMono-Regular,Menlo,monospace;display:grid;min-height:100vh;place-items:center}.box{width:min(88vw,390px);padding:30px;border:1px solid #245c37;background:#0b1710;box-shadow:0 18px 80px #0008}h1{font-size:22px;color:#55e27b}label,input,button{display:block;width:100%;box-sizing:border-box}label{margin:22px 0 8px;color:#a9c9b3}input{padding:12px;background:#06100a;border:1px solid #327448;color:#fff;font:inherit}button{margin-top:16px;padding:12px;border:0;background:#55e27b;color:#061008;font:700 15px inherit;cursor:pointer}.error{color:#ff7f87}</style></head><body><main class="box"><h1>OpenRung telemetry</h1><p>Administrator access</p><form method="post" action="/admin/telemetry/login"><label for="token">Dashboard token</label><input id="token" name="token" type="password" autocomplete="current-password" required autofocus><button type="submit">Sign in</button></form></main></body></html>`))
}

func (d *dashboardServer) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid login request")
		return
	}
	presented := sha256.Sum256([]byte(r.FormValue("token")))
	if subtle.ConstantTimeCompare(presented[:], d.tokenHash[:]) != 1 {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "invalid dashboard token", http.StatusUnauthorized)
		return
	}

	sessionBytes := make([]byte, 32)
	if _, err := rand.Read(sessionBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "could not create dashboard session")
		return
	}
	sessionID := base64.RawURLEncoding.EncodeToString(sessionBytes)
	expires := d.now().UTC().Add(dashboardSessionTTL)
	d.mu.Lock()
	d.removeExpiredLocked(d.now().UTC())
	d.sessions[sessionID] = expires
	d.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     dashboardCookieName,
		Value:    sessionID,
		Path:     "/admin",
		Expires:  expires,
		MaxAge:   int(dashboardSessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/admin/telemetry", http.StatusSeeOther)
}

func (d *dashboardServer) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(dashboardCookieName); err == nil {
		d.mu.Lock()
		delete(d.sessions, cookie.Value)
		d.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name: dashboardCookieName, Value: "", Path: "/admin", MaxAge: -1,
		HttpOnly: true, Secure: requestIsHTTPS(r), SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/admin/telemetry/login", http.StatusSeeOther)
}

func (d *dashboardServer) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Everything behind the admin gate is private and per-request — the
		// data API recomputes every few seconds — so no browser or shared
		// cache may store it. Set before dispatch so it covers the authorized
		// responses too, not just the redirect/401 below. Without this a plain
		// refresh (and the in-page Refresh button) could re-render a stale
		// cached response; only a hard reload, which bypasses the cache,
		// fetched fresh data.
		w.Header().Set("Cache-Control", "no-store")
		if !d.authenticated(r) {
			if strings.HasPrefix(r.URL.Path, "/admin/api/") {
				writeError(w, http.StatusUnauthorized, "dashboard session expired")
			} else {
				http.Redirect(w, r, "/admin/telemetry/login", http.StatusSeeOther)
			}
			return
		}
		next(w, r)
	}
}

func (d *dashboardServer) authenticated(r *http.Request) bool {
	cookie, err := r.Cookie(dashboardCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	now := d.now().UTC()
	d.mu.Lock()
	defer d.mu.Unlock()
	expires, ok := d.sessions[cookie.Value]
	if !ok || !expires.After(now) {
		delete(d.sessions, cookie.Value)
		return false
	}
	return true
}

func (d *dashboardServer) removeExpiredLocked(now time.Time) {
	for id, expires := range d.sessions {
		if !expires.After(now) {
			delete(d.sessions, id)
		}
	}
}

func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (d *dashboardServer) dashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	_, _ = w.Write(dashboardHTML)
}

func (d *dashboardServer) overview(w http.ResponseWriter, r *http.Request) {
	window, ok := dashboardWindow(r.URL.Query().Get("window"))
	if !ok {
		writeError(w, http.StatusBadRequest, "window must be 1h, 24h, or 7d")
		return
	}
	now := d.now().UTC()
	ov, err := d.querier.TelemetryOverview(now, window)
	if err != nil {
		slog.Error("could not build telemetry overview", "error", err)
		writeError(w, http.StatusInternalServerError, "could not build telemetry overview")
		return
	}
	if d.relayDisplays != nil {
		applyRelayDisplays(&ov, d.relayDisplays())
	}
	writeJSON(w, http.StatusOK, ov)
}

type sessionsPage struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Window      string           `json:"window"`
	Total       int              `json:"total"`
	Offset      int              `json:"offset"`
	Limit       int              `json:"limit"`
	Sessions    []sessionSummary `json:"sessions"`
}

func (d *dashboardServer) listSessions(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	window, ok := dashboardWindow(query.Get("window"))
	if !ok {
		writeError(w, http.StatusBadRequest, "window must be 1h, 24h, or 7d")
		return
	}
	offset, err := queryInt(query.Get("offset"), 0)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
		return
	}
	limit, err := queryInt(query.Get("limit"), sessionsDefaultPageSize)
	if err != nil || limit < 1 || limit > sessionsMaxPageSize {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be between 1 and %d", sessionsMaxPageSize))
		return
	}

	now := d.now().UTC()
	page, total, err := d.querier.TelemetrySessions(now, window, offset, limit)
	if err != nil {
		slog.Error("could not list telemetry sessions", "error", err)
		writeError(w, http.StatusInternalServerError, "could not list telemetry sessions")
		return
	}
	if offset > total {
		offset = total
	}
	if d.relayDisplays != nil {
		applySessionRelayDisplays(page, d.relayDisplays())
	}
	writeJSON(w, http.StatusOK, sessionsPage{
		GeneratedAt: now, Window: window.String(),
		Total: total, Offset: offset, Limit: limit, Sessions: page,
	})
}

func queryInt(raw string, fallback int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	return strconv.Atoi(raw)
}

func dashboardWindow(raw string) (time.Duration, bool) {
	switch raw {
	case "", "24h":
		return 24 * time.Hour, true
	case "1h":
		return time.Hour, true
	case "7d":
		return telemetryRetention, true
	default:
		return 0, false
	}
}

type telemetryOverview struct {
	GeneratedAt     time.Time      `json:"generated_at"`
	Window          string         `json:"window"`
	Totals          overviewTotals `json:"totals"`
	Trend           []trendPoint   `json:"trend"`
	TopRelays       []relaySummary `json:"top_relays"`
	TopApps         []countSummary `json:"top_applications"`
	TopCountries    []countSummary `json:"top_countries"`
	TopCities       []countSummary `json:"top_cities"`
	TopISPs         []countSummary `json:"top_isps"`
	ActiveRelays    []countSummary `json:"active_by_relay"`
	ActiveCountries []countSummary `json:"active_by_country"`
	ActiveCities    []countSummary `json:"active_by_city"`
	ActiveISPs      []countSummary `json:"active_by_isp"`
	ActiveOS        []countSummary `json:"active_by_os"`
	FailureStages   []countSummary `json:"failure_stages"`
	FailureReasons  []countSummary `json:"failure_reasons"`
	// Recent is the window's full session list, ordered newest-last-seen first.
	// It is not serialized (the dashboard reads sessions from the dedicated
	// endpoint): it exists only so the in-memory querier can paginate its
	// TelemetrySessions pages out of a single buildTelemetryOverview pass. The
	// Postgres querier leaves it nil and pages sessions with its own query.
	Recent     []sessionSummary   `json:"-"`
	SpeedTests []speedTestSummary `json:"speed_tests"`
}

type overviewTotals struct {
	Clients        int     `json:"clients"`
	Sessions       int     `json:"sessions"`
	Attempts       int     `json:"attempts"`
	Successes      int     `json:"successes"`
	Failures       int     `json:"failures"`
	ActiveClients  int     `json:"active_clients"`
	ActiveSessions int     `json:"active_sessions"`
	SuccessRate    float64 `json:"success_rate"`
}

type trendPoint struct {
	Time      time.Time `json:"time"`
	Attempts  int       `json:"attempts"`
	Successes int       `json:"successes"`
	Failures  int       `json:"failures"`
}

type countSummary struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	// NodeClass is set only for relay-keyed rankings (active_by_relay); the
	// city/country/ISP/OS/application rankings that share this shape leave it
	// empty so views render them without a class suffix.
	NodeClass string `json:"node_class,omitempty"`
	Count     int    `json:"count"`
}

type relaySummary struct {
	RelayID   string `json:"relay_id"`
	Label     string `json:"label,omitempty"`
	NodeClass string `json:"node_class,omitempty"`
	Successes int    `json:"successes"`
	Failures  int    `json:"failures"`
	// TopFailureReason is the modal relay_attempt_failed reason for this relay,
	// empty when the relay logged no failed attempts in the window.
	TopFailureReason string `json:"top_failure_reason,omitempty"`
}

type sessionSummary struct {
	SessionID       string `json:"session_id"`
	ClientID        string `json:"client_id"`
	SourceIP        string `json:"source_ip"`
	OperatingSystem string `json:"operating_system,omitempty"`
	DeviceInfo      string `json:"device_info,omitempty"`
	AppVersion      string `json:"app_version,omitempty"`
	Country         string `json:"country,omitempty"`
	City            string `json:"city,omitempty"`
	ISP             string `json:"isp,omitempty"`
	Organization    string `json:"organization,omitempty"`
	ASN             string `json:"asn,omitempty"`
	RelayID         string `json:"relay_id,omitempty"`
	RelayLabel      string `json:"relay_label,omitempty"`
	RelayNodeClass  string `json:"relay_node_class,omitempty"`
	Status          string `json:"status"`
	// Failure fields are populated only from connection_failed events; per field
	// the latest non-empty value wins, like the attribute accumulation above.
	FailureStage    string     `json:"failure_stage,omitempty"`
	FailureReason   string     `json:"failure_reason,omitempty"`
	FailureDetail   string     `json:"failure_detail,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	LastSeenAt      time.Time  `json:"last_seen_at"`
	DurationMS      int64      `json:"duration_ms,omitempty"`
	BytesSent       int64      `json:"bytes_sent,omitempty"`
	BytesReceived   int64      `json:"bytes_received,omitempty"`
	Active          bool       `json:"active"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
}

type speedTestSummary struct {
	RelayID       string  `json:"relay_id"`
	Label         string  `json:"label,omitempty"`
	NodeClass     string  `json:"node_class,omitempty"`
	Tests         int     `json:"tests"`
	AverageMbps   float64 `json:"average_mbps"`
	AverageTTFBMS float64 `json:"average_ttfb_ms"`
}

type sessionAccumulator struct {
	summary            sessionSummary
	deviceManufacturer string
	deviceModel        string
	isp                string
	observedClientIP   string
	reportedClientIP   string
	fallbackSourceIP   string
	lastHeartbeatAt    time.Time
	runningDurationMS  int64
	terminal           bool
	attempted          bool
	succeeded          bool
	failed             bool
}

// finalize derives the presentation fields from the accumulated raw ones. It
// is shared by the in-memory aggregator and the Postgres querier so both
// backends resolve source-IP/ISP precedence, device labels, status, and
// activity identically.
func (a *sessionAccumulator) finalize(now time.Time) sessionSummary {
	summary := a.summary
	summary.SourceIP = firstNonEmpty(a.observedClientIP, a.reportedClientIP, a.fallbackSourceIP)
	summary.ISP = firstNonEmpty(a.isp, summary.Organization, summary.ASN)
	summary.DeviceInfo = deviceInfoLabel(a.deviceManufacturer, a.deviceModel, summary.OperatingSystem)
	// connection_ended carries the final duration; until then, surface the
	// running duration from heartbeats so active sessions aren't blank.
	if summary.DurationMS == 0 {
		summary.DurationMS = a.runningDurationMS
	}
	switch {
	case a.failed:
		summary.Status = "failed"
	case a.succeeded:
		summary.Status = "succeeded"
	case a.attempted:
		summary.Status = "attempting"
	}
	if !a.lastHeartbeatAt.IsZero() {
		lastHeartbeatAt := a.lastHeartbeatAt
		summary.LastHeartbeatAt = &lastHeartbeatAt
	}
	summary.Active = !a.terminal && a.lastHeartbeatAt.After(now.Add(-activeSessionTimeout))
	return summary
}

func buildTelemetryOverview(records []TelemetryRecord, now time.Time, window time.Duration) telemetryOverview {
	start := now.Add(-window)
	clients := make(map[string]struct{})
	sessions := make(map[string]*sessionAccumulator)
	apps := make(map[string]int)
	failures := make(map[string]int)
	failureReasons := make(map[string]int)
	relays := make(map[string]*relaySummary)
	relayFailureReasons := make(map[string]map[string]int)
	type speedAccumulator struct {
		tests      int
		mbps, ttfb int64
	}
	speeds := make(map[string]*speedAccumulator)
	buckets := make(map[time.Time]*trendPoint)

	for bucket := start.Truncate(time.Hour); !bucket.After(now); bucket = bucket.Add(time.Hour) {
		buckets[bucket] = &trendPoint{Time: bucket}
	}
	for _, record := range records {
		event := record.Event
		if event.OccurredAt.Before(start) || event.OccurredAt.After(now) {
			continue
		}
		clients[event.ClientID] = struct{}{}
		session := sessions[event.SessionID]
		if session == nil {
			session = &sessionAccumulator{summary: sessionSummary{
				SessionID: event.SessionID, ClientID: event.ClientID,
				Status: "seen", StartedAt: event.OccurredAt, LastSeenAt: record.ReceivedAt,
			}}
			sessions[event.SessionID] = session
		}
		if record.SourceIP != "" {
			session.fallbackSourceIP = record.SourceIP
			if event.Event == "client_seen" {
				session.observedClientIP = record.SourceIP
			}
		}
		if clientIP := event.Attributes["client_ip"]; clientIP != "" {
			session.reportedClientIP = clientIP
		}
		if event.OccurredAt.Before(session.summary.StartedAt) {
			session.summary.StartedAt = event.OccurredAt
		}
		if record.ReceivedAt.After(session.summary.LastSeenAt) {
			session.summary.LastSeenAt = record.ReceivedAt
		}
		if event.RelayID != "" {
			session.summary.RelayID = event.RelayID
		}
		// Clients report their OS through one of three mutually exclusive keys:
		// desktop sends a ready-made operating_system label ("macOS (arm64)"),
		// iOS sends ios_version, and Android sends the android_api level.
		if operatingSystem := event.Attributes["operating_system"]; operatingSystem != "" {
			session.summary.OperatingSystem = operatingSystem
		} else if iosVersion := event.Attributes["ios_version"]; iosVersion != "" {
			session.summary.OperatingSystem = "iOS " + iosVersion
		} else if androidAPI := event.Attributes["android_api"]; androidAPI != "" {
			session.summary.OperatingSystem = "Android (API " + androidAPI + ")"
		}
		if manufacturer := event.Attributes["device_manufacturer"]; manufacturer != "" {
			session.deviceManufacturer = manufacturer
		}
		if model := event.Attributes["device_model"]; model != "" {
			session.deviceModel = model
		}
		if appVersion := event.Attributes["app_version"]; appVersion != "" {
			session.summary.AppVersion = appVersion
		}
		if country := firstNonEmpty(event.Attributes["country"], event.Attributes["country_code"]); country != "" {
			session.summary.Country = country
		}
		if city := event.Attributes["city"]; city != "" {
			session.summary.City = city
		}
		if organization := event.Attributes["organization"]; organization != "" {
			session.summary.Organization = organization
		}
		if asn := event.Attributes["asn"]; asn != "" {
			session.summary.ASN = asn
		}
		if isp := event.Attributes["isp"]; isp != "" {
			session.isp = isp
		}
		if event.Application != "" {
			apps[event.Application]++
		}
		// bytes_sent / bytes_received are cumulative per session (heartbeats carry
		// the running totals, connection_ended the final ones), so the largest
		// reported value wins regardless of event order.
		if sent := event.Measurements["bytes_sent"]; sent > session.summary.BytesSent {
			session.summary.BytesSent = sent
		}
		if received := event.Measurements["bytes_received"]; received > session.summary.BytesReceived {
			session.summary.BytesReceived = received
		}
		bucket := buckets[event.OccurredAt.Truncate(time.Hour)]
		switch event.Event {
		case "connection_attempted":
			session.attempted = true
			if bucket != nil {
				bucket.Attempts++
			}
		case "connection_succeeded":
			session.succeeded = true
			if bucket != nil {
				bucket.Successes++
			}
			if event.RelayID != "" {
				relayFor(relays, event.RelayID).Successes++
			}
		case "relay_failover":
			// A failover is a successful relay observation, but not another
			// session-level success or connection-trend success.
			if event.RelayID != "" {
				relayFor(relays, event.RelayID).Successes++
			}
		case "connection_failed":
			session.failed = true
			session.terminal = true
			if bucket != nil {
				bucket.Failures++
			}
			stageLabel := firstNonEmpty(event.Attributes["failure_stage"], "unknown")
			// New-style classified reason preferred; fall back to the error_type
			// mobile/CLI already send today.
			reason := firstNonEmpty(event.Attributes["failure_reason"], event.Attributes["error_type"])
			failures[stageLabel]++
			failureReasons[stageLabel+" · "+firstNonEmpty(reason, "unknown")]++
			if stage := event.Attributes["failure_stage"]; stage != "" {
				session.summary.FailureStage = stage
			}
			if reason != "" {
				session.summary.FailureReason = reason
			}
			if detail := event.Attributes["failure_detail"]; detail != "" {
				session.summary.FailureDetail = detail
			}
		case "relay_attempt_failed":
			if event.RelayID != "" {
				relayFor(relays, event.RelayID).Failures++
				reason := firstNonEmpty(event.Attributes["failure_reason"], event.Attributes["error_type"], "unknown")
				if relayFailureReasons[event.RelayID] == nil {
					relayFailureReasons[event.RelayID] = make(map[string]int)
				}
				relayFailureReasons[event.RelayID][reason]++
			}
		case "connection_ended":
			session.terminal = true
			session.summary.DurationMS = event.Measurements["session_duration_ms"]
		case "tunnel_stopped":
			session.terminal = true
		case "session_heartbeat":
			if record.ReceivedAt.After(session.lastHeartbeatAt) {
				session.lastHeartbeatAt = record.ReceivedAt
			}
			if elapsed := event.Measurements["session_duration_ms"]; elapsed > session.runningDurationMS {
				session.runningDurationMS = elapsed
			}
		case "speed_test_completed":
			if event.RelayID != "" {
				speed := speeds[event.RelayID]
				if speed == nil {
					speed = &speedAccumulator{}
					speeds[event.RelayID] = speed
				}
				speed.tests++
				speed.mbps += event.Measurements["download_mbps_milli"]
				speed.ttfb += event.Measurements["time_to_first_byte_ms"]
			}
		}
	}

	overview := telemetryOverview{GeneratedAt: now, Window: window.String()}
	overview.Totals.Clients, overview.Totals.Sessions = len(clients), len(sessions)
	countryCounts := make(map[string]int)
	cityCounts := make(map[string]int)
	ispCounts := make(map[string]int)
	activeClients := make(map[string]struct{})
	activeRelays := make(map[string]int)
	activeCountries := make(map[string]int)
	activeCities := make(map[string]int)
	activeISPs := make(map[string]int)
	activeOS := make(map[string]int)
	for _, session := range sessions {
		summary := session.finalize(now)
		if session.attempted {
			overview.Totals.Attempts++
		}
		if session.succeeded {
			overview.Totals.Successes++
		}
		if session.failed {
			overview.Totals.Failures++
		}
		incrementNonEmpty(countryCounts, summary.Country)
		incrementNonEmpty(cityCounts, summary.City)
		incrementNonEmpty(ispCounts, summary.ISP)
		if summary.Active {
			overview.Totals.ActiveSessions++
			activeClients[summary.ClientID] = struct{}{}
			incrementNonEmpty(activeRelays, summary.RelayID)
			incrementNonEmpty(activeCountries, summary.Country)
			incrementNonEmpty(activeCities, summary.City)
			incrementNonEmpty(activeISPs, summary.ISP)
			incrementNonEmpty(activeOS, summary.OperatingSystem)
		}
		overview.Recent = append(overview.Recent, summary)
	}
	overview.Totals.ActiveClients = len(activeClients)
	if overview.Totals.Attempts > 0 {
		overview.Totals.SuccessRate = float64(overview.Totals.Successes) / float64(overview.Totals.Attempts)
	}
	for _, point := range buckets {
		overview.Trend = append(overview.Trend, *point)
	}
	sort.Slice(overview.Trend, func(i, j int) bool { return overview.Trend[i].Time.Before(overview.Trend[j].Time) })
	// The session id tiebreak keeps the order stable across rebuilds — batched
	// uploads share one ReceivedAt, and an unstable order would make pages of
	// the sessions endpoint overlap or skip entries between requests.
	sort.Slice(overview.Recent, func(i, j int) bool {
		if overview.Recent[i].LastSeenAt.Equal(overview.Recent[j].LastSeenAt) {
			return overview.Recent[i].SessionID < overview.Recent[j].SessionID
		}
		return overview.Recent[i].LastSeenAt.After(overview.Recent[j].LastSeenAt)
	})
	for _, relay := range relays {
		relay.TopFailureReason = topFailureReason(relayFailureReasons[relay.RelayID])
		overview.TopRelays = append(overview.TopRelays, *relay)
	}
	sortTopRelays(overview.TopRelays)
	if len(overview.TopRelays) > 10 {
		overview.TopRelays = overview.TopRelays[:10]
	}
	overview.TopApps = sortedCounts(apps, 10)
	overview.TopCountries = sortedCounts(countryCounts, 10)
	overview.TopCities = sortedCounts(cityCounts, 10)
	overview.TopISPs = sortedCounts(ispCounts, 10)
	overview.ActiveRelays = sortedCounts(activeRelays, 10)
	overview.ActiveCountries = sortedCounts(activeCountries, 10)
	overview.ActiveCities = sortedCounts(activeCities, 10)
	overview.ActiveISPs = sortedCounts(activeISPs, 10)
	overview.ActiveOS = sortedCounts(activeOS, 10)
	overview.FailureStages = sortedCounts(failures, 10)
	overview.FailureReasons = sortedCounts(failureReasons, 10)
	for relayID, speed := range speeds {
		overview.SpeedTests = append(overview.SpeedTests, speedTestSummary{RelayID: relayID, Tests: speed.tests, AverageMbps: float64(speed.mbps) / float64(speed.tests) / 1000, AverageTTFBMS: float64(speed.ttfb) / float64(speed.tests)})
	}
	sortSpeedTests(overview.SpeedTests)
	return overview
}

// sortTopRelays and sortSpeedTests are shared by the in-memory aggregator and
// the Postgres querier so both rank identically.
func sortTopRelays(relays []relaySummary) {
	sort.Slice(relays, func(i, j int) bool {
		return relays[i].Successes+relays[i].Failures > relays[j].Successes+relays[j].Failures
	})
}

func sortSpeedTests(speedTests []speedTestSummary) {
	sort.Slice(speedTests, func(i, j int) bool { return speedTests[i].AverageMbps > speedTests[j].AverageMbps })
}

// applyRelayDisplays decorates every relay-keyed view in the overview with the
// relay's label and node class. A relay with no label keeps the ID as its name
// but still carries the class, so each view can render "name (class)".
func applyRelayDisplays(ov *telemetryOverview, displays map[string]relayDisplay) {
	if len(displays) == 0 {
		return
	}
	for i := range ov.TopRelays {
		display := displays[ov.TopRelays[i].RelayID]
		if display.Label != "" {
			ov.TopRelays[i].Label = display.Label
		}
		ov.TopRelays[i].NodeClass = display.NodeClass
	}
	// ov.Recent is not part of the overview response; the sessions endpoint
	// decorates its own page via applySessionRelayDisplays.
	for i := range ov.ActiveRelays {
		display := displays[ov.ActiveRelays[i].Name]
		if display.Label != "" {
			ov.ActiveRelays[i].Label = display.Label
		}
		ov.ActiveRelays[i].NodeClass = display.NodeClass
	}
	for i := range ov.SpeedTests {
		display := displays[ov.SpeedTests[i].RelayID]
		if display.Label != "" {
			ov.SpeedTests[i].Label = display.Label
		}
		ov.SpeedTests[i].NodeClass = display.NodeClass
	}
}

func applySessionRelayDisplays(sessions []sessionSummary, displays map[string]relayDisplay) {
	if len(displays) == 0 {
		return
	}
	for i := range sessions {
		display := displays[sessions[i].RelayID]
		if display.Label != "" {
			sessions[i].RelayLabel = display.Label
		}
		sessions[i].RelayNodeClass = display.NodeClass
	}
}

func relayFor(relays map[string]*relaySummary, id string) *relaySummary {
	relay := relays[id]
	if relay == nil {
		relay = &relaySummary{RelayID: id}
		relays[id] = relay
	}
	return relay
}

// topFailureReason returns the most frequent reason in counts. Ties resolve to
// the lexicographically smallest reason so the result is deterministic and the
// Postgres querier can reproduce it. Returns "" for an empty (or nil) map.
func topFailureReason(counts map[string]int) string {
	best, bestCount := "", 0
	for reason, count := range counts {
		if count > bestCount || (count == bestCount && reason < best) {
			best, bestCount = reason, count
		}
	}
	return best
}

func sortedCounts(values map[string]int, limit int) []countSummary {
	result := make([]countSummary, 0, len(values))
	for name, count := range values {
		result = append(result, countSummary{Name: name, Count: count})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Count == result[j].Count {
			return result[i].Name < result[j].Name
		}
		return result[i].Count > result[j].Count
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

// deviceInfoLabel combines the hardware identity a client reported (manufacturer
// plus model, e.g. "Google Pixel 7") with its OS label ("Android (API 34)",
// "iOS 17.5", "macOS (arm64)") into a single dashboard cell. Desktop clients
// report no hardware, so it degrades to just the OS label; a client that
// reported neither yields "" (rendered as "Unknown").
func deviceInfoLabel(manufacturer, model, operatingSystem string) string {
	manufacturer = strings.TrimSpace(manufacturer)
	model = strings.TrimSpace(model)
	var hardware string
	switch {
	case manufacturer != "" && model != "":
		// Avoid "OnePlus OnePlus 9" when the model already carries the brand.
		if strings.HasPrefix(strings.ToLower(model), strings.ToLower(manufacturer)) {
			hardware = model
		} else {
			hardware = manufacturer + " " + model
		}
	case manufacturer != "":
		hardware = manufacturer
	default:
		hardware = model
	}
	parts := make([]string, 0, 2)
	if hardware != "" {
		parts = append(parts, hardware)
	}
	if os := strings.TrimSpace(operatingSystem); os != "" {
		parts = append(parts, os)
	}
	return strings.Join(parts, " · ")
}

func incrementNonEmpty(values map[string]int, name string) {
	if strings.TrimSpace(name) != "" {
		values[name]++
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
