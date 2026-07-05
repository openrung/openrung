package broker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxTelemetryBodyBytes  = 512 << 10
	maxTelemetryEvents     = 200
	maxSpeedTestBytes      = 25_000_000
	telemetryRetention     = 7 * 24 * time.Hour
	telemetryFlushInterval = 5 * time.Second
	telemetrySyncInterval  = 30 * time.Second

	// maxTelemetryFutureSkew tolerates client clock drift. Events dated further
	// into the future are rejected so forged timestamps cannot pollute
	// time-windowed dashboards; retention itself keys off the server-assigned
	// ReceivedAt and never trusts the client clock.
	maxTelemetryFutureSkew = time.Hour

	// Length caps for free-form event fields keep a single record small; the
	// identity fields have their own caps in validateTelemetryEvent.
	maxTelemetryKeyBytes   = 64
	maxTelemetryValueBytes = 256

	// maxTelemetryMemoryBytes bounds the decoded records held in memory for the
	// dashboard, and maxTelemetryFileBytes bounds the JSONL file on disk (it is
	// compacted down to the retained in-memory set when appends exceed the
	// budget). Together they cap what an anonymous telemetry flood can consume.
	maxTelemetryMemoryBytes = 64 << 20
	maxTelemetryFileBytes   = 256 << 20

	// clientSeenDedupWindow suppresses repeat client_seen records per client
	// session so relay-list polling is not one disk write per request. It stays
	// below the 5-minute operational-stats window so an active client is still
	// recorded at least once per window.
	clientSeenDedupWindow     = 4 * time.Minute
	clientSeenDedupMaxEntries = 10_000

	// speedTestWriteWindow is how long a single speed-test download may take
	// before the connection is cut. The handler extends its own write deadline
	// to this because slow links legitimately need minutes for 25 MB, far past
	// the server-wide write timeout.
	speedTestWriteWindow = 5 * time.Minute
)

func speedTestHandler(maxConcurrent int) http.HandlerFunc {
	slots := make(chan struct{}, maxConcurrent)
	return func(w http.ResponseWriter, r *http.Request) {
		requested, err := strconv.Atoi(r.URL.Query().Get("bytes"))
		if err != nil || requested < 1 || requested > maxSpeedTestBytes {
			writeError(w, http.StatusBadRequest, "bytes must be between 1 and 25000000")
			return
		}

		// Each stream can push up to 25 MB, so the number running at once — not
		// the per-IP request rate — is what bounds worst-case broker egress.
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			w.Header().Set("Retry-After", "10")
			writeError(w, http.StatusTooManyRequests, "speed test is busy, retry later")
			return
		}

		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(speedTestWriteWindow))

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(requested))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = io.CopyN(w, speedTestReader{}, int64(requested))
	}
}

type speedTestReader struct{}

func (speedTestReader) Read(buffer []byte) (int, error) {
	for index := range buffer {
		buffer[index] = byte((index*31 + 17) % 251)
	}
	return len(buffer), nil
}

type TelemetryEvent struct {
	SchemaVersion   int               `json:"schema_version"`
	EventID         string            `json:"event_id"`
	Event           string            `json:"event"`
	OccurredAt      time.Time         `json:"occurred_at"`
	ClientID        string            `json:"client_id"`
	SessionID       string            `json:"session_id"`
	RelayID         string            `json:"relay_id,omitempty"`
	Application     string            `json:"application_package,omitempty"`
	ApplicationID   int               `json:"application_uid,omitempty"`
	DestinationIP   string            `json:"destination_ip,omitempty"`
	DestinationPort int               `json:"destination_port,omitempty"`
	Protocol        string            `json:"protocol,omitempty"`
	Attributes      map[string]string `json:"attributes,omitempty"`
	Measurements    map[string]int64  `json:"measurements,omitempty"`
}

type telemetryBatch struct {
	Events []TelemetryEvent `json:"events"`
}

