package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type dashboardTelemetryStore struct{ records []TelemetryRecord }

func (s *dashboardTelemetryStore) WriteTelemetry(_ context.Context, records []TelemetryRecord) error {
	s.records = append(s.records, records...)
	return nil
}

func (s *dashboardTelemetryStore) TelemetryRecords(since time.Time) []TelemetryRecord {
	var records []TelemetryRecord
	for _, record := range s.records {
		if !record.Event.OccurredAt.Before(since) {
			records = append(records, record)
		}
	}
	return records
}

func TestApplyRelayLabelsCoversAllRelayViews(t *testing.T) {
	ov := telemetryOverview{
		TopRelays:    []relaySummary{{RelayID: "relay_a"}, {RelayID: "relay_x"}},
		ActiveRelays: []countSummary{{Name: "relay_a", Count: 3}, {Name: "relay_x", Count: 1}},
		SpeedTests:   []speedTestSummary{{RelayID: "relay_a"}},
		Recent:       []sessionSummary{{RelayID: "relay_a"}},
	}
	applyRelayLabels(&ov, map[string]string{"relay_a": "proud-falcon"})

	if ov.TopRelays[0].Label != "proud-falcon" {
		t.Errorf("top_relays label = %q, want proud-falcon", ov.TopRelays[0].Label)
	}
	if ov.ActiveRelays[0].Label != "proud-falcon" {
		t.Errorf("active_by_relay label = %q, want proud-falcon", ov.ActiveRelays[0].Label)
	}
	if ov.ActiveRelays[0].Name != "relay_a" {
		t.Errorf("active_by_relay should keep the id in Name, got %q", ov.ActiveRelays[0].Name)
	}
	if ov.SpeedTests[0].Label != "proud-falcon" {
		t.Errorf("speed_tests label = %q, want proud-falcon", ov.SpeedTests[0].Label)
	}
	if ov.Recent[0].RelayLabel != "proud-falcon" {
		t.Errorf("recent_sessions label = %q, want proud-falcon", ov.Recent[0].RelayLabel)
	}
	if ov.ActiveRelays[1].Label != "" {
		t.Errorf("unmatched relay should stay unlabeled, got %q", ov.ActiveRelays[1].Label)
	}
}

func TestDashboardRoutesDisabledWithoutToken(t *testing.T) {
	server := NewServer(NewStore(), Config{TelemetrySink: &dashboardTelemetryStore{}})
	request := httptest.NewRequest(http.MethodGet, "/admin/telemetry", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", response.Code)
	}
}

func TestDashboardLoginOverviewAndLogout(t *testing.T) {
	store := &dashboardTelemetryStore{}
	server := NewServer(NewStore(), Config{TelemetrySink: store, TelemetryReader: store, DashboardToken: "secret-token"})

	bad := postLogin(server, "wrong")
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("expected invalid login to return 401, got %d", bad.Code)
	}
	login := postLogin(server, "secret-token")
	if login.Code != http.StatusSeeOther {
		t.Fatalf("expected login redirect, got %d: %s", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected login cookie: %+v", cookies)
	}

	dashboardRequest := httptest.NewRequest(http.MethodGet, "/admin/telemetry", nil)
	dashboardRequest.AddCookie(cookies[0])
	dashboardResponse := httptest.NewRecorder()
	server.ServeHTTP(dashboardResponse, dashboardRequest)
	if dashboardResponse.Code != http.StatusOK || !strings.Contains(dashboardResponse.Body.String(), "OPENRUNG / TELEMETRY") {
		t.Fatalf("dashboard failed: %d", dashboardResponse.Code)
	}

	overviewRequest := httptest.NewRequest(http.MethodGet, "/admin/api/telemetry/overview?window=24h", nil)
	overviewRequest.AddCookie(cookies[0])
	overviewResponse := httptest.NewRecorder()
	server.ServeHTTP(overviewResponse, overviewRequest)
	if overviewResponse.Code != http.StatusOK {
		t.Fatalf("overview failed: %d: %s", overviewResponse.Code, overviewResponse.Body.String())
	}

	logoutRequest := httptest.NewRequest(http.MethodPost, "/admin/telemetry/logout", nil)
	logoutRequest.AddCookie(cookies[0])
	logoutResponse := httptest.NewRecorder()
	server.ServeHTTP(logoutResponse, logoutRequest)
	if logoutResponse.Code != http.StatusSeeOther || logoutResponse.Result().Cookies()[0].MaxAge != -1 {
		t.Fatalf("unexpected logout response: %d", logoutResponse.Code)
	}
}

