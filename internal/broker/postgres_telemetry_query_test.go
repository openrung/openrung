package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// The parity tests feed one synthetic event set through the in-memory
// aggregator (the JSONL sink's dashboard path) and the SQL aggregation, then
// require byte-identical JSON. Only the two orderings that were already
// nondeterministic in the in-memory path (top-relay and speed-test ties) are
// kept tie-free in the fixture.

// parityTelemetryRecords exercises the semantics the SQL path must reproduce:
// latest-non-empty attribute precedence (including the android_api/ios_version
// OS fallbacks and country_code), status transitions, cumulative byte maxima,
// running-vs-final duration, source-IP preference, window boundaries (events
// before the window, after "now", and received before the window started),
// speed tests, and multi-client sessions.
func parityTelemetryRecords(now time.Time) []TelemetryRecord {
	sequence := 0
	record := func(occurredMinutesAgo, receivedMinutesAgo int, event, clientID, sessionID, relayID, sourceIP string, attributes map[string]string, measurements map[string]int64) TelemetryRecord {
		sequence++
		return TelemetryRecord{
			ReceivedAt: now.Add(-time.Duration(receivedMinutesAgo) * time.Minute),
			SourceIP:   sourceIP,
			Event: TelemetryEvent{
				SchemaVersion: 1,
				EventID:       fmt.Sprintf("parity-%03d", sequence),
				Event:         event,
				OccurredAt:    now.Add(-time.Duration(occurredMinutesAgo) * time.Minute),
				ClientID:      clientID,
				SessionID:     sessionID,
				RelayID:       relayID,
				Application:   attributes["_app"],
				Attributes:    withoutKey(attributes, "_app"),
				Measurements:  measurements,
			},
		}
	}

	return []TelemetryRecord{
		// session-android: succeeded then still heartbeating (active). In the
		// 1h window only the heartbeat survives, so its status degrades to
		// "seen" with no attributes — a window-boundary case in itself.
		record(85, 85, "client_seen", "client-android", "session-android", "", "203.0.113.10",
			map[string]string{"android_api": "34", "device_manufacturer": "Google", "device_model": "Pixel 7", "app_version": "1.2.0", "country": "DE", "city": "Berlin", "isp": "Deutsche Telekom"}, nil),
		record(84, 84, "connection_attempted", "client-android", "session-android", "relay-1", "203.0.113.10",
			map[string]string{"client_ip": "10.1.2.3"}, nil),
		record(83, 83, "connection_succeeded", "client-android", "session-android", "relay-1", "203.0.113.10",
			map[string]string{"_app": "org.mozilla.firefox"}, map[string]int64{"bytes_sent": 100, "bytes_received": 200}),
		record(1, 1, "session_heartbeat", "client-android", "session-android", "relay-1", "203.0.113.10",
			nil, map[string]int64{"session_duration_ms": 500000, "bytes_sent": 5000, "bytes_received": 9000}),

		// session-ios: attempted then failed (terminal), OS from ios_version,
		// country from country_code, a relay failure for top-relay counts.
		record(80, 80, "connection_attempted", "client-ios", "session-ios", "", "198.51.100.20",
			map[string]string{"ios_version": "17.5", "app_version": "2.0.1", "country_code": "IR", "city": "Tehran", "_app": "org.mozilla.firefox"}, nil),
		record(79, 79, "relay_attempt_failed", "client-ios", "session-ios", "relay-2", "198.51.100.20",
			nil, map[string]int64{"relay_tcp_ms": 300}),
		record(78, 78, "connection_failed", "client-ios", "session-ios", "", "198.51.100.20",
			map[string]string{"failure_stage": "tcp_connect"}, nil),

		// session-desktop: full lifecycle. The heartbeat reports larger byte
		// counters than the final connection_ended (maxima must win), while
		// connection_ended's duration overrides the running one. ISP falls
		// back to organization.
		record(30, 30, "client_seen", "client-desktop", "session-desktop", "", "198.51.100.7",
			map[string]string{"operating_system": "macOS (arm64)", "organization": "Example Org", "asn": "AS64500", "app_version": "0.9.0"}, nil),
		record(29, 29, "connection_attempted", "client-desktop", "session-desktop", "relay-2", "198.51.100.7", nil, nil),
		record(28, 28, "connection_succeeded", "client-desktop", "session-desktop", "relay-2", "198.51.100.7",
			map[string]string{"_app": "com.brave.browser"}, map[string]int64{"bytes_sent": 700}),
		record(25, 25, "session_heartbeat", "client-desktop", "session-desktop", "relay-2", "198.51.100.7",
			nil, map[string]int64{"session_duration_ms": 60000, "bytes_sent": 900, "bytes_received": 1500}),
		record(20, 20, "connection_ended", "client-desktop", "session-desktop", "relay-2", "198.51.100.7",
			nil, map[string]int64{"session_duration_ms": 540000, "bytes_sent": 800, "bytes_received": 1200}),

		// session-precedence: latest non-empty attribute wins (city updated,
		// country kept, then country_code), reported client_ip beats the
		// transport source because no client_seen carries a source, and the
		// session spans two client IDs (the first one names the session).
		record(70, 70, "connection_attempted", "client-p1", "session-precedence", "", "192.0.2.50",
			map[string]string{"operating_system": "Windows 11", "country": "US", "city": "Austin", "isp": "AT&T", "client_ip": "10.9.8.7"}, nil),
		record(60, 60, "connection_attempted", "client-p1", "session-precedence", "", "192.0.2.50",
			map[string]string{"city": "Dallas"}, nil),
		record(50, 50, "connection_attempted", "client-p2", "session-precedence", "", "192.0.2.51",
			map[string]string{"country_code": "CA"}, nil),

		// session-boundary: attempted long ago (outside the 1h window), then a
		// stale heartbeat — attempting-but-inactive in 24h, bare "seen" in 1h.
		record(200, 200, "connection_attempted", "client-boundary", "session-boundary", "relay-1", "203.0.113.99", nil, nil),
		record(55, 55, "session_heartbeat", "client-boundary", "session-boundary", "relay-1", "203.0.113.99",
			nil, map[string]int64{"session_duration_ms": 120000}),
		// Occurred after "now" (allowed by ingest skew): excluded everywhere.
		// If either path counted it, session-boundary would turn failed.
		record(-10, 5, "connection_failed", "client-boundary", "session-boundary", "", "203.0.113.99",
			map[string]string{"failure_stage": "phantom"}, nil),

		// session-skew: received before the 1h window opened but occurred
		// inside it — the SQL received_at pruning margin must keep it.
		record(50, 65, "client_seen", "client-skew", "session-skew", "", "192.0.2.99",
			map[string]string{"operating_system": "Linux"}, nil),

		// session-speed: speed tests with tie-free averages per relay.
		record(40, 40, "speed_test_completed", "client-speed", "session-speed", "relay-1", "192.0.2.60",
			nil, map[string]int64{"download_mbps_milli": 45000, "time_to_first_byte_ms": 120}),
		record(39, 39, "speed_test_completed", "client-speed", "session-speed", "relay-3", "192.0.2.60",
			nil, map[string]int64{"download_mbps_milli": 90000, "time_to_first_byte_ms": 80}),
		record(38, 38, "speed_test_completed", "client-speed", "session-speed", "relay-3", "192.0.2.60",
			nil, map[string]int64{"download_mbps_milli": 70000, "time_to_first_byte_ms": 100}),
	}
}

