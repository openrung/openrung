package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPostgresTelemetrySinkPersistsAndReadsBack(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	sink := newTestPostgresTelemetrySink(t, now)

	records := []TelemetryRecord{
		{
			ReceivedAt: now,
			SourceIP:   "203.0.113.42",
			Event: TelemetryEvent{
				SchemaVersion: 1,
				EventID:       "event-1",
				Event:         "connection_succeeded",
				OccurredAt:    now.Add(-time.Second),
				ClientID:      "client-1",
				SessionID:     "session-1",
				RelayID:       "relay-1",
				Attributes:    map[string]string{"app_version": "1.2.3"},
				Measurements:  map[string]int64{"relay_tcp_ms": 42},
			},
		},
		{
			ReceivedAt: now,
			Event: TelemetryEvent{
				SchemaVersion: 1,
				EventID:       "event-2",
				Event:         "client_seen",
				OccurredAt:    now,
				ClientID:      "client-2",
				SessionID:     "session-2",
			},
		},
	}
	if err := sink.WriteTelemetry(context.Background(), records); err != nil {
		t.Fatalf("write telemetry: %v", err)
	}

	// Writes buffer until the ticker or a full batch flushes them; the reader
	// flushes before selecting, so it sees them either way.
	if got := countTelemetryRows(t, sink); got != 0 {
		t.Fatalf("expected buffered records not yet inserted, found %d rows", got)
	}
	if got := sink.TelemetryRecords(now.Add(-time.Hour)); len(got) != 2 {
		t.Fatalf("expected reader to flush and return 2 records, got %d", len(got))
	}
	if got := countTelemetryRows(t, sink); got != 2 {
		t.Fatalf("expected 2 rows after read-triggered flush, found %d", got)
	}

	// A fresh instance must serve the same records straight from Postgres —
	// there is no startup backfill or in-memory copy to rely on.
	reopened := newTestPostgresTelemetrySinkWithoutCleanup(t, now.Add(time.Minute))
	t.Cleanup(func() { reopened.Close() })
	loaded := reopened.TelemetryRecords(now.Add(-time.Hour))
	if len(loaded) != 2 {
		t.Fatalf("expected 2 records from reopened sink, got %d", len(loaded))
	}
	byID := make(map[string]TelemetryRecord, len(loaded))
	for _, record := range loaded {
		byID[record.Event.EventID] = record
	}
	first, ok := byID["event-1"]
	if !ok {
		t.Fatalf("expected event-1 in read-back, got %+v", loaded)
	}
	if !first.ReceivedAt.Equal(now) || first.SourceIP != "203.0.113.42" {
		t.Fatalf("unexpected read-back envelope: %+v", first)
	}
	want := records[0].Event
	got := first.Event
	if got.EventID != want.EventID || got.Event != want.Event || !got.OccurredAt.Equal(want.OccurredAt) ||
		got.ClientID != want.ClientID || got.SessionID != want.SessionID || got.RelayID != want.RelayID ||
		got.SchemaVersion != want.SchemaVersion || got.Attributes["app_version"] != "1.2.3" ||
		got.Measurements["relay_tcp_ms"] != 42 {
		t.Fatalf("read-back event does not round-trip: got %+v want %+v", got, want)
	}
	second, ok := byID["event-2"]
	if !ok {
		t.Fatalf("expected event-2 in read-back, got %+v", loaded)
	}
	if second.SourceIP != "" || second.Event.RelayID != "" {
		t.Fatalf("expected empty source IP and relay ID to round-trip as empty, got %+v", second)
	}
}

func TestPostgresTelemetrySinkCreatesDailyPartitions(t *testing.T) {
	now := time.Date(2026, 6, 24, 23, 30, 0, 0, time.UTC)
	sink := newTestPostgresTelemetrySink(t, now)

	// Startup pre-creates today's and tomorrow's partitions.
	for _, day := range []string{"20260624", "20260625"} {
		if !telemetryPartitionExists(t, sink, day) {
			t.Fatalf("expected startup to create partition for %s", day)
		}
	}

	// A record beyond the pre-created days must create its partition on demand.
	record := validTelemetryRecord(now.Add(72*time.Hour), "event-later")
	if err := sink.WriteTelemetry(context.Background(), []TelemetryRecord{record}); err != nil {
		t.Fatalf("write telemetry: %v", err)
	}
	if err := sink.flush(); err != nil {
		t.Fatalf("flush telemetry: %v", err)
	}
	if !telemetryPartitionExists(t, sink, "20260627") {
		t.Fatal("expected write path to create the partition for its day")
	}
	if got := countTelemetryRows(t, sink); got != 1 {
		t.Fatalf("expected 1 row after flush, found %d", got)
	}
}