func TestDashboardRejectsExpiredSessionAndInvalidWindow(t *testing.T) {
	store := &dashboardTelemetryStore{}
	dashboard := newDashboardServer("secret", store)
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	dashboard.now = func() time.Time { return now }
	dashboard.sessions["expired"] = now.Add(-time.Second)

	expiredRequest := httptest.NewRequest(http.MethodGet, "/admin/api/telemetry/overview", nil)
	expiredRequest.AddCookie(&http.Cookie{Name: dashboardCookieName, Value: "expired"})
	expiredResponse := httptest.NewRecorder()
	dashboard.requireAuth(dashboard.overview).ServeHTTP(expiredResponse, expiredRequest)
	if expiredResponse.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired session, got %d", expiredResponse.Code)
	}

	dashboard.sessions["valid"] = now.Add(time.Hour)
	invalidRequest := httptest.NewRequest(http.MethodGet, "/admin/api/telemetry/overview?window=30d", nil)
	invalidRequest.AddCookie(&http.Cookie{Name: dashboardCookieName, Value: "valid"})
	invalidResponse := httptest.NewRecorder()
	dashboard.requireAuth(dashboard.overview).ServeHTTP(invalidResponse, invalidRequest)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid window, got %d", invalidResponse.Code)
	}
}

func TestBuildTelemetryOverview(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 30, 0, 0, time.UTC)
	clientOneAttributes := map[string]string{
		"android_api": "37", "country": "US", "city": "Austin",
		"isp": "Google Fiber Inc.", "organization": "Google Fiber Inc", "asn": "AS16591",
	}
	records := []TelemetryRecord{
		dashboardRecord(now.Add(-30*time.Minute), "attempt-1", "connection_attempted", "client-1", "session-1", "", clientOneAttributes, nil),
		dashboardRecord(now.Add(-29*time.Minute), "success-1", "connection_succeeded", "client-1", "session-1", "relay-1", clientOneAttributes, nil),
		dashboardRecord(now.Add(-20*time.Minute), "app-1", "application_connection", "client-1", "session-1", "relay-1", clientOneAttributes, nil),
		dashboardRecord(now.Add(-time.Minute), "heartbeat-1", "session_heartbeat", "client-1", "session-1", "relay-1", clientOneAttributes, map[string]int64{"connected_duration_ms": 60_000, "session_duration_ms": 1_740_000}),
		dashboardRecord(now.Add(-10*time.Minute), "attempt-2", "connection_attempted", "client-2", "session-2", "", map[string]string{"android_api": "35", "country": "CA", "city": "Toronto", "organization": "Fallback Network", "asn": "AS64500"}, nil),
		dashboardRecord(now.Add(-9*time.Minute), "failure-2", "connection_failed", "client-2", "session-2", "", map[string]string{"failure_stage": "broker_fetch", "country": "CA", "city": "Toronto", "organization": "Fallback Network", "asn": "AS64500"}, nil),
		dashboardRecord(now.Add(-5*time.Minute), "speed-1", "speed_test_completed", "client-1", "session-1", "relay-1", nil, map[string]int64{"download_mbps_milli": 42500, "time_to_first_byte_ms": 100}),
	}
	records[2].Event.Application = "com.example.app"
	overview := buildTelemetryOverview(records, now, time.Hour)
	if overview.Totals.Clients != 2 || overview.Totals.Sessions != 2 || overview.Totals.Attempts != 2 || overview.Totals.Successes != 1 || overview.Totals.Failures != 1 {
		t.Fatalf("unexpected totals: %+v", overview.Totals)
	}
	if overview.Totals.SuccessRate != 0.5 {
		t.Fatalf("expected 50%% success rate, got %f", overview.Totals.SuccessRate)
	}
	if overview.Totals.ActiveClients != 1 || overview.Totals.ActiveSessions != 1 {
		t.Fatalf("unexpected active totals: %+v", overview.Totals)
	}
	if len(overview.ActiveCities) != 1 || overview.ActiveCities[0].Name != "Austin" || len(overview.ActiveISPs) != 1 || overview.ActiveISPs[0].Name != "Google Fiber Inc." {
		t.Fatalf("unexpected active breakdowns: cities=%+v isps=%+v", overview.ActiveCities, overview.ActiveISPs)
	}
	if len(overview.TopCountries) != 2 || overview.TopCountries[0].Count != 1 {
		t.Fatalf("countries should count unique sessions: %+v", overview.TopCountries)
	}
	if len(overview.TopCities) != 2 || overview.TopCities[0].Count != 1 {
		t.Fatalf("cities should count unique sessions: %+v", overview.TopCities)
	}
	if len(overview.TopISPs) != 2 || overview.TopISPs[0].Count != 1 {
		t.Fatalf("ISPs should count unique sessions: %+v", overview.TopISPs)
	}
	if len(overview.TopApps) != 1 || overview.TopApps[0].Name != "com.example.app" {
		t.Fatalf("unexpected app ranking: %+v", overview.TopApps)
	}
	if len(overview.SpeedTests) != 1 || overview.SpeedTests[0].AverageMbps != 42.5 {
		t.Fatalf("unexpected speed summary: %+v", overview.SpeedTests)
	}
	var firstSession, secondSession sessionSummary
	for _, session := range overview.Recent {
		switch session.SessionID {
		case "session-1":
			firstSession = session
		case "session-2":
			secondSession = session
		}
	}
	if firstSession.OperatingSystem != "Android (API 37)" || firstSession.City != "Austin" || firstSession.ISP != "Google Fiber Inc." || firstSession.Organization != "Google Fiber Inc" || firstSession.ASN != "AS16591" {
		t.Fatalf("unexpected enriched session: %+v", firstSession)
	}
	if !firstSession.Active || firstSession.LastHeartbeatAt == nil {
		t.Fatalf("expected first session to be active: %+v", firstSession)
	}
	// Active session has no connection_ended yet; duration comes from the heartbeat.
	if firstSession.DurationMS != 1_740_000 {
		t.Fatalf("expected running duration from heartbeat, got %d", firstSession.DurationMS)
	}
	if secondSession.ISP != "Fallback Network" {
		t.Fatalf("expected organization ISP fallback, got %+v", secondSession)
	}
	encoded, err := json.Marshal(overview)
	if err != nil || !strings.Contains(string(encoded), `"recent_sessions"`) {
		t.Fatalf("overview JSON failed: %v %s", err, encoded)
	}
}

