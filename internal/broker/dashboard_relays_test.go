package broker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openrung/internal/relay"
)

func relayStatRecord(at time.Time, nodeClass, eventID, event, clientID, sessionID, relayID string, attributes map[string]string, measurements map[string]int64) TelemetryRecord {
	record := dashboardRecord(at, eventID, event, clientID, sessionID, relayID, attributes, measurements)
	record.RelayNodeClass = nodeClass
	return record
}

func TestBuildRelayTelemetryStats(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	records := []TelemetryRecord{
		// Outside the 1h window: must not count toward relay-a's successes.
		relayStatRecord(now.Add(-2*time.Hour), relay.NodeClassFoundation, "old", "connection_succeeded", "c0", "s0", "relay-a", nil, nil),
		// relay-a: one success, an active heartbeating session, and a
		// speed-test session (attributed to relay-a by its latest event).
		relayStatRecord(now.Add(-10*time.Minute), relay.NodeClassFoundation, "e1", "connection_succeeded", "c1", "s1", "relay-a", nil, nil),
		relayStatRecord(now.Add(-time.Minute), relay.NodeClassFoundation, "e2", "session_heartbeat", "c1", "s1", "relay-a", nil, nil),
		relayStatRecord(now.Add(-4*time.Minute), relay.NodeClassFoundation, "e3", "speed_test_completed", "c4", "s4", "relay-a", nil, map[string]int64{"download_mbps_milli": 45000, "time_to_first_byte_ms": 120}),
		relayStatRecord(now.Add(-3*time.Minute), relay.NodeClassFoundation, "e4", "speed_test_completed", "c4", "s4", "relay-a", nil, map[string]int64{"download_mbps_milli": 55000, "time_to_first_byte_ms": 80}),
		// s2 attempts relay-a but fails over to relay-b: the session counts for
		// relay-b (latest relay-bearing event) and the failover is relay-b's
		// success.
		relayStatRecord(now.Add(-9*time.Minute), relay.NodeClassFoundation, "e5", "connection_attempted", "c2", "s2", "relay-a", nil, nil),
		relayStatRecord(now.Add(-8*time.Minute), "", "e6", "relay_failover", "c2", "s2", "relay-b", nil, nil),
		// relay-b failures with a modal reason: alpha twice (once via
		// failure_reason, once via error_type), beta once.
		relayStatRecord(now.Add(-7*time.Minute), "", "e7", "relay_attempt_failed", "c3", "s3", "relay-b", map[string]string{"failure_reason": "alpha", "error_type": "zzz"}, nil),
		relayStatRecord(now.Add(-6*time.Minute), "", "e8", "relay_attempt_failed", "c3", "s3", "relay-b", map[string]string{"error_type": "beta"}, nil),
		relayStatRecord(now.Add(-5*time.Minute), "", "e9", "relay_attempt_failed", "c3", "s3", "relay-b", map[string]string{"error_type": "alpha"}, nil),
		// relay-c: nothing but an active heartbeat, class retained from the
		// record.
		relayStatRecord(now.Add(-2*time.Minute), relay.NodeClassVolunteer, "e10", "session_heartbeat", "c5", "s5", "relay-c", nil, nil),
		// application_connection is never aggregated, even when it names a relay.
		relayStatRecord(now.Add(-time.Minute), relay.NodeClassFoundation, "e11", telemetryAppConnectionEvent, "c9", "s9", "relay-a", nil, nil),
	}

	stats := buildRelayTelemetryStats(records, now, time.Hour)

	if len(stats.Relays) != 3 {
		t.Fatalf("expected 3 relays, got %d: %+v", len(stats.Relays), stats.Relays)
	}
	// Busiest first: relay-a (1 active, 2 sessions), relay-c (1 active,
	// 1 session), relay-b (0 active).
	if stats.Relays[0].RelayID != "relay-a" || stats.Relays[1].RelayID != "relay-c" || stats.Relays[2].RelayID != "relay-b" {
		t.Fatalf("unexpected order: %q, %q, %q", stats.Relays[0].RelayID, stats.Relays[1].RelayID, stats.Relays[2].RelayID)
	}

	relayA := stats.Relays[0]
	if relayA.Successes != 1 || relayA.Failures != 0 {
		t.Errorf("relay-a successes/failures = %d/%d, want 1/0 (the 2h-old success is outside the window)", relayA.Successes, relayA.Failures)
	}
	if relayA.Sessions != 2 || relayA.Clients != 2 {
		t.Errorf("relay-a sessions/clients = %d/%d, want 2/2", relayA.Sessions, relayA.Clients)
	}
	if relayA.ActiveSessions != 1 || relayA.ActiveClients != 1 {
		t.Errorf("relay-a active sessions/clients = %d/%d, want 1/1", relayA.ActiveSessions, relayA.ActiveClients)
	}
	if relayA.SpeedTests != 2 || relayA.AverageMbps != 50 || relayA.AverageTTFBMS != 100 {
		t.Errorf("relay-a speed = %d tests %.1f Mbps %.0f ms, want 2/50.0/100", relayA.SpeedTests, relayA.AverageMbps, relayA.AverageTTFBMS)
	}
	if relayA.NodeClass != relay.NodeClassFoundation {
		t.Errorf("relay-a node class = %q, want foundation", relayA.NodeClass)
	}
	if !relayA.LastSeenAt.Equal(now.Add(-time.Minute)) {
		t.Errorf("relay-a last seen = %s, want the heartbeat a minute ago", relayA.LastSeenAt)
	}

	relayB := stats.Relays[2]
	if relayB.Successes != 1 {
		t.Errorf("relay-b successes = %d, want 1 (the failover)", relayB.Successes)
	}
	if relayB.Failures != 3 || relayB.TopFailureReason != "alpha" {
		t.Errorf("relay-b failures/top reason = %d/%q, want 3/alpha", relayB.Failures, relayB.TopFailureReason)
	}
	if relayB.Sessions != 2 {
		t.Errorf("relay-b sessions = %d, want 2 (s2 followed its failover here, s3 failed here)", relayB.Sessions)
	}

	relayC := stats.Relays[1]
	if relayC.NodeClass != relay.NodeClassVolunteer {
		t.Errorf("relay-c node class = %q, want retained volunteer", relayC.NodeClass)
	}
	if relayC.ActiveSessions != 1 || relayC.Successes != 0 {
		t.Errorf("relay-c active/successes = %d/%d, want 1/0", relayC.ActiveSessions, relayC.Successes)
	}

	// c1 (active on relay-a) and c5 (active on relay-c), deduplicated.
	if stats.ActiveClients != 2 {
		t.Errorf("overall active clients = %d, want 2", stats.ActiveClients)
	}
}