type TelemetryRecord struct {
	ReceivedAt time.Time      `json:"received_at"`
	SourceIP   string         `json:"source_ip"`
	Event      TelemetryEvent `json:"event"`
}

type TelemetrySink interface {
	WriteTelemetry(context.Context, []TelemetryRecord) error
}

type TelemetryReader interface {
	TelemetryRecords(time.Time) []TelemetryRecord
}

// storedTelemetryRecord pairs a record with its encoded JSONL size so the
// memory and file budgets can be enforced without re-marshalling.
type storedTelemetryRecord struct {
	record TelemetryRecord
	bytes  int64
}

type JSONLTelemetrySink struct {
	mu          sync.RWMutex
	path        string
	file        *os.File
	writer      *bufio.Writer
	records     []storedTelemetryRecord
	memoryBytes int64
	fileBytes   int64
	memoryLimit int64
	fileLimit   int64
	now         func() time.Time
	stop        chan struct{}
	done        chan struct{}
	closeOnce   sync.Once
	closeErr    error
	writeErr    error
	closed      bool
}

func NewJSONLTelemetrySink(path string) (*JSONLTelemetrySink, error) {
	return newJSONLTelemetrySink(path, time.Now)
}

func newJSONLTelemetrySink(path string, now func() time.Time) (*JSONLTelemetrySink, error) {
	return newJSONLTelemetrySinkWithIntervals(path, now, telemetryFlushInterval, telemetrySyncInterval)
}

func newJSONLTelemetrySinkWithIntervals(path string, now func() time.Time, flushInterval, syncInterval time.Duration) (*JSONLTelemetrySink, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("telemetry file path is required")
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create telemetry directory: %w", err)
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open telemetry file: %w", err)
	}
	sink := &JSONLTelemetrySink{
		path: path, file: file, now: now, stop: make(chan struct{}), done: make(chan struct{}),
		memoryLimit: maxTelemetryMemoryBytes, fileLimit: maxTelemetryFileBytes,
	}
	if err := sink.loadRecent(); err != nil {
		_ = file.Close()
		return nil, err
	}
	sink.writer = bufio.NewWriter(file)
	if err := sink.maybeCompactLocked(); err != nil {
		_ = sink.file.Close()
		return nil, fmt.Errorf("compact telemetry file: %w", err)
	}
	go sink.persistenceLoop(flushInterval, syncInterval)
	return sink, nil
}

func (s *JSONLTelemetrySink) loadRecent() error {
	info, err := s.file.Stat()
	if err != nil {
		return fmt.Errorf("stat telemetry file: %w", err)
	}
	s.fileBytes = info.Size()

	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek telemetry file: %w", err)
	}
	cutoff := s.now().UTC().Add(-telemetryRetention)
	scanner := bufio.NewScanner(s.file)
	scanner.Buffer(make([]byte, 64<<10), maxTelemetryBodyBytes*2)
	line := 0
	for scanner.Scan() {
		line++
		var record TelemetryRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return fmt.Errorf("decode telemetry record on line %d: %w", line, err)
		}
		if retentionTime(record).Before(cutoff) {
			continue
		}
		size := int64(len(scanner.Bytes()) + 1)
		s.records = append(s.records, storedTelemetryRecord{record: record, bytes: size})
		s.memoryBytes += size
		// Enforce the budget while scanning so loading an oversized file cannot
		// balloon memory before a post-load trim.
		s.dropOldestOverBudgetLocked()
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read telemetry file: %w", err)
	}
	_, err = s.file.Seek(0, io.SeekEnd)
	return err
}

// retentionTime is the timestamp retention decisions key off: the
// server-assigned receipt time, falling back to the event time for legacy
// records persisted before ReceivedAt existed. OccurredAt alone is never
// trusted — it is client-controlled.
func retentionTime(record TelemetryRecord) time.Time {
	if !record.ReceivedAt.IsZero() {
		return record.ReceivedAt
	}
	return record.Event.OccurredAt
}