func TestBuildTelemetryOverviewExpiresAndTerminatesHeartbeatSessions(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 30, 0, 0, time.UTC)
	expired := dashboardRecord(now.Add(-activeSessionTimeout-time.Second), "expired", "session_heartbeat", "client-1", "session-expired", "relay-1", nil, nil)
	active := dashboardRecord(now.Add(-time.Minute), "active", "session_heartbeat", "client-2", "session-active", "relay-1", nil, nil)
	terminalHeartbeat := dashboardRecord(now.Add(-time.Minute), "terminal-heartbeat", "session_heartbeat", "client-3", "session-terminal", "relay-1", nil, nil)
	terminal := dashboardRecord(now.Add(-30*time.Second), "terminal", "connection_ended", "client-3", "session-terminal", "relay-1", nil, nil)

	overview := buildTelemetryOverview([]TelemetryRecord{expired, active, terminalHeartbeat, terminal}, now, time.Hour)
	if overview.Totals.ActiveSessions != 1 || overview.Totals.ActiveClients != 1 {
		t.Fatalf("expected only one active session: %+v", overview.Totals)
	}
	for _, session := range overview.Recent {
		if session.SessionID != "session-active" && session.Active {
			t.Fatalf("expired or terminal session marked active: %+v", session)
		}
	}
}

