package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"openrung/internal/relay"
)

type memoryTelemetrySink struct {
	records []TelemetryRecord
}

func (s *memoryTelemetrySink) WriteTelemetry(_ context.Context, records []TelemetryRecord) error {
	s.records = append(s.records, records...)
	return nil
}

func TestTelemetryHandlerStoresSourceIPAndEvents(t *testing.T) {
	sink := &memoryTelemetrySink{}
	payload, err := json.Marshal(telemetryBatch{Events: []TelemetryEvent{{
		SchemaVersion: 1,
		EventID:       "event-1",
		Event:         "connection_attempted",
		OccurredAt:    time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC),
		ClientID:      "client-1",
		SessionID:     "session-1",
	}}})
	if err != nil {
		t.Fatalf("marshal telemetry: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/telemetry/events", bytes.NewReader(payload))
	req.RemoteAddr = "203.0.113.42:54321"
	recorder := httptest.NewRecorder()
	telemetryHandler(sink, nil, newClientIPResolver(nil)).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if len(sink.records) != 1 {
		t.Fatalf("expected one telemetry record, got %d", len(sink.records))
	}
	if got := sink.records[0].SourceIP; got != "203.0.113.42" {
		t.Fatalf("expected retained source IP, got %q", got)
	}
}

func TestTelemetryHandlerStoresBrokerAttestedRelayNodeClass(t *testing.T) {
	now := time.Now().UTC()
	store := NewStore()
	request := validRegisterRequest()
	request.NodeClass = relay.NodeClassFoundation
	desc, err := store.Register(request, now, time.Minute)
	if err != nil {
		t.Fatalf("register foundation relay: %v", err)
	}

	sink := &memoryTelemetrySink{}
	payload, err := json.Marshal(telemetryBatch{Events: []TelemetryEvent{{
		SchemaVersion: 1,
		EventID:       "event-foundation",
		Event:         "connection_succeeded",
		OccurredAt:    now,
		ClientID:      "client-1",
		SessionID:     "session-1",
		RelayID:       desc.ID,
	}}})
	if err != nil {
		t.Fatalf("marshal telemetry: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/telemetry/events", bytes.NewReader(payload))
	recorder := httptest.NewRecorder()
	telemetryHandler(sink, store, newClientIPResolver(nil)).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if len(sink.records) != 1 || sink.records[0].RelayNodeClass != relay.NodeClassFoundation {
		t.Fatalf("stored telemetry class = %+v, want foundation", sink.records)
	}
}

func TestRelayListRecordsPreTunnelClientIP(t *testing.T) {
	sink := &memoryTelemetrySink{}
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), TelemetrySink: sink})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/relays?limit=5", nil)
	req.RemoteAddr = "198.51.100.19:4242"
	req.Header.Set("X-OpenRung-Client-ID", "client-1")
	req.Header.Set("X-OpenRung-Session-ID", "session-1")
	req.Header.Set("X-OpenRung-App-Version", "0.1.0")
	req.Header.Set("X-OpenRung-Android-API", "35")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if len(sink.records) != 1 {
		t.Fatalf("expected one client_seen record, got %d", len(sink.records))
	}
	record := sink.records[0]
	if record.SourceIP != "198.51.100.19" || record.Event.Event != "client_seen" {
		t.Fatalf("unexpected client seen record: %+v", record)
	}
	if record.Event.Attributes["android_api"] != "35" {
		t.Fatalf("expected Android API attribute, got %+v", record.Event.Attributes)
	}
}