func (s *JSONLTelemetrySink) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closed = true
		if err := s.flushLocked(true); err != nil {
			s.closeErr = err
		}
		if err := s.file.Close(); err != nil && s.closeErr == nil {
			s.closeErr = err
		}
	})
	return s.closeErr
}

func (s *JSONLTelemetrySink) WriteTelemetry(_ context.Context, records []TelemetryRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("telemetry storage is closed")
	}
	if s.writeErr != nil {
		return s.writeErr
	}

	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("encode telemetry record: %w", err)
		}
		line = append(line, '\n')
		if _, err := s.writer.Write(line); err != nil {
			s.writeErr = fmt.Errorf("write telemetry record: %w", err)
			return s.writeErr
		}
		size := int64(len(line))
		s.records = append(s.records, storedTelemetryRecord{record: record, bytes: size})
		s.memoryBytes += size
		s.fileBytes += size
	}
	s.pruneLocked(s.now().UTC().Add(-telemetryRetention))
	s.dropOldestOverBudgetLocked()
	if err := s.maybeCompactLocked(); err != nil {
		s.writeErr = fmt.Errorf("compact telemetry file: %w", err)
		return s.writeErr
	}
	return nil
}

// maybeCompactLocked compacts once the file is over budget, but only when the
// retained set is small enough that rewriting meaningfully shrinks the file —
// otherwise every append would trigger a futile full rewrite. With the default
// limits (memory budget ≤ ¼ of the file budget) an over-budget file always
// qualifies.
func (s *JSONLTelemetrySink) maybeCompactLocked() error {
	if s.fileBytes <= s.fileLimit || s.memoryBytes >= s.fileBytes/2 {
		return nil
	}
	return s.compactLocked()
}

func (s *JSONLTelemetrySink) persistenceLoop(flushInterval, syncInterval time.Duration) {
	flushTicker := time.NewTicker(flushInterval)
	syncTicker := time.NewTicker(syncInterval)
	defer func() {
		flushTicker.Stop()
		syncTicker.Stop()
		close(s.done)
	}()
	for {
		select {
		case <-flushTicker.C:
			s.flush(false)
		case <-syncTicker.C:
			s.flush(true)
		case <-s.stop:
			return
		}
	}
}

func (s *JSONLTelemetrySink) flush(syncDisk bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.flushLocked(syncDisk)
}

func (s *JSONLTelemetrySink) flushLocked(syncDisk bool) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	if err := s.writer.Flush(); err != nil {
		s.writeErr = fmt.Errorf("flush telemetry records: %w", err)
		return s.writeErr
	}
	if syncDisk {
		if err := s.file.Sync(); err != nil {
			s.writeErr = fmt.Errorf("sync telemetry records: %w", err)
			return s.writeErr
		}
	}
	return nil
}

func (s *JSONLTelemetrySink) TelemetryRecords(since time.Time) []TelemetryRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	records := make([]TelemetryRecord, 0, len(s.records))
	for _, stored := range s.records {
		if !stored.record.Event.OccurredAt.Before(since) {
			records = append(records, stored.record)
		}
	}
	return records
}

func (s *JSONLTelemetrySink) pruneLocked(cutoff time.Time) {
	kept := s.records[:0]
	for _, stored := range s.records {
		if retentionTime(stored.record).Before(cutoff) {
			s.memoryBytes -= stored.bytes
			continue
		}
		kept = append(kept, stored)
	}
	s.records = kept
}

// dropOldestOverBudgetLocked enforces the in-memory byte budget by discarding
// the oldest-received records first. This — not the retention window — is what
// keeps an anonymous telemetry flood from growing broker memory without bound.
func (s *JSONLTelemetrySink) dropOldestOverBudgetLocked() {
	drop := 0
	for drop < len(s.records) && s.memoryBytes > s.memoryLimit {
		s.memoryBytes -= s.records[drop].bytes
		drop++
	}
	if drop == 0 {
		return
	}
	s.records = append(s.records[:0], s.records[drop:]...)
}