func TestPostgresTelemetrySinkReaderIsWindowBounded(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	sink := newTestPostgresTelemetrySink(t, now)

	const total = 30
	records := make([]TelemetryRecord, 0, total)
	for i := 0; i < total; i++ {
		records = append(records, validTelemetryRecord(now.Add(-time.Duration(i)*time.Minute), fmt.Sprintf("event-%03d", i)))
	}
	if err := sink.WriteTelemetry(context.Background(), records); err != nil {
		t.Fatalf("write telemetry: %v", err)
	}
	if err := sink.flush(); err != nil {
		t.Fatalf("flush telemetry: %v", err)
	}

	// The operational reader returns only the requested window, oldest first.
	loaded := sink.TelemetryRecords(now.Add(-5 * time.Minute))
	if len(loaded) != 6 {
		t.Fatalf("expected 6 records inside the 5-minute window, got %d", len(loaded))
	}
	for i := 1; i < len(loaded); i++ {
		if loaded[i].ReceivedAt.Before(loaded[i-1].ReceivedAt) {
			t.Fatal("expected records ordered oldest-received first")
		}
	}
}

func TestTelemetryInsertStatement(t *testing.T) {
	got := telemetryInsertStatement(2)
	want := `INSERT INTO telemetry_events (` + telemetryInsertColumns + `) VALUES ` +
		`($1,$2,$3,$4,$5,$6,$7,$8,$9),($10,$11,$12,$13,$14,$15,$16,$17,$18)`
	if got != want {
		t.Fatalf("unexpected insert statement:\n got %s\nwant %s", got, want)
	}
	if count := strings.Count(telemetryInsertStatement(postgresTelemetryInsertChunk), "$"); count != postgresTelemetryInsertChunk*9 {
		t.Fatalf("expected %d placeholders for a full chunk, got %d", postgresTelemetryInsertChunk*9, count)
	}
}

