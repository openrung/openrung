package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	"openrung/internal/relay"
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
	relayClasses := map[string]string{
		"relay-1": relay.NodeClassFoundation,
		"relay-2": relay.NodeClassVolunteer,
		"relay-3": relay.NodeClassFoundation,
		"relay-4": relay.NodeClassFoundation,
	}
	record := func(occurredMinutesAgo, receivedMinutesAgo int, event, clientID, sessionID, relayID, sourceIP string, attributes map[string]string, measurements map[string]int64) TelemetryRecord {
		sequence++
		return TelemetryRecord{
			ReceivedAt:     now.Add(-time.Duration(receivedMinutesAgo) * time.Minute),
			SourceIP:       sourceIP,
			RelayNodeClass: relayClasses[relayID],
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

	records := []TelemetryRecord{
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
		// error_type stands in for the not-yet-sent failure_reason, and a
		// failure_detail rides along — both surface on the session and the
		// failure_reasons panel keys on "tcp_connect · connection_refused".
		record(78, 78, "connection_failed", "client-ios", "session-ios", "", "198.51.100.20",
			map[string]string{"failure_stage": "tcp_connect", "error_type": "connection_refused", "failure_detail": "ECONNREFUSED"}, nil),

		// session-desktop: full lifecycle plus a measured relay failover. The
		// failover credits relay-4 without adding another connection-trend success.
		// The heartbeat reports larger byte counters than the final
		// connection_ended (maxima must win), while connection_ended's duration
		// overrides the running one. ISP falls back to organization.
		record(30, 30, "client_seen", "client-desktop", "session-desktop", "", "198.51.100.7",
			map[string]string{"operating_system": "macOS (arm64)", "organization": "Example Org", "asn": "AS64500", "app_version": "0.9.0"}, nil),
		record(29, 29, "connection_attempted", "client-desktop", "session-desktop", "relay-2", "198.51.100.7", nil, nil),
		record(28, 28, "connection_succeeded", "client-desktop", "session-desktop", "relay-2", "198.51.100.7",
			map[string]string{"_app": "com.brave.browser"}, map[string]int64{"bytes_sent": 700}),
		record(27, 27, "relay_failover", "client-desktop", "session-desktop", "relay-4", "198.51.100.7",
			map[string]string{"from_relay_id": "relay-2"}, map[string]int64{"relay_tcp_ms": 75, "internet_probe_ms": 140}),
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

		// session-failwin: two connection_failed events. The first carries a
		// classified failure_reason (preferred over its error_type) plus a
		// stage and detail; the later one presents an empty failure_stage and
		// no reason, so per field the earlier non-empty value must survive
		// (latest-non-empty-wins). The empty one still keys the panel as
		// "unknown · unknown".
		record(45, 45, "connection_failed", "client-failwin", "session-failwin", "", "192.0.2.70",
			map[string]string{"failure_stage": "tls_handshake", "failure_reason": "cert_expired", "error_type": "tls_error", "failure_detail": "certificate expired"}, nil),
		record(44, 44, "connection_failed", "client-failwin", "session-failwin", "", "192.0.2.70",
			map[string]string{"failure_stage": ""}, nil),

		// relay-ghost is telemetry-only and never carries a broker-attested
		// class — the anonymous API accepts any relay_id string. It still
		// feeds the overview's relay rankings (existing semantics), but the
		// relays page must drop its row and keep its active client out of the
		// connected-clients sentinel, identically on both backends.
		record(20, 20, "connection_succeeded", "client-ghost", "session-ghost", "relay-ghost", "192.0.2.80", nil, nil),
		record(19, 19, "speed_test_completed", "client-ghost", "session-ghost", "relay-ghost", "192.0.2.80",
			nil, map[string]int64{"download_mbps_milli": 99000, "time_to_first_byte_ms": 10}),
		record(2, 2, "session_heartbeat", "client-ghost", "session-ghost", "relay-ghost", "192.0.2.80",
			nil, map[string]int64{"session_duration_ms": 60000}),

		// relay-current is registered right now, but none of its window events
		// carry a stamp — they all arrived during a lease gap. The relay-stats
		// parity test passes it in the registered set, so both backends must
		// keep its row (class empty) and count its active client.
		record(6, 6, "connection_succeeded", "client-current", "session-current", "relay-current", "192.0.2.81", nil, nil),
		record(1, 1, "session_heartbeat", "client-current", "session-current", "relay-current", "192.0.2.81",
			nil, map[string]int64{"session_duration_ms": 30000}),

		// session-relayfail: three relay_attempt_failed against relay-4 with
		// distinct reasons (the first prefers failure_reason over error_type,
		// the rest fall back to error_type). All tie at one, so relay-4's
		// TopFailureReason resolves to the lexicographically smallest, "alpha".
		record(35, 35, "relay_attempt_failed", "client-relayfail", "session-relayfail", "relay-4", "192.0.2.71",
			map[string]string{"failure_reason": "alpha", "error_type": "zzz"}, nil),
		record(34, 34, "relay_attempt_failed", "client-relayfail", "session-relayfail", "relay-4", "192.0.2.71",
			map[string]string{"error_type": "beta"}, nil),
		record(33, 33, "relay_attempt_failed", "client-relayfail", "session-relayfail", "relay-4", "192.0.2.71",
			map[string]string{"error_type": "gamma"}, nil),

		// application_connection events: stored by neither backend — both fold
		// them into the hourly top-apps rollup keyed by receipt hour and drop
		// the record. The first is a legacy one-flow event; the second represents
		// 41 client-aggregated flows. session-apponly/client-apponly exist only
		// here, so they must appear in no total or panel besides
		// top_applications. With
		// now=12:30, the two events received 3–4 minutes ago bucket to 12:00
		// and the one received 70 minutes ago buckets to 11:00 — inside the 1h
		// window's truncated start hour even though it was received before the
		// window opened, pinning the hour-granular window edge in both paths.
		record(3, 3, "application_connection", "client-apponly", "session-apponly", "relay-1", "203.0.113.10",
			map[string]string{"_app": "com.instagram.android", "country": "IR", "client_ip": "203.0.113.77"}, nil),
		record(4, 4, "application_connection", "client-apponly", "session-apponly", "relay-1", "203.0.113.10",
			map[string]string{"_app": "com.instagram.android"}, map[string]int64{telemetryAppConnectionCountMeasurement: 41}),
		record(70, 70, "application_connection", "client-apponly", "session-apponly", "", "192.0.2.50",
			map[string]string{"_app": "org.telegram.messenger"}, nil),
	}
	// The desktop heartbeat loses its class stamp, as if it was received while
	// relay-2's registration had briefly lapsed. Relay-2 stays attested through
	// its neighbouring stamped records — including the later connection_ended,
	// which keeps the session's latest relay-bearing event stamped — and the
	// unstamped event must still count toward relay-2's aggregates.
	for i := range records {
		if records[i].Event.Event == "session_heartbeat" && records[i].Event.SessionID == "session-desktop" {
			records[i].RelayNodeClass = ""
		}
	}
	return records
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
	memoryStore := &dashboardTelemetryStore{}
	if err := memoryStore.WriteTelemetry(context.Background(), records); err != nil {
		t.Fatalf("write telemetry to in-memory store: %v", err)
	}
	memory := newTelemetryReaderQuerier(memoryStore)
	rollupReference := newTelemetryReaderQuerier(rollupReferenceStore(t, records, now))

	for _, window := range []time.Duration{time.Hour, 24 * time.Hour, telemetryRetention} {
		reference := memory
		if sink.useRollup(window) {
			reference = rollupReference
		}
		want, err := reference.TelemetryOverview(now, window)
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
	// The 24h window is served from the rollup, so the reference is the
	// rollup's reading of the fixture (see rollupReferenceStore).
	memory := newTelemetryReaderQuerier(rollupReferenceStore(t, records, now))

	window := 24 * time.Hour
	if !sink.useRollup(window) {
		t.Fatal("expected the 24h window to be served from the rollup")
	}
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

func TestPostgresTelemetryQuerierMatchesInMemoryRelayStats(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 30, 0, 0, time.UTC)
	sink := newTestPostgresTelemetrySink(t, now)
	records := parityTelemetryRecords(now)
	if err := sink.WriteTelemetry(context.Background(), records); err != nil {
		t.Fatalf("write telemetry: %v", err)
	}
	memoryStore := &dashboardTelemetryStore{}
	if err := memoryStore.WriteTelemetry(context.Background(), records); err != nil {
		t.Fatalf("write telemetry to in-memory store: %v", err)
	}
	memory := newTelemetryReaderQuerier(memoryStore)
	rollupReference := newTelemetryReaderQuerier(rollupReferenceStore(t, records, now))

	// relay-current is trusted only through this set; relay-ghost is in
	// neither the set nor stamped, so it must vanish from both backends.
	registered := []string{"relay-current"}
	for _, window := range []time.Duration{time.Hour, 24 * time.Hour, telemetryRetention} {
		reference := memory
		if sink.useRollup(window) {
			reference = rollupReference
		}
		want, err := reference.TelemetryRelayStats(now, window, registered)
		if err != nil {
			t.Fatalf("in-memory relay stats (%s): %v", window, err)
		}
		got, err := sink.TelemetryRelayStats(now, window, registered)
		if err != nil {
			t.Fatalf("postgres relay stats (%s): %v", window, err)
		}
		assertSameJSON(t, fmt.Sprintf("relay stats window=%s", window), want, got)

		kept := make(map[string]bool, len(got.Relays))
		for _, row := range got.Relays {
			kept[row.RelayID] = true
		}
		if !kept["relay-current"] || kept["relay-ghost"] {
			t.Fatalf("trust gate wrong (window=%s): relay-current kept=%t, relay-ghost kept=%t", window, kept["relay-current"], kept["relay-ghost"])
		}
	}
}

// rollupReferenceStore is the in-memory store the rollup-served windows (24h
// and 7d) are held to. The rollup folds every event into its session the
// moment it is ingested, so an event dated up to maxTelemetryFutureSkew ahead
// of the broker clock counts at once, whereas the per-event path (and the 1h
// window, which still uses it) hides it until the window reaches it. The
// reference therefore reads a future-dated event as occurring now, which is
// what the rollup shows for one landing in the current hour. Everything else
// in the fixture lies well inside both long windows, so the rollup's
// per-session window membership and hour-granular event edge are not
// exercised here — only its field-merge semantics.
func rollupReferenceStore(t *testing.T, records []TelemetryRecord, now time.Time) *dashboardTelemetryStore {
	t.Helper()
	clamped := make([]TelemetryRecord, 0, len(records))
	for _, record := range records {
		if record.Event.OccurredAt.After(now) {
			record.Event.OccurredAt = now
		}
		clamped = append(clamped, record)
	}
	store := &dashboardTelemetryStore{}
	if err := store.WriteTelemetry(context.Background(), clamped); err != nil {
		t.Fatalf("write telemetry to in-memory reference store: %v", err)
	}
	return store
}

// TestPostgresTelemetryRollupMergesAcrossFlushes writes the parity fixture in
// several chronological flushes, so sessions straddle batches and the rollup's
// ON CONFLICT merge — rather than one batch's array_agg — has to reproduce the
// latest-non-empty-wins, first-client, maxima and flag semantics.
func TestPostgresTelemetryRollupMergesAcrossFlushes(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 30, 0, 0, time.UTC)
	sink := newTestPostgresTelemetrySink(t, now)
	records := parityTelemetryRecords(now)
	chronological := append([]TelemetryRecord{}, records...)
	sort.SliceStable(chronological, func(i, j int) bool {
		a, b := chronological[i], chronological[j]
		if !a.ReceivedAt.Equal(b.ReceivedAt) {
			return a.ReceivedAt.Before(b.ReceivedAt)
		}
		return a.Event.OccurredAt.Before(b.Event.OccurredAt)
	})
	const flushes = 4
	for i := 0; i < flushes; i++ {
		batch := chronological[i*len(chronological)/flushes : (i+1)*len(chronological)/flushes]
		if err := sink.WriteTelemetry(context.Background(), batch); err != nil {
			t.Fatalf("write telemetry batch %d: %v", i, err)
		}
		if err := sink.flush(); err != nil {
			t.Fatalf("flush telemetry batch %d: %v", i, err)
		}
	}
	assertRollupMatchesReference(t, sink, records, now)
}

// TestPostgresTelemetryRollupBackfill stores the parity fixture the way an
// older broker left it — event rows only, no rollup — then opens a new sink
// whose startup backfill must fold those rows into the rollup, chunk by chunk
// under the watermark, before the long windows switch over to it.
func TestPostgresTelemetryRollupBackfill(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 30, 0, 0, time.UTC)
	sink := newTestPostgresTelemetrySink(t, now)
	ctx := context.Background()
	records := parityTelemetryRecords(now)
	var stored []TelemetryRecord
	for _, record := range records {
		if record.Event.Event == telemetryAppConnectionEvent {
			continue // never a row in any broker version
		}
		stored = append(stored, record)
		if err := sink.ensurePartition(ctx, insertReceivedAt(record, now)); err != nil {
			t.Fatalf("ensure partition: %v", err)
		}
	}
	var args []any
	for _, record := range stored {
		args = append(args, telemetryInsertArgs(record, now)...)
	}
	if _, err := sink.pool.Exec(ctx, telemetryInsertStatement(len(stored)), args...); err != nil {
		t.Fatalf("insert rows without rollup: %v", err)
	}
	if got := countRollupSessions(t, sink); got != 0 {
		t.Fatalf("expected no rollup rows before backfill, found %d", got)
	}
	// Forget the completed backfill so the next start redoes it from the
	// oldest partition up to its own start time.
	if _, err := sink.pool.Exec(ctx, `DELETE FROM telemetry_rollup_state`); err != nil {
		t.Fatalf("reset rollup state: %v", err)
	}

	reopened := newTestPostgresTelemetrySinkWithoutCleanup(t, now.Add(time.Minute))
	t.Cleanup(func() { reopened.Close() })
	if got := countRollupSessions(t, sink); got == 0 {
		t.Fatal("expected the backfill to populate the rollup")
	}
	var until, through time.Time
	if err := sink.pool.QueryRow(ctx, `
		SELECT u.value, d.value FROM telemetry_rollup_state u, telemetry_rollup_state d
		WHERE u.key = 'backfill_until' AND d.key = 'backfill_done_through'`).Scan(&until, &through); err != nil {
		t.Fatalf("read rollup state: %v", err)
	}
	if !until.Equal(now.Add(time.Minute)) || through.Before(until) {
		t.Fatalf("backfill watermark did not reach its end: until=%s done_through=%s", until, through)
	}
	// The top-apps rollup is fed by ingestion, not the backfill, and the
	// application_connection records above were never stored; feed them so
	// the overview's top_applications panel has its usual input.
	var appRecords []TelemetryRecord
	for _, record := range records {
		if record.Event.Event == telemetryAppConnectionEvent {
			appRecords = append(appRecords, record)
		}
	}
	if err := reopened.WriteTelemetry(ctx, appRecords); err != nil {
		t.Fatalf("write application records: %v", err)
	}
	assertRollupMatchesReference(t, reopened, records, now)
}