func TestTelemetryHandlerRejectsMissingIdentity(t *testing.T) {
	sink := &memoryTelemetrySink{}
	payload := []byte(`{"events":[{"schema_version":1,"event_id":"event-1","event":"connection_attempted","occurred_at":"2026-06-20T12:00:00Z","client_id":"","session_id":"session-1"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/telemetry/events", bytes.NewReader(payload))
	recorder := httptest.NewRecorder()
	telemetryHandler(sink, nil, newClientIPResolver(nil)).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestSpeedTestHandlerStreamsRequestedBytes(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/speed-test?bytes=10000", nil)
	recorder := httptest.NewRecorder()
	speedTestHandler(speedTestMaxConcurrent).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.Len() != 10000 {
		t.Fatalf("expected 10000 bytes, got %d", recorder.Body.Len())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("expected no-store cache control")
	}
}

func TestSpeedTestHandlerRejectsOversizedRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/speed-test?bytes=25000001", nil)
	recorder := httptest.NewRecorder()
	speedTestHandler(speedTestMaxConcurrent).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

// gatedWriter blocks the handler's first body write until released, keeping a
// speed-test stream "in flight" so concurrency limits can be observed.
type gatedWriter struct {
	header  http.Header
	started chan struct{}
	release chan struct{}
	once    sync.Once
	written int
}

func newGatedWriter() *gatedWriter {
	return &gatedWriter{header: make(http.Header), started: make(chan struct{}), release: make(chan struct{})}
}

func (w *gatedWriter) Header() http.Header { return w.header }
func (w *gatedWriter) WriteHeader(int)     {}
func (w *gatedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() {
		close(w.started)
		<-w.release
	})
	w.written += len(p)
	return len(p), nil
}

func TestSpeedTestHandlerLimitsConcurrentStreams(t *testing.T) {
	handler := speedTestHandler(1)

	blocked := newGatedWriter()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		handler.ServeHTTP(blocked, httptest.NewRequest(http.MethodGet, "/api/v1/speed-test?bytes=100000", nil))
	}()
	<-blocked.started

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/speed-test?bytes=100000", nil))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 while a stream holds the only slot, got %d", recorder.Code)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header on busy response")
	}

	close(blocked.release)
	<-firstDone
	if blocked.written != 100000 {
		t.Fatalf("expected first stream to finish with 100000 bytes, got %d", blocked.written)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/speed-test?bytes=10", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected freed slot to serve again, got %d", recorder.Code)
	}
}

func TestJSONLTelemetrySinkLoadsOnlyRecentRecords(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "telemetry.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, occurredAt := range []time.Time{now.Add(-8 * 24 * time.Hour), now.Add(-time.Hour)} {
		record := telemetryRecordAt(occurredAt, "event-"+occurredAt.Format(time.RFC3339))
		if err := encoder.Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	sink, err := newJSONLTelemetrySink(path, func() time.Time { return now })
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	defer sink.Close()
	records := sink.TelemetryRecords(now.Add(-telemetryRetention))
	if len(records) != 1 || records[0].Event.OccurredAt != now.Add(-time.Hour) {
		t.Fatalf("expected only recent record, got %+v", records)
	}
}

func TestJSONLTelemetrySinkPrunesOutOfOrderRecords(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	sink, err := newJSONLTelemetrySink(filepath.Join(t.TempDir(), "telemetry.jsonl"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	records := []TelemetryRecord{
		telemetryRecordAt(now.Add(-time.Hour), "recent-1"),
		telemetryRecordAt(now.Add(-8*24*time.Hour), "old"),
		telemetryRecordAt(now.Add(-2*time.Hour), "recent-2"),
	}
	if err := sink.WriteTelemetry(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	if got := sink.TelemetryRecords(now.Add(-telemetryRetention)); len(got) != 2 {
		t.Fatalf("expected two retained records, got %d", len(got))
	}
}

func TestJSONLTelemetrySinkConcurrentReadWrite(t *testing.T) {
	now := time.Now().UTC()
	sink, err := newJSONLTelemetrySink(filepath.Join(t.TempDir(), "telemetry.jsonl"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	var wg sync.WaitGroup
	for index := 0; index < 20; index++ {
		wg.Add(2)
		go func(index int) {
			defer wg.Done()
			_ = sink.WriteTelemetry(context.Background(), []TelemetryRecord{telemetryRecordAt(now, "event-"+string(rune('a'+index)))})
		}(index)
		go func() {
			defer wg.Done()
			_ = sink.TelemetryRecords(now.Add(-time.Hour))
		}()
	}
	wg.Wait()
	if got := len(sink.TelemetryRecords(now.Add(-time.Hour))); got != 20 {
		t.Fatalf("expected 20 records, got %d", got)
	}
}

func TestJSONLTelemetrySinkRejectsMalformedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.jsonl")
	if err := os.WriteFile(path, []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := newJSONLTelemetrySink(path, time.Now)
	if err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("expected line-specific decode error, got %v", err)
	}
}

func TestJSONLTelemetrySinkBuffersThenFlushes(t *testing.T) {
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "telemetry.jsonl")
	sink, err := newJSONLTelemetrySinkWithIntervals(path, func() time.Time { return now }, 20*time.Millisecond, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	if err := sink.WriteTelemetry(context.Background(), []TelemetryRecord{telemetryRecordAt(now, "buffered")}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("expected write to remain buffered initially, size=%d", info.Size())
	}
	deadline := time.Now().Add(time.Second)
	for {
		info, err := os.Stat(path)
		if err == nil && info.Size() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("telemetry buffer did not flush on schedule")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestJSONLTelemetrySinkCloseFlushesAndSyncs(t *testing.T) {
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "telemetry.jsonl")
	sink, err := newJSONLTelemetrySinkWithIntervals(path, func() time.Time { return now }, time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.WriteTelemetry(context.Background(), []TelemetryRecord{telemetryRecordAt(now, "close")}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), `"event_id":"close"`) {
		t.Fatalf("close did not persist buffered telemetry: %s", contents)
	}
}

func TestTelemetryHandlerRejectsFutureEvent(t *testing.T) {
	sink := &memoryTelemetrySink{}
	payload, err := json.Marshal(telemetryBatch{Events: []TelemetryEvent{{
		SchemaVersion: 1,
		EventID:       "event-1",
		Event:         "connection_attempted",
		OccurredAt:    time.Now().UTC().Add(2 * time.Hour),
		ClientID:      "client-1",
		SessionID:     "session-1",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/telemetry/events", bytes.NewReader(payload))
	recorder := httptest.NewRecorder()
	telemetryHandler(sink, nil, newClientIPResolver(nil)).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for future-dated event, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if len(sink.records) != 0 {
		t.Fatalf("expected no stored records, got %d", len(sink.records))
	}
}

func TestTelemetryHandlerRejectsOversizedAttributeValue(t *testing.T) {
	sink := &memoryTelemetrySink{}
	payload, err := json.Marshal(telemetryBatch{Events: []TelemetryEvent{{
		SchemaVersion: 1,
		EventID:       "event-1",
		Event:         "connection_attempted",
		OccurredAt:    time.Now().UTC(),
		ClientID:      "client-1",
		SessionID:     "session-1",
		Attributes:    map[string]string{"note": strings.Repeat("x", maxTelemetryValueBytes+1)},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/telemetry/events", bytes.NewReader(payload))
	recorder := httptest.NewRecorder()
	telemetryHandler(sink, nil, newClientIPResolver(nil)).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized attribute, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestJSONLTelemetrySinkEnforcesMemoryBudget(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	sink, err := newJSONLTelemetrySink(filepath.Join(t.TempDir(), "telemetry.jsonl"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	sample, err := json.Marshal(telemetryRecordAt(now, "event-0"))
	if err != nil {
		t.Fatal(err)
	}
	recordSize := int64(len(sample) + 1)
	sink.memoryLimit = 2*recordSize + recordSize/2

	for index := 0; index < 5; index++ {
		record := telemetryRecordAt(now, fmt.Sprintf("event-%d", index))
		if err := sink.WriteTelemetry(context.Background(), []TelemetryRecord{record}); err != nil {
			t.Fatal(err)
		}
	}

	records := sink.TelemetryRecords(time.Time{})
	if len(records) != 2 {
		t.Fatalf("expected budget to retain 2 records, got %d", len(records))
	}
	if records[0].Event.EventID != "event-3" || records[1].Event.EventID != "event-4" {
		t.Fatalf("expected oldest records dropped first, got %q and %q", records[0].Event.EventID, records[1].Event.EventID)
	}
	if sink.memoryBytes > sink.memoryLimit {
		t.Fatalf("memory accounting exceeds budget: %d > %d", sink.memoryBytes, sink.memoryLimit)
	}
}

func TestJSONLTelemetrySinkLoadTrimsToNewestWithinBudget(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "telemetry.jsonl")

	// A pre-existing file with far more records than the budget can hold, oldest
	// first (append-only order).
	const total = 50
	var buf bytes.Buffer
	var recordSize int64
	for index := 0; index < total; index++ {
		line, err := json.Marshal(telemetryRecordAt(now, fmt.Sprintf("event-%03d", index)))
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
		recordSize = int64(len(line) + 1)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	// Budget holds 10 records; loading must keep the 10 newest and honour the
	// budget invariant despite the mid-scan 2× overshoot.
	const want = 10
	sink, err := newJSONLTelemetrySinkWithLimits(path, func() time.Time { return now }, time.Hour, time.Hour, want*recordSize, maxTelemetryFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	if sink.memoryBytes > sink.memoryLimit {
		t.Fatalf("loaded set exceeds budget: %d > %d", sink.memoryBytes, sink.memoryLimit)
	}
	records := sink.TelemetryRecords(time.Time{})
	if len(records) != want {
		t.Fatalf("expected %d newest records retained, got %d", want, len(records))
	}
	for i, record := range records {
		wantID := fmt.Sprintf("event-%03d", total-want+i)
		if record.Event.EventID != wantID {
			t.Fatalf("retained record %d = %q, want %q (oldest should be dropped)", i, record.Event.EventID, wantID)
		}
	}
}

func TestJSONLTelemetrySinkLoadCompactionStaysLinear(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "telemetry.jsonl")

	// Many more records than the budget retains, so loading must trim repeatedly.
	const total = 20_000
	sample, err := json.Marshal(telemetryRecordAt(now, "event-000000"))
	if err != nil {
		t.Fatal(err)
	}
	recordSize := int64(len(sample) + 1)

	var buf bytes.Buffer
	buf.Grow(int(recordSize) * total)
	for index := 0; index < total; index++ {
		line, err := json.Marshal(telemetryRecordAt(now, fmt.Sprintf("event-%06d", index)))
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	// The old per-line trim shifted the whole retained slice on every over-budget
	// line, so total shifts were ~O(total²) (here ~1000× this bound) and startup
	// hung on a large file. The amortised trim moves each record a constant number
	// of times, keeping shifts O(total). This is a deterministic, hardware-
	// independent guard against reintroducing the quadratic load.
	const retain = 2000
	sink, err := newJSONLTelemetrySinkWithLimits(path, func() time.Time { return now }, time.Hour, time.Hour, retain*recordSize, maxTelemetryFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	if sink.memoryBytes > sink.memoryLimit {
		t.Fatalf("loaded set exceeds budget: %d > %d", sink.memoryBytes, sink.memoryLimit)
	}
	records := sink.TelemetryRecords(time.Time{})
	if len(records) == 0 || records[len(records)-1].Event.EventID != "event-019999" {
		t.Fatalf("expected newest record retained last, got %d records", len(records))
	}
	if sink.compactionShifts > 4*int64(total) {
		t.Fatalf("load shifted %d records for %d total; expected O(total) — possible O(n²) regression", sink.compactionShifts, total)
	}
}

func TestJSONLTelemetrySinkCompactsOversizedFile(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "telemetry.jsonl")
	sink, err := newJSONLTelemetrySinkWithIntervals(path, func() time.Time { return now }, time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	sample, err := json.Marshal(telemetryRecordAt(now, "event-0"))
	if err != nil {
		t.Fatal(err)
	}
	recordSize := int64(len(sample) + 1)
	sink.memoryLimit = 2*recordSize + recordSize/2
	sink.fileLimit = 3 * recordSize

	for index := 0; index < 6; index++ {
		record := telemetryRecordAt(now, fmt.Sprintf("event-%d", index))
		if err := sink.WriteTelemetry(context.Background(), []TelemetryRecord{record}); err != nil {
			t.Fatal(err)
		}
	}
	if sink.fileBytes > sink.fileLimit {
		t.Fatalf("file accounting exceeds budget after writes: %d > %d", sink.fileBytes, sink.fileLimit)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
	if len(lines) >= 6 {
		t.Fatalf("expected compaction to shrink the file, still holds %d records", len(lines))
	}
	for _, line := range lines {
		var record TelemetryRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("compacted file contains malformed record: %v", err)
		}
	}
	if !strings.Contains(lines[len(lines)-1], `"event_id":"event-5"`) {
		t.Fatalf("expected newest record to survive compaction, got %s", lines[len(lines)-1])
	}
}

func TestJSONLTelemetrySinkRetentionIgnoresClientClock(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	sink, err := newJSONLTelemetrySink(filepath.Join(t.TempDir(), "telemetry.jsonl"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	// Received 8 days ago but "occurred" a year from now: the client-controlled
	// event time must not be able to extend retention.
	forged := telemetryRecordAt(now.Add(365*24*time.Hour), "forged-future")
	forged.ReceivedAt = now.Add(-8 * 24 * time.Hour)
	current := telemetryRecordAt(now, "current")
	if err := sink.WriteTelemetry(context.Background(), []TelemetryRecord{forged, current}); err != nil {
		t.Fatal(err)
	}

	records := sink.TelemetryRecords(time.Time{})
	if len(records) != 1 || records[0].Event.EventID != "current" {
		t.Fatalf("expected only the honestly-timed record to survive, got %+v", records)
	}
}

func TestClientSeenDeduperSuppressesRepeatsWithinWindow(t *testing.T) {
	dedup := newClientSeenDeduper(4*time.Minute, 10)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	if !dedup.shouldRecord("client-1", "session-1", now) {
		t.Fatal("first sighting must record")
	}
	if dedup.shouldRecord("client-1", "session-1", now.Add(time.Minute)) {
		t.Fatal("repeat inside the window must be suppressed")
	}
	if !dedup.shouldRecord("client-1", "session-2", now) {
		t.Fatal("a different session must record")
	}
	if !dedup.shouldRecord("client-1", "session-1", now.Add(5*time.Minute)) {
		t.Fatal("sighting after the window must record again")
	}
}

func TestRelayListDedupsRepeatClientSeen(t *testing.T) {
	sink := &memoryTelemetrySink{}
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), TelemetrySink: sink})
	for range 3 {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil)
		req.Header.Set("X-OpenRung-Client-ID", "client-1")
		req.Header.Set("X-OpenRung-Session-ID", "session-1")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", recorder.Code)
		}
	}
	if len(sink.records) != 1 {
		t.Fatalf("expected one client_seen record for repeat polling, got %d", len(sink.records))
	}
}

func telemetryRecordAt(occurredAt time.Time, eventID string) TelemetryRecord {
	return TelemetryRecord{
		ReceivedAt: occurredAt,
		SourceIP:   "203.0.113.10",
		Event: TelemetryEvent{
			SchemaVersion: 1,
			EventID:       eventID,
			Event:         "connection_attempted",
			OccurredAt:    occurredAt,
			ClientID:      "client-1",
			SessionID:     eventID,
		},
	}
}

func TestTelemetryRetainedBudgetPruneAndSince(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	retained := telemetryRetained{memoryLimit: 30}
	for i := 0; i < 4; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		retained.append(TelemetryRecord{
			ReceivedAt: at,
			Event:      TelemetryEvent{EventID: fmt.Sprintf("event-%d", i), OccurredAt: at},
		}, 10)
	}

	retained.dropOldestOverBudget()
	if retained.memoryBytes != 30 || len(retained.records) != 3 {
		t.Fatalf("unexpected retained state after budget drop: %d bytes, %d records", retained.memoryBytes, len(retained.records))
	}
	if retained.records[0].record.Event.EventID != "event-1" {
		t.Fatalf("expected oldest record dropped first, got %q", retained.records[0].record.Event.EventID)
	}

	retained.prune(now.Add(2 * time.Minute))
	if retained.memoryBytes != 20 || len(retained.records) != 2 {
		t.Fatalf("unexpected retained state after prune: %d bytes, %d records", retained.memoryBytes, len(retained.records))
	}

	since := retained.recordsSince(now.Add(3 * time.Minute))
	if len(since) != 1 || since[0].Event.EventID != "event-3" {
		t.Fatalf("unexpected recordsSince result: %+v", since)
	}
}

func appConnectionRecordAt(receivedAt time.Time, eventID, application string) TelemetryRecord {
	record := telemetryRecordAt(receivedAt, eventID)
	record.Event.Event = telemetryAppConnectionEvent
	record.Event.Application = application
	record.Event.DestinationIP = "198.51.100.9"
	record.Event.DestinationPort = 443
	return record
}

func TestTelemetryAppRollupBucketsPrunesAndCaps(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 30, 0, 0, time.UTC)
	var rollup telemetryAppRollup

	rollup.add(now.Truncate(time.Hour), "app-a", 1)
	rollup.add(now.Truncate(time.Hour), "app-a", 2)
	rollup.add(now.Add(-70*time.Minute).Truncate(time.Hour), "app-b", 1)
	rollup.add(now.Add(-30*time.Hour).Truncate(time.Hour), "app-old", 5)
	rollup.add(now.Truncate(time.Hour), "", 9) // empty package names never count

	// The 1h window's start (11:30) truncates to the 11:00 bucket, so app-b —
	// received before the window opened — still counts: the edge is
	// hour-granular by design, matching the SQL rollup filter.
	counts := rollup.countsIn(now, time.Hour)
	if counts["app-a"] != 3 || counts["app-b"] != 1 || len(counts) != 2 {
		t.Fatalf("unexpected 1h counts: %+v", counts)
	}
	counts = rollup.countsIn(now, 48*time.Hour)
	if counts["app-old"] != 5 || len(counts) != 3 {
		t.Fatalf("unexpected 48h counts: %+v", counts)
	}

	rollup.prune(now.Add(-24 * time.Hour))
	if counts = rollup.countsIn(now, 48*time.Hour); counts["app-old"] != 0 || len(counts) != 2 {
		t.Fatalf("expected pruned rollup to drop app-old: %+v", counts)
	}
	if rollup.entries != 2 {
		t.Fatalf("expected 2 entries after prune, got %d", rollup.entries)
	}

	var capped telemetryAppRollup
	hour := now.Truncate(time.Hour)
	for i := 0; i < maxTelemetryAppRollupEntries; i++ {
		if !capped.add(hour, fmt.Sprintf("app-%05d", i), 1) {
			t.Fatalf("unexpected cap refusal at entry %d", i)
		}
	}
	if capped.add(hour, "app-overflow", 1) {
		t.Fatal("expected the entry cap to refuse a new application")
	}
	if !capped.add(hour, "app-00000", 1) {
		t.Fatal("existing applications must keep counting at the cap")
	}
}

func TestJSONLTelemetrySinkRollsUpApplicationConnections(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 30, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "telemetry.jsonl")
	sink, err := newJSONLTelemetrySink(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	records := []TelemetryRecord{
		telemetryRecordAt(now.Add(-time.Minute), "kept-1"),
		appConnectionRecordAt(now.Add(-2*time.Minute), "app-1", "com.instagram.android"),
		appConnectionRecordAt(now.Add(-3*time.Minute), "app-2", "com.instagram.android"),
		appConnectionRecordAt(now.Add(-70*time.Minute), "app-3", "org.telegram.messenger"),
	}
	if err := sink.WriteTelemetry(context.Background(), records); err != nil {
		t.Fatal(err)
	}

	if got := sink.TelemetryRecords(now.Add(-telemetryRetention)); len(got) != 1 || got[0].Event.EventID != "kept-1" {
		t.Fatalf("application_connection records must not be retained: %+v", got)
	}
	counts := sink.AppConnectionCounts(now, time.Hour)
	if counts["com.instagram.android"] != 2 || counts["org.telegram.messenger"] != 1 {
		t.Fatalf("unexpected rollup counts: %+v", counts)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), telemetryAppConnectionEvent) || strings.Contains(string(data), "198.51.100.9") {
		t.Fatalf("application_connection payloads must never reach disk: %s", data)
	}
}

func TestJSONLTelemetrySinkSkipsLegacyApplicationConnectionRows(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 30, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "telemetry.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, record := range []TelemetryRecord{
		appConnectionRecordAt(now.Add(-time.Hour), "legacy-app", "com.instagram.android"),
		telemetryRecordAt(now.Add(-time.Minute), "kept-1"),
	} {
		if err := encoder.Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	sink, err := newJSONLTelemetrySink(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	if got := sink.TelemetryRecords(now.Add(-telemetryRetention)); len(got) != 1 || got[0].Event.EventID != "kept-1" {
		t.Fatalf("legacy application_connection rows must be skipped at load: %+v", got)
	}
	// Legacy rows are not folded into the rollup either — whether the file
	// still holds one depends on compaction timing, so counting them would
	// make restart counts nondeterministic.
	if counts := sink.AppConnectionCounts(now, telemetryRetention); len(counts) != 0 {
		t.Fatalf("legacy rows must not seed the rollup: %+v", counts)
	}
}

func TestTelemetryReaderQuerierServesTopAppsFromRollup(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 30, 0, 0, time.UTC)
	store := &dashboardTelemetryStore{}
	err := store.WriteTelemetry(context.Background(), []TelemetryRecord{
		telemetryRecordAt(now.Add(-time.Minute), "session-event"),
		appConnectionRecordAt(now.Add(-2*time.Minute), "app-1", "com.instagram.android"),
		appConnectionRecordAt(now.Add(-3*time.Minute), "app-2", "com.instagram.android"),
		appConnectionRecordAt(now.Add(-4*time.Minute), "app-3", "org.telegram.messenger"),
	})
	if err != nil {
		t.Fatal(err)
	}
	overview, err := newTelemetryReaderQuerier(store).TelemetryOverview(now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.TopApps) != 2 || overview.TopApps[0].Name != "com.instagram.android" || overview.TopApps[0].Count != 2 {
		t.Fatalf("unexpected top apps: %+v", overview.TopApps)
	}
	// The app-connection events created no sessions or clients: only the
	// stored record's session counts.
	if overview.Totals.Sessions != 1 || overview.Totals.Clients != 1 {
		t.Fatalf("application_connection must not create sessions: %+v", overview.Totals)
	}
}