func withoutKey(values map[string]string, key string) map[string]string {
	if _, ok := values[key]; !ok {
		return values
	}
	trimmed := make(map[string]string, len(values))
	for name, value := range values {
		if name != key {
			trimmed[name] = value
		}
	}
	if len(trimmed) == 0 {
		return nil
	}
	return trimmed
}

func TestPostgresTelemetryQuerierMatchesInMemoryOverview(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 30, 0, 0, time.UTC)
	sink := newTestPostgresTelemetrySink(t, now)
	records := parityTelemetryRecords(now)
	if err := sink.WriteTelemetry(context.Background(), records); err != nil {
		t.Fatalf("write telemetry: %v", err)
	}
	memory := newTelemetryReaderQuerier(&dashboardTelemetryStore{records: records})

	for _, window := range []time.Duration{time.Hour, 24 * time.Hour, telemetryRetention} {
		want, err := memory.TelemetryOverview(now, window)
		if err != nil {
			t.Fatalf("in-memory overview (%s): %v", window, err)
		}
		got, err := sink.TelemetryOverview(now, window)
		if err != nil {
			t.Fatalf("postgres overview (%s): %v", window, err)
		}
		assertSameJSON(t, fmt.Sprintf("overview window=%s", window), want, got)
	}
}

func TestPostgresTelemetryQuerierMatchesInMemorySessions(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 30, 0, 0, time.UTC)
	sink := newTestPostgresTelemetrySink(t, now)
	records := parityTelemetryRecords(now)
	if err := sink.WriteTelemetry(context.Background(), records); err != nil {
		t.Fatalf("write telemetry: %v", err)
	}
	memory := newTelemetryReaderQuerier(&dashboardTelemetryStore{records: records})

	window := 24 * time.Hour
	for _, page := range []struct{ offset, limit int }{
		{0, 3},    // first page
		{3, 3},    // middle page
		{0, 100},  // everything
		{6, 2},    // tail page
		{100, 10}, // offset beyond the window's sessions
	} {
		wantSessions, wantTotal, err := memory.TelemetrySessions(now, window, page.offset, page.limit)
		if err != nil {
			t.Fatalf("in-memory sessions (offset=%d): %v", page.offset, err)
		}
		gotSessions, gotTotal, err := sink.TelemetrySessions(now, window, page.offset, page.limit)
		if err != nil {
			t.Fatalf("postgres sessions (offset=%d): %v", page.offset, err)
		}
		if wantTotal != gotTotal {
			t.Fatalf("sessions total mismatch at offset=%d: in-memory %d, postgres %d", page.offset, wantTotal, gotTotal)
		}
		assertSameJSON(t, fmt.Sprintf("sessions offset=%d limit=%d", page.offset, page.limit), wantSessions, gotSessions)
	}
}