func TestBuildTelemetryOverviewHandlesMissingMetadataAndASNFallback(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 30, 0, 0, time.UTC)
	records := []TelemetryRecord{
		dashboardRecord(now.Add(-time.Minute), "missing", "connection_attempted", "client-1", "session-missing", "", nil, nil),
		dashboardRecord(now.Add(-time.Minute), "asn", "connection_attempted", "client-2", "session-asn", "", map[string]string{"asn": "AS64501"}, nil),
	}
	overview := buildTelemetryOverview(records, now, time.Hour)
	if len(overview.TopCities) != 0 || len(overview.TopISPs) != 1 || overview.TopISPs[0].Name != "AS64501" {
		t.Fatalf("unexpected missing metadata aggregation: cities=%+v isps=%+v", overview.TopCities, overview.TopISPs)
	}
	for _, session := range overview.Recent {
		if session.SessionID == "session-missing" && (session.OperatingSystem != "" || session.City != "" || session.ISP != "") {
			t.Fatalf("missing metadata should stay optional: %+v", session)
		}
	}
}

func TestBuildTelemetryOverviewPrefersClientIPOverRelayTransportIP(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 30, 0, 0, time.UTC)
	clientSeen := dashboardRecord(now.Add(-time.Minute), "client-seen", "client_seen", "client-1", "session-1", "", nil, nil)
	clientSeen.SourceIP = "198.51.100.20"
	upload := dashboardRecord(now, "connected", "connection_succeeded", "client-1", "session-1", "relay-1", map[string]string{"client_ip": "198.51.100.20"}, nil)
	upload.SourceIP = "203.0.113.50"

	overview := buildTelemetryOverview([]TelemetryRecord{clientSeen, upload}, now, time.Hour)
	if got := overview.Recent[0].SourceIP; got != "198.51.100.20" {
		t.Fatalf("expected pre-tunnel client IP, got %q", got)
	}
}

func TestBuildTelemetryOverviewSourceIPFallbacks(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 30, 0, 0, time.UTC)
	reported := dashboardRecord(now, "reported", "connection_succeeded", "client-1", "session-reported", "relay-1", map[string]string{"client_ip": "198.51.100.21"}, nil)
	reported.SourceIP = "203.0.113.50"
	transport := dashboardRecord(now, "transport", "connection_succeeded", "client-2", "session-transport", "relay-1", nil, nil)
	transport.SourceIP = "198.51.100.22"

	overview := buildTelemetryOverview([]TelemetryRecord{reported, transport}, now, time.Hour)
	got := make(map[string]string)
	for _, session := range overview.Recent {
		got[session.SessionID] = session.SourceIP
	}
	if got["session-reported"] != "198.51.100.21" || got["session-transport"] != "198.51.100.22" {
		t.Fatalf("unexpected source IP fallbacks: %+v", got)
	}
}

func postLogin(server http.Handler, token string) *httptest.ResponseRecorder {
	form := url.Values{"token": {token}}
	request := httptest.NewRequest(http.MethodPost, "/admin/telemetry/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func dashboardRecord(at time.Time, eventID, event, clientID, sessionID, relayID string, attributes map[string]string, measurements map[string]int64) TelemetryRecord {
	return TelemetryRecord{ReceivedAt: at, SourceIP: "203.0.113.20", Event: TelemetryEvent{
		SchemaVersion: 1, EventID: eventID, Event: event, OccurredAt: at,
		ClientID: clientID, SessionID: sessionID, RelayID: relayID,
		Attributes: attributes, Measurements: measurements,
	}}
}