// compactLocked rewrites the JSONL file to contain only the records still
// retained in memory and swaps the live handle to the rewritten file. Without
// this the append-only file grows without bound.
func (s *JSONLTelemetrySink) compactLocked() error {
	if err := s.flushLocked(false); err != nil {
		return err
	}
	dir, base := filepath.Split(s.path)
	temp, err := os.CreateTemp(dir, base+".compact-*")
	if err != nil {
		return fmt.Errorf("create compaction file: %w", err)
	}
	tempPath := temp.Name()
	discard := func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}

	writer := bufio.NewWriter(temp)
	var written int64
	for _, stored := range s.records {
		line, err := json.Marshal(stored.record)
		if err == nil {
			line = append(line, '\n')
			_, err = writer.Write(line)
		}
		if err != nil {
			discard()
			return fmt.Errorf("write compacted telemetry: %w", err)
		}
		written += int64(len(line))
	}
	if err := writer.Flush(); err != nil {
		discard()
		return fmt.Errorf("flush compacted telemetry: %w", err)
	}
	if err := temp.Sync(); err != nil {
		discard()
		return fmt.Errorf("sync compacted telemetry: %w", err)
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		discard()
		return fmt.Errorf("swap compacted telemetry file: %w", err)
	}

	// The rename unlinked the old file; keep appending through the handle of
	// the freshly written one.
	if _, err := temp.Seek(0, io.SeekEnd); err != nil {
		_ = temp.Close()
		return fmt.Errorf("seek compacted telemetry file: %w", err)
	}
	old := s.file
	s.file = temp
	s.writer = bufio.NewWriter(temp)
	s.fileBytes = written
	_ = old.Close()
	return nil
}

func telemetryHandler(sink TelemetrySink, relayMetrics RelayStore, clientIP *clientIPResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sink == nil {
			writeError(w, http.StatusServiceUnavailable, "telemetry is not configured")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxTelemetryBodyBytes)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var batch telemetryBatch
		if err := decoder.Decode(&batch); err != nil {
			writeError(w, http.StatusBadRequest, "invalid telemetry JSON")
			return
		}
		if len(batch.Events) == 0 || len(batch.Events) > maxTelemetryEvents {
			writeError(w, http.StatusBadRequest, "events must contain between 1 and 200 items")
			return
		}

		now := time.Now().UTC()
		sourceIP := clientIP.clientIP(r)
		records := make([]TelemetryRecord, 0, len(batch.Events))
		for _, event := range batch.Events {
			if err := validateTelemetryEvent(event, now); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			records = append(records, TelemetryRecord{ReceivedAt: now, SourceIP: sourceIP, Event: event})
		}
		if err := sink.WriteTelemetry(r.Context(), records); err != nil {
			slog.Error("could not store telemetry", "records", len(records), "error", err)
			writeError(w, http.StatusInternalServerError, "could not store telemetry")
			return
		}
		if relayMetrics != nil {
			if err := relayMetrics.RecordRelayTelemetry(r.Context(), records, now); err != nil {
				slog.Error("could not store relay metrics", "records", len(records), "error", err)
				writeError(w, http.StatusServiceUnavailable, "could not store relay metrics")
				return
			}
		}
		writeJSON(w, http.StatusAccepted, map[string]int{"accepted": len(records)})
	}
}