// assertRollupMatchesReference holds every rollup-served query (overview,
// every sessions page, relay stats) for the 24h and 7d windows to the
// in-memory reading of the fixture.
func assertRollupMatchesReference(t *testing.T, sink *PostgresTelemetrySink, records []TelemetryRecord, now time.Time) {
	t.Helper()
	reference := newTelemetryReaderQuerier(rollupReferenceStore(t, records, now))
	for _, window := range []time.Duration{24 * time.Hour, telemetryRetention} {
		if !sink.useRollup(window) {
			t.Fatalf("expected the %s window to be served from the rollup", window)
		}
		want, err := reference.TelemetryOverview(now, window)
		if err != nil {
			t.Fatalf("reference overview (%s): %v", window, err)
		}
		got, err := sink.TelemetryOverview(now, window)
		if err != nil {
			t.Fatalf("rollup overview (%s): %v", window, err)
		}
		assertSameJSON(t, fmt.Sprintf("rollup overview window=%s", window), want, got)

		wantSessions, wantTotal, err := reference.TelemetrySessions(now, window, 0, 100)
		if err != nil {
			t.Fatalf("reference sessions (%s): %v", window, err)
		}
		gotSessions, gotTotal, err := sink.TelemetrySessions(now, window, 0, 100)
		if err != nil {
			t.Fatalf("rollup sessions (%s): %v", window, err)
		}
		if wantTotal != gotTotal {
			t.Fatalf("rollup sessions total (%s): reference %d, rollup %d", window, wantTotal, gotTotal)
		}
		assertSameJSON(t, fmt.Sprintf("rollup sessions window=%s", window), wantSessions, gotSessions)

		registered := []string{"relay-current"}
		wantRelays, err := reference.TelemetryRelayStats(now, window, registered)
		if err != nil {
			t.Fatalf("reference relay stats (%s): %v", window, err)
		}
		gotRelays, err := sink.TelemetryRelayStats(now, window, registered)
		if err != nil {
			t.Fatalf("rollup relay stats (%s): %v", window, err)
		}
		assertSameJSON(t, fmt.Sprintf("rollup relay stats window=%s", window), wantRelays, gotRelays)
	}
}