func TestBuildRelaysPanelMergesRegistryAndTelemetry(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	descriptors := []relay.Descriptor{
		{
			ID: "relay-online", Label: "tokyo-1", NodeClass: relay.NodeClassFoundation,
			PublicHost: "203.0.113.5", PublicPort: 443,
			GeoLocation:  relay.GeoLocation{City: "Tokyo", Country: "Japan", CountryCode: "JP"},
			Transport:    relay.TransportDirect,
			RelayVersion: "0.1.4", MaxSessions: 100, MaxMbps: 500, PunchCapable: true,
			WSSFronts:    []relay.WSSFrontDescriptor{{ID: "front-1", URL: "https://front.example"}},
			RegisteredAt: now.Add(-time.Hour), LastHeartbeatAt: now.Add(-time.Minute),
		},
		{
			ID: "relay-quiet", NodeClass: relay.NodeClassVolunteer,
			PublicHost: "2001:db8::7", PublicPort: 8443,
			Transport: relay.TransportTunnel, MaxSessions: 40,
		},
	}
	stats := relayTelemetryStats{
		ActiveClients: 4,
		Relays: []relayStatRow{
			// The retained volunteer class must not override the online row's
			// broker-attested foundation class.
			{RelayID: "relay-online", NodeClass: relay.NodeClassVolunteer, Successes: 9, Failures: 1, Sessions: 5, Clients: 4, ActiveSessions: 3, ActiveClients: 3, LastSeenAt: now.Add(-2 * time.Minute)},
			{RelayID: "relay-gone", NodeClass: relay.NodeClassVolunteer, Failures: 7, TopFailureReason: "timeout", Sessions: 2, Clients: 2, ActiveSessions: 1, ActiveClients: 1, LastSeenAt: now.Add(-30 * time.Minute)},
		},
	}

	panel := buildRelaysPanel(descriptors, stats, now, 24*time.Hour)

	totals := panel.Totals
	if totals.OnlineRelays != 2 || totals.FoundationRelays != 1 || totals.VolunteerRelays != 1 || totals.OfflineRelays != 1 {
		t.Fatalf("relay counts = %+v, want 2 online (1 foundation, 1 volunteer), 1 offline", totals)
	}
	if totals.SessionCapacity != 140 || totals.DirectRelays != 1 || totals.TunnelRelays != 1 || totals.PunchCapableRelays != 1 || totals.WSSCapableRelays != 1 {
		t.Errorf("capacity/transport totals = %+v", totals)
	}
	// 3 active on the online relay + 1 still active on the vanished relay.
	if totals.ActiveSessions != 4 {
		t.Errorf("active sessions = %d, want 4", totals.ActiveSessions)
	}
	if totals.ConnectedClients != 4 {
		t.Errorf("connected clients = %d, want the querier's distinct count", totals.ConnectedClients)
	}

	if len(panel.Relays) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(panel.Relays))
	}
	// Online rows first (busiest leading), the offline row last.
	if panel.Relays[0].RelayID != "relay-online" || panel.Relays[1].RelayID != "relay-quiet" || panel.Relays[2].RelayID != "relay-gone" {
		t.Fatalf("unexpected order: %q, %q, %q", panel.Relays[0].RelayID, panel.Relays[1].RelayID, panel.Relays[2].RelayID)
	}

	online := panel.Relays[0]
	if !online.Online || online.Label != "tokyo-1" || online.NodeClass != relay.NodeClassFoundation {
		t.Errorf("online row basics wrong: %+v", online)
	}
	if online.Endpoint != "203.0.113.5:443" || online.City != "Tokyo" || online.CountryCode != "JP" || online.RelayVersion != "0.1.4" {
		t.Errorf("online row registry fields wrong: %+v", online)
	}
	if online.Successes != 9 || online.ActiveSessions != 3 || online.Clients != 4 {
		t.Errorf("online row telemetry fields wrong: %+v", online)
	}
	if online.RegisteredAt == nil || online.LastHeartbeatAt == nil {
		t.Errorf("online row should carry registry timestamps: %+v", online)
	}

	quiet := panel.Relays[1]
	if quiet.Endpoint != "[2001:db8::7]:8443" {
		t.Errorf("IPv6 endpoint = %q, want bracketed host:port", quiet.Endpoint)
	}
	if quiet.Sessions != 0 || quiet.Successes != 0 {
		t.Errorf("relay without telemetry should have zero counters: %+v", quiet)
	}

	gone := panel.Relays[2]
	if gone.Online {
		t.Errorf("relay-gone should be offline")
	}
	if gone.NodeClass != relay.NodeClassVolunteer {
		t.Errorf("offline row should keep the retained class, got %q", gone.NodeClass)
	}
	if gone.Failures != 7 || gone.TopFailureReason != "timeout" || gone.LastSeenAt == nil {
		t.Errorf("offline row telemetry fields wrong: %+v", gone)
	}
	if gone.Endpoint != "" || gone.RegisteredAt != nil || gone.LastHeartbeatAt != nil {
		t.Errorf("offline row must not invent registry fields: %+v", gone)
	}
}