func validateTelemetryEvent(event TelemetryEvent, now time.Time) error {
	switch {
	case event.SchemaVersion != 1:
		return errors.New("schema_version must be 1")
	case strings.TrimSpace(event.EventID) == "" || len(event.EventID) > 128:
		return errors.New("event_id is required and must be at most 128 characters")
	case strings.TrimSpace(event.Event) == "" || len(event.Event) > 64:
		return errors.New("event is required and must be at most 64 characters")
	case strings.TrimSpace(event.ClientID) == "" || len(event.ClientID) > 128:
		return errors.New("client_id is required and must be at most 128 characters")
	case strings.TrimSpace(event.SessionID) == "" || len(event.SessionID) > 128:
		return errors.New("session_id is required and must be at most 128 characters")
	case event.OccurredAt.IsZero():
		return errors.New("occurred_at is required")
	case event.OccurredAt.After(now.Add(maxTelemetryFutureSkew)):
		return errors.New("occurred_at must not be in the future")
	case len(event.RelayID) > 128:
		return errors.New("relay_id must be at most 128 characters")
	case len(event.Application) > maxTelemetryValueBytes:
		return errors.New("application_package must be at most 256 characters")
	case len(event.DestinationIP) > maxTelemetryKeyBytes:
		return errors.New("destination_ip must be at most 64 characters")
	case len(event.Protocol) > maxTelemetryKeyBytes:
		return errors.New("protocol must be at most 64 characters")
	case len(event.Attributes) > 32 || len(event.Measurements) > 32:
		return errors.New("telemetry maps may contain at most 32 entries")
	}
	for key, value := range event.Attributes {
		if len(key) > maxTelemetryKeyBytes || len(value) > maxTelemetryValueBytes {
			return errors.New("attribute keys are limited to 64 characters and values to 256")
		}
	}
	for key := range event.Measurements {
		if len(key) > maxTelemetryKeyBytes {
			return errors.New("measurement keys are limited to 64 characters")
		}
	}
	return nil
}

// clientSeenDeduper suppresses repeat client_seen records for the same client
// session inside a short window.
type clientSeenDeduper struct {
	window     time.Duration
	maxEntries int
	mu         sync.Mutex
	seen       map[string]time.Time
}

func newClientSeenDeduper(window time.Duration, maxEntries int) *clientSeenDeduper {
	return &clientSeenDeduper{window: window, maxEntries: maxEntries, seen: make(map[string]time.Time)}
}

func (d *clientSeenDeduper) shouldRecord(clientID, sessionID string, now time.Time) bool {
	key := clientID + "\x00" + sessionID
	d.mu.Lock()
	defer d.mu.Unlock()
	if last, ok := d.seen[key]; ok && now.Sub(last) < d.window {
		return false
	}
	if len(d.seen) >= d.maxEntries {
		for seenKey, seenAt := range d.seen {
			if now.Sub(seenAt) >= d.window {
				delete(d.seen, seenKey)
			}
		}
	}
	// When the table is full of live entries the record is written rather than
	// suppressed: over-recording under pressure beats hiding active clients.
	if len(d.seen) < d.maxEntries {
		d.seen[key] = now
	}
	return true
}

func recordClientSeen(r *http.Request, sink TelemetrySink, clientIP *clientIPResolver, dedup *clientSeenDeduper) {
	clientID := strings.TrimSpace(r.Header.Get("X-OpenRung-Client-ID"))
	sessionID := strings.TrimSpace(r.Header.Get("X-OpenRung-Session-ID"))
	if sink == nil || clientID == "" || sessionID == "" || len(clientID) > 128 || len(sessionID) > 128 {
		return
	}
	now := time.Now().UTC()
	if dedup != nil && !dedup.shouldRecord(clientID, sessionID, now) {
		return
	}
	event := TelemetryEvent{
		SchemaVersion: 1,
		EventID:       "client-seen-" + sessionID,
		Event:         "client_seen",
		OccurredAt:    now,
		ClientID:      clientID,
		SessionID:     sessionID,
		Attributes: map[string]string{
			"app_version": r.Header.Get("X-OpenRung-App-Version"),
			"android_api": r.Header.Get("X-OpenRung-Android-API"),
		},
	}
	if err := sink.WriteTelemetry(r.Context(), []TelemetryRecord{{
		ReceivedAt: now,
		SourceIP:   clientIP.clientIP(r),
		Event:      event,
	}}); err != nil {
		slog.Error("could not store client-seen telemetry", "error", err)
	}
}