func TestPostgresTelemetryQuerierMatchesInMemoryWhenEmpty(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 30, 0, 0, time.UTC)
	sink := newTestPostgresTelemetrySink(t, now)
	memory := newTelemetryReaderQuerier(&dashboardTelemetryStore{})

	want, err := memory.TelemetryOverview(now, time.Hour)
	if err != nil {
		t.Fatalf("in-memory empty overview: %v", err)
	}
	got, err := sink.TelemetryOverview(now, time.Hour)
	if err != nil {
		t.Fatalf("postgres empty overview: %v", err)
	}
	assertSameJSON(t, "empty overview", want, got)

	wantSessions, wantTotal, err := memory.TelemetrySessions(now, time.Hour, 0, 25)
	if err != nil {
		t.Fatalf("in-memory empty sessions: %v", err)
	}
	gotSessions, gotTotal, err := sink.TelemetrySessions(now, time.Hour, 0, 25)
	if err != nil {
		t.Fatalf("postgres empty sessions: %v", err)
	}
	if wantTotal != 0 || gotTotal != 0 {
		t.Fatalf("expected zero totals, got in-memory %d, postgres %d", wantTotal, gotTotal)
	}
	assertSameJSON(t, "empty sessions", wantSessions, gotSessions)
}

func assertSameJSON(t *testing.T, context string, want, got any) {
	t.Helper()
	wantJSON, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatalf("marshal in-memory %s: %v", context, err)
	}
	gotJSON, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal postgres %s: %v", context, err)
	}
	if string(wantJSON) != string(gotJSON) {
		t.Fatalf("%s diverges between backends:\n--- in-memory ---\n%s\n--- postgres ---\n%s", context, wantJSON, gotJSON)
	}
}