func TestDashboardRelaysEndpointAndPage(t *testing.T) {
	now := time.Now().UTC()
	store := NewStore()
	registered, err := store.Register(relay.RegisterRequest{
		Label: "tokyo-1", NodeClass: relay.NodeClassFoundation,
		PublicHost: "203.0.113.5", PublicPort: 443,
		MaxSessions: 100, MaxMbps: 500, RelayVersion: "0.1.4", PunchCapable: true,
	}, now, time.Hour)
	if err != nil {
		t.Fatalf("register relay: %v", err)
	}
	if err := store.UpdateGeo(registered.ID, registered.LeaseToken, relay.GeoLocation{City: "Tokyo", Country: "Japan", CountryCode: "JP"}); err != nil {
		t.Fatalf("update geo: %v", err)
	}

	telemetry := &dashboardTelemetryStore{}
	if err := telemetry.WriteTelemetry(t.Context(), []TelemetryRecord{
		relayStatRecord(now.Add(-10*time.Minute), relay.NodeClassFoundation, "e1", "connection_succeeded", "c1", "s1", registered.ID, nil, nil),
		relayStatRecord(now.Add(-30*time.Second), relay.NodeClassFoundation, "e2", "session_heartbeat", "c1", "s1", registered.ID, nil, nil),
		relayStatRecord(now.Add(-15*time.Minute), relay.NodeClassVolunteer, "e3", "relay_attempt_failed", "c2", "s2", "relay_offline", map[string]string{"error_type": "timeout"}, nil),
	}); err != nil {
		t.Fatalf("write telemetry: %v", err)
	}

	server := NewServer(store, Config{SigningSeed: testSigningSeed(), TelemetrySink: telemetry, TelemetryReader: telemetry, DashboardToken: "secret-token"})

	// Unauthenticated: the API answers 401, the page redirects to login.
	apiRequest := httptest.NewRequest(http.MethodGet, "/admin/api/telemetry/relays", nil)
	apiResponse := httptest.NewRecorder()
	server.ServeHTTP(apiResponse, apiRequest)
	if apiResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated API = %d, want 401", apiResponse.Code)
	}
	pageRequest := httptest.NewRequest(http.MethodGet, "/admin/telemetry/relays", nil)
	pageResponse := httptest.NewRecorder()
	server.ServeHTTP(pageResponse, pageRequest)
	if pageResponse.Code != http.StatusSeeOther || pageResponse.Header().Get("Location") != "/admin/telemetry/login" {
		t.Fatalf("unauthenticated page = %d → %q, want redirect to login", pageResponse.Code, pageResponse.Header().Get("Location"))
	}

	login := postLogin(server, "secret-token")
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login failed: %d", login.Code)
	}
	cookie := login.Result().Cookies()[0]

	get := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}

	badWindow := get("/admin/api/telemetry/relays?window=2h")
	if badWindow.Code != http.StatusBadRequest {
		t.Fatalf("invalid window = %d, want 400", badWindow.Code)
	}

	response := get("/admin/api/telemetry/relays?window=24h")
	if response.Code != http.StatusOK {
		t.Fatalf("relays API failed: %d: %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	var panel relaysPanelResponse
	if err := json.Unmarshal(response.Body.Bytes(), &panel); err != nil {
		t.Fatalf("decode relays response: %v", err)
	}
	if panel.Totals.OnlineRelays != 1 || panel.Totals.FoundationRelays != 1 || panel.Totals.OfflineRelays != 1 {
		t.Fatalf("totals = %+v, want 1 online foundation and 1 offline", panel.Totals)
	}
	if panel.Totals.ActiveSessions != 1 || panel.Totals.ConnectedClients != 1 {
		t.Errorf("active sessions/connected clients = %d/%d, want 1/1", panel.Totals.ActiveSessions, panel.Totals.ConnectedClients)
	}
	if len(panel.Relays) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(panel.Relays))
	}
	online := panel.Relays[0]
	if online.RelayID != registered.ID || !online.Online || online.Label != "tokyo-1" || online.City != "Tokyo" || online.RelayVersion != "0.1.4" {
		t.Errorf("online row wrong: %+v", online)
	}
	if online.Successes != 1 || online.ActiveSessions != 1 || online.ActiveClients != 1 {
		t.Errorf("online row telemetry wrong: %+v", online)
	}
	offline := panel.Relays[1]
	if offline.RelayID != "relay_offline" || offline.Online || offline.NodeClass != relay.NodeClassVolunteer || offline.Failures != 1 || offline.TopFailureReason != "timeout" {
		t.Errorf("offline row wrong: %+v", offline)
	}

	// The page itself renders behind the same session, with the shared CSP.
	page := get("/admin/telemetry/relays")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "OPENRUNG / RELAYS") {
		t.Fatalf("relays page failed: %d", page.Code)
	}
	if page.Header().Get("Content-Security-Policy") == "" {
		t.Errorf("relays page missing CSP header")
	}
}

// Both dashboard pages must link to each other so the relays page is
// discoverable and escapable without editing the URL.
func TestDashboardPagesCrossLink(t *testing.T) {
	if !strings.Contains(string(dashboardHTML), `href="/admin/telemetry/relays"`) {
		t.Errorf("overview page has no link to the relays page")
	}
	if !strings.Contains(string(relaysHTML), `href="/admin/telemetry"`) {
		t.Errorf("relays page has no link back to the overview")
	}
}