func countRollupSessions(t *testing.T, sink *PostgresTelemetrySink) int {
	t.Helper()
	var count int
	if err := sink.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM telemetry_sessions`).Scan(&count); err != nil {
		t.Fatalf("count rollup sessions: %v", err)
	}
	return count
}

// TestPostgresTelemetryRollupWindowSelection pins which windows the rollup
// serves: never the 1h window, and nothing until the backfill has completed.
func TestPostgresTelemetryRollupWindowSelection(t *testing.T) {
	sink := &PostgresTelemetrySink{backfillDone: make(chan struct{})}
	for _, window := range []time.Duration{time.Hour, 24 * time.Hour, telemetryRetention} {
		if sink.useRollup(window) {
			t.Fatalf("window %s must not use the rollup before the backfill completes", window)
		}
	}
	sink.markRollupReady()
	if sink.useRollup(time.Hour) {
		t.Fatal("the 1h window must keep aggregating events directly")
	}
	for _, window := range []time.Duration{24 * time.Hour, telemetryRetention} {
		if !sink.useRollup(window) {
			t.Fatalf("window %s must use the rollup once ready", window)
		}
	}
}

// TestPostgresTelemetryRollupPrune ages rollup rows out with the retention
// cutoff: sessions by their latest event, hourly counts by their hour.
func TestPostgresTelemetryRollupPrune(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 30, 0, 0, time.UTC)
	sink := newTestPostgresTelemetrySink(t, now)
	ctx := context.Background()
	old := now.Add(-telemetryRetention - 2*time.Hour)
	recent := now.Add(-time.Hour)
	records := []TelemetryRecord{
		dashboardRecord(old, "old-1", "connection_succeeded", "client-old", "session-old", "relay-1", nil, nil),
		dashboardRecord(recent, "new-1", "connection_succeeded", "client-new", "session-new", "relay-1", nil, nil),
	}
	if err := sink.WriteTelemetry(ctx, records); err != nil {
		t.Fatalf("write telemetry: %v", err)
	}
	if err := sink.flush(); err != nil {
		t.Fatalf("flush telemetry: %v", err)
	}
	if got := countRollupSessions(t, sink); got != 2 {
		t.Fatalf("expected 2 rollup sessions before prune, got %d", got)
	}
	if _, err := sink.PruneTelemetry(now); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := countRollupSessions(t, sink); got != 1 {
		t.Fatalf("expected 1 rollup session after prune, got %d", got)
	}
	var kept string
	if err := sink.pool.QueryRow(ctx, `SELECT session_id FROM telemetry_sessions`).Scan(&kept); err != nil || kept != "session-new" {
		t.Fatalf("kept session = %q (%v), want session-new", kept, err)
	}
	var hours int
	if err := sink.pool.QueryRow(ctx, `SELECT COUNT(DISTINCT hour) FROM telemetry_event_counts`).Scan(&hours); err != nil {
		t.Fatalf("count event-count hours: %v", err)
	}
	if hours != 1 {
		t.Fatalf("expected only the recent hour's counts after prune, got %d hours", hours)
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

	wantRelayStats, err := memory.TelemetryRelayStats(now, time.Hour, nil)
	if err != nil {
		t.Fatalf("in-memory empty relay stats: %v", err)
	}
	gotRelayStats, err := sink.TelemetryRelayStats(now, time.Hour, nil)
	if err != nil {
		t.Fatalf("postgres empty relay stats: %v", err)
	}
	assertSameJSON(t, "empty relay stats", wantRelayStats, gotRelayStats)
}

// TestPostgresTelemetryFailureDiagnostics asserts the SQL path's failure fields
// directly (the parity tests only prove the two backends agree). It exercises
// the error_type fallback, latest-non-empty-wins across two connection_failed
// events, the attribute-less "unknown · unknown" panel key, and a relay
// TopFailureReason tie resolved lexicographically.
func TestPostgresTelemetryFailureDiagnostics(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 30, 0, 0, time.UTC)
	sink := newTestPostgresTelemetrySink(t, now)
	records := []TelemetryRecord{
		// session-a: the first failure classifies a stage and (via error_type,
		// no failure_reason yet) a reason plus a detail; the later failure
		// reports an empty stage and nothing else, so the earlier non-empty
		// values must survive.
		dashboardRecord(now.Add(-5*time.Minute), "a1", "connection_failed", "client-a", "session-a", "",
			map[string]string{"failure_stage": "tls_handshake", "error_type": "cert_error", "failure_detail": "certificate expired"}, nil),
		dashboardRecord(now.Add(-4*time.Minute), "a2", "connection_failed", "client-a", "session-a", "",
			map[string]string{"failure_stage": ""}, nil),
		// session-b: a bare connection_failed with no diagnostics keys the panel
		// as "unknown · unknown".
		dashboardRecord(now.Add(-3*time.Minute), "b1", "connection_failed", "client-b", "session-b", "", nil, nil),
		// relay-tie: bravo via error_type, alpha via failure_reason (preferred
		// over its own error_type). One each ties, so alpha wins the tiebreak.
		dashboardRecord(now.Add(-6*time.Minute), "r1", "relay_attempt_failed", "client-c", "session-c", "relay-tie",
			map[string]string{"error_type": "bravo"}, nil),
		dashboardRecord(now.Add(-2*time.Minute), "r2", "relay_attempt_failed", "client-d", "session-d", "relay-tie",
			map[string]string{"failure_reason": "alpha", "error_type": "zzz"}, nil),
	}
	if err := sink.WriteTelemetry(context.Background(), records); err != nil {
		t.Fatalf("write telemetry: %v", err)
	}

	overview, err := sink.TelemetryOverview(now, time.Hour)
	if err != nil {
		t.Fatalf("postgres overview: %v", err)
	}
	// The overview no longer carries the session list; the per-session failure
	// fields come from the sessions endpoint the dashboard reads.
	sessions, _, err := sink.TelemetrySessions(now, time.Hour, 0, 100)
	if err != nil {
		t.Fatalf("postgres sessions: %v", err)
	}

	session := recentByID(sessions)["session-a"]
	if session.Status != "failed" {
		t.Fatalf("session-a status = %q, want failed", session.Status)
	}
	if session.FailureStage != "tls_handshake" || session.FailureReason != "cert_error" || session.FailureDetail != "certificate expired" {
		t.Fatalf("session-a failure fields wrong (latest-empty must not clobber): %+v", session)
	}

	if got := countByName(overview.FailureReasons, "tls_handshake · cert_error"); got != 1 {
		t.Fatalf("failure_reasons 'tls_handshake · cert_error' = %d, want 1: %+v", got, overview.FailureReasons)
	}
	// session-a's empty second failure and session-b's bare failure both key here.
	if got := countByName(overview.FailureReasons, "unknown · unknown"); got != 2 {
		t.Fatalf("failure_reasons 'unknown · unknown' = %d, want 2: %+v", got, overview.FailureReasons)
	}

	byRelay := make(map[string]relaySummary, len(overview.TopRelays))
	for _, relay := range overview.TopRelays {
		byRelay[relay.RelayID] = relay
	}
	if got := byRelay["relay-tie"]; got.Failures != 2 || got.TopFailureReason != "alpha" {
		t.Fatalf("relay-tie summary = %+v, want failures=2 top=alpha", got)
	}
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