func TestTelemetryInsertArgs(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	record := TelemetryRecord{
		ReceivedAt: now,
		SourceIP:   "203.0.113.42",
		Event: TelemetryEvent{
			SchemaVersion:   1,
			EventID:         "event-1",
			Event:           "connection_succeeded",
			OccurredAt:      now.Add(-time.Second),
			ClientID:        "client-1",
			SessionID:       "session-1",
			RelayID:         "relay-1",
			Application:     "org.example.app",
			DestinationIP:   "198.51.100.7",
			DestinationPort: 443,
			Protocol:        "tcp",
			Attributes:      map[string]string{"k": "v"},
			Measurements:    map[string]int64{"relay_tcp_ms": 42},
		},
	}
	args := telemetryInsertArgs(record, now)
	if len(args) != 9 {
		t.Fatalf("expected 9 args, got %d", len(args))
	}
	if got := args[0].(time.Time); !got.Equal(now) {
		t.Fatalf("unexpected received_at: %v", got)
	}
	if got := args[1].(netip.Addr); got != netip.MustParseAddr("203.0.113.42") {
		t.Fatalf("unexpected source_ip: %v", got)
	}
	if args[7] != "relay-1" {
		t.Fatalf("unexpected relay_id: %v", args[7])
	}
	var payload telemetryEventPayload
	if err := json.Unmarshal(args[8].([]byte), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.SchemaVersion != 1 || payload.Application != "org.example.app" ||
		payload.DestinationIP != "198.51.100.7" || payload.DestinationPort != 443 ||
		payload.Protocol != "tcp" || payload.Attributes["k"] != "v" || payload.Measurements["relay_tcp_ms"] != 42 {
		t.Fatalf("payload does not round-trip: %+v", payload)
	}

	// Missing envelope options become NULLs, and an unparseable source IP
	// must not fail the batch.
	minimal := telemetryInsertArgs(TelemetryRecord{SourceIP: "not-an-ip", Event: TelemetryEvent{OccurredAt: now}}, now)
	if minimal[1] != nil || minimal[7] != nil {
		t.Fatalf("expected NULL source_ip and relay_id, got %v and %v", minimal[1], minimal[7])
	}
	if got := minimal[0].(time.Time); !got.Equal(now) {
		t.Fatalf("expected occurred_at fallback for received_at, got %v", got)
	}
}

func TestPostgresTelemetrySinkPendingCapDropsOldest(t *testing.T) {
	sink := &PostgresTelemetrySink{pendingLimit: 30}
	for i := 0; i < 4; i++ {
		record := TelemetryRecord{Event: TelemetryEvent{EventID: fmt.Sprintf("event-%d", i)}}
		sink.pending = append(sink.pending, storedTelemetryRecord{record: record, bytes: 10})
		sink.pendingBytes += 10
	}
	if dropped := sink.dropOldestPendingOverCapLocked(); dropped != 1 {
		t.Fatalf("expected 1 dropped record, got %d", dropped)
	}
	if sink.pendingBytes != 30 || len(sink.pending) != 3 {
		t.Fatalf("unexpected pending state: %d bytes, %d records", sink.pendingBytes, len(sink.pending))
	}
	if sink.pending[0].record.Event.EventID != "event-1" {
		t.Fatalf("expected oldest record dropped first, got %q", sink.pending[0].record.Event.EventID)
	}
}

func validTelemetryRecord(at time.Time, eventID string) TelemetryRecord {
	return TelemetryRecord{
		ReceivedAt: at,
		SourceIP:   "203.0.113.42",
		Event: TelemetryEvent{
			SchemaVersion: 1,
			EventID:       eventID,
			Event:         "client_seen",
			OccurredAt:    at,
			ClientID:      "client-1",
			SessionID:     "session-1",
		},
	}
}

func newTestPostgresTelemetrySink(t *testing.T, now time.Time) *PostgresTelemetrySink {
	t.Helper()
	sink := newTestPostgresTelemetrySinkWithoutCleanup(t, now)
	cleanupPostgresTelemetry(t, sink)
	t.Cleanup(func() {
		cleanupPostgresTelemetry(t, sink)
		sink.Close()
	})
	return sink
}

func newTestPostgresTelemetrySinkWithoutCleanup(t *testing.T, now time.Time) *PostgresTelemetrySink {
	t.Helper()
	databaseURL := os.Getenv("OPENRUNG_TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("OPENRUNG_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// An hour-long flush interval keeps the ticker out of the way; tests
	// flush explicitly.
	sink, err := newPostgresTelemetrySink(ctx, databaseURL, func() time.Time { return now }, time.Hour)
	if err != nil {
		t.Fatalf("open postgres telemetry sink: %v", err)
	}
	return sink
}

func cleanupPostgresTelemetry(t *testing.T, sink *PostgresTelemetrySink) {
	t.Helper()
	if _, err := sink.pool.Exec(context.Background(), `TRUNCATE telemetry_events`); err != nil {
		t.Fatalf("cleanup telemetry events: %v", err)
	}
	sink.mu.Lock()
	sink.pending = nil
	sink.pendingBytes = 0
	sink.mu.Unlock()
}

func countTelemetryRows(t *testing.T, sink *PostgresTelemetrySink) int {
	t.Helper()
	var count int
	if err := sink.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM telemetry_events`).Scan(&count); err != nil {
		t.Fatalf("count telemetry rows: %v", err)
	}
	return count
}

func telemetryPartitionExists(t *testing.T, sink *PostgresTelemetrySink, day string) bool {
	t.Helper()
	var exists *string
	if err := sink.pool.QueryRow(context.Background(), `SELECT to_regclass($1)::text`, "telemetry_events_"+day).Scan(&exists); err != nil {
		t.Fatalf("check partition: %v", err)
	}
	return exists != nil
}
