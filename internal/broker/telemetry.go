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
	ReceivedAt     time.Time      `json:"received_at"`
	SourceIP       string         `json:"source_ip"`
	RelayNodeClass string         `json:"relay_node_class,omitempty"`
	Event          TelemetryEvent `json:"event"`
}

type TelemetrySink interface {
	WriteTelemetry(context.Context, []TelemetryRecord) error
}

type TelemetryReader interface {
	TelemetryRecords(time.Time) []TelemetryRecord
}

// TelemetryPruner is implemented by telemetry stores whose retention is a
// distinct reclamation step the broker's maintenance loop drives on its tick.
// The Postgres store drops daily partitions that have aged out of the window
// and returns their names; the JSONL sink prunes on write and does not
// implement this.
type TelemetryPruner interface {
	PruneTelemetry(now time.Time) ([]string, error)
}

// storedTelemetryRecord pairs a record with its encoded JSONL size so the
// memory and file budgets can be enforced without re-marshalling.
type storedTelemetryRecord struct {
	record TelemetryRecord
	bytes  int64
}

// application_connection events are one row per tunnelled flow (with a
// per-package fan-out on Android) — at production volume they were ~95% of all
// telemetry rows while feeding exactly one dashboard panel, and their payload
// pairs the client's IP with every destination it visited. No sink stores them
// as records: every sink folds them into an hourly per-application count and
// discards the rest of the event, so the destination/client-IP browsing log
// never reaches disk. The dashboard's top-apps panel reads these rollups.
const telemetryAppConnectionEvent = "application_connection"

// maxTelemetryAppRollupEntries bounds the distinct (hour, application) pairs a
// rollup holds in memory so a flood of fabricated package names cannot grow
// broker memory without bound. When the cap is full, a count from a newer hour
// evicts the oldest complete hour bucket; this preserves the most recent data
// instead of starving every new hour until an old bucket reaches retention.
const maxTelemetryAppRollupEntries = 10000

// telemetryAppRollupHour is the bucket an application_connection event counts
// into: the server-assigned receipt hour (falling back like retentionTime, so
// a bucket can never be client-controlled).
func telemetryAppRollupHour(record TelemetryRecord, now time.Time) time.Time {
	if at := retentionTime(record); !at.IsZero() {
		return at.UTC().Truncate(time.Hour)
	}
	return now.UTC().Truncate(time.Hour)
}

// telemetryAppRollup accumulates hourly application-connection counts. None of
// its methods lock — the owning sink's lock guards all access.
type telemetryAppRollup struct {
	hours          map[time.Time]map[string]int64
	entries        int
	evictedEntries int64
}

// add counts one bucket increment. Existing pairs always increment. At the
// entry cap, a newer hour evicts the oldest complete bucket to make room; add
// reports false only when the cap is saturated entirely by the incoming hour
// or newer hours, as can happen during a fabricated-package flood.
func (r *telemetryAppRollup) add(hour time.Time, application string, count int64) bool {
	if application == "" || count <= 0 {
		return true
	}
	if r.hours == nil {
		r.hours = make(map[time.Time]map[string]int64)
	}
	if apps := r.hours[hour]; apps != nil {
		if _, ok := apps[application]; ok {
			apps[application] += count
			return true
		}
	}
	for r.entries >= maxTelemetryAppRollupEntries {
		if !r.evictOldestHourBefore(hour) {
			return false
		}
	}
	apps := r.hours[hour]
	if apps == nil {
		apps = make(map[string]int64)
		r.hours[hour] = apps
	}
	r.entries++
	apps[application] += count
	return true
}

// evictOldestHourBefore removes the oldest complete bucket strictly before
// hour. Keeping eviction hour-granular makes the rollup's degraded behavior
// predictable under cardinality pressure and guarantees current data wins over
// older data without ever exceeding the global memory bound.
func (r *telemetryAppRollup) evictOldestHourBefore(hour time.Time) bool {
	var oldest time.Time
	found := false
	for candidate := range r.hours {
		if !candidate.Before(hour) || (found && !candidate.Before(oldest)) {
			continue
		}
		oldest = candidate
		found = true
	}
	if !found {
		return false
	}
	evicted := len(r.hours[oldest])
	r.entries -= evicted
	r.evictedEntries += int64(evicted)
	delete(r.hours, oldest)
	return true
}

// prune drops whole hour buckets that have aged out of the retention window,
// mirroring the Postgres store's DELETE on its counts table.
func (r *telemetryAppRollup) prune(cutoff time.Time) {
	cutoffHour := cutoff.UTC().Truncate(time.Hour)
	for hour, apps := range r.hours {
		if hour.Before(cutoffHour) {
			r.entries -= len(apps)
			delete(r.hours, hour)
		}
	}
}

// countsIn sums the buckets the dashboard window touches: every hour from the
// truncated window start through now. The window edge is hour-granular by
// design — the SQL path filters its counts table the same way.
func (r *telemetryAppRollup) countsIn(now time.Time, window time.Duration) map[string]int {
	startHour := now.Add(-window).UTC().Truncate(time.Hour)
	counts := make(map[string]int)
	for hour, apps := range r.hours {
		if hour.Before(startHour) || hour.After(now) {
			continue
		}
		for application, count := range apps {
			counts[application] += int(count)
		}
	}
	return counts
}

// telemetryRetained is the bounded in-memory record set that serves the
// dashboard's TelemetryReader queries; it is shared by every sink that keeps
// dashboard reads in memory. None of its methods lock — the owning sink's
// lock guards all access.
type telemetryRetained struct {
	records     []storedTelemetryRecord
	memoryBytes int64
	memoryLimit int64
	// compactionShifts counts records slid to the front of the slice by
	// dropOldestOverBudget. Over-budget loads must keep this O(number of
	// records) — the per-line trim it replaced made it O(records²) and hung
	// startup on a large file. Tests assert the linear bound.
	compactionShifts int64
}

func (r *telemetryRetained) append(record TelemetryRecord, size int64) {
	r.records = append(r.records, storedTelemetryRecord{record: record, bytes: size})
	r.memoryBytes += size
}

func (r *telemetryRetained) prune(cutoff time.Time) {
	kept := r.records[:0]
	for _, stored := range r.records {
		if retentionTime(stored.record).Before(cutoff) {
			r.memoryBytes -= stored.bytes
			continue
		}
		kept = append(kept, stored)
	}
	r.records = kept
}

// dropOldestOverBudget enforces the in-memory byte budget by discarding the
// oldest-received records first. This — not the retention window — is what
// keeps an anonymous telemetry flood from growing broker memory without bound.
func (r *telemetryRetained) dropOldestOverBudget() {
	drop := 0
	for drop < len(r.records) && r.memoryBytes > r.memoryLimit {
		r.memoryBytes -= r.records[drop].bytes
		drop++
	}
	if drop == 0 {
		return
	}
	r.compactionShifts += int64(len(r.records) - drop)
	r.records = append(r.records[:0], r.records[drop:]...)
}

func (r *telemetryRetained) recordsSince(since time.Time) []TelemetryRecord {
	records := make([]TelemetryRecord, 0, len(r.records))
	for _, stored := range r.records {
		if !stored.record.Event.OccurredAt.Before(since) {
			records = append(records, stored.record)
		}
	}
	return records
}

type JSONLTelemetrySink struct {
	mu     sync.RWMutex
	path   string
	file   *os.File
	writer *bufio.Writer
	telemetryRetained
	appRollup telemetryAppRollup
	fileBytes int64
	fileLimit int64
	now       func() time.Time
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
	writeErr  error
	closed    bool
}

func NewJSONLTelemetrySink(path string) (*JSONLTelemetrySink, error) {
	return newJSONLTelemetrySink(path, time.Now)
}

func newJSONLTelemetrySink(path string, now func() time.Time) (*JSONLTelemetrySink, error) {
	return newJSONLTelemetrySinkWithIntervals(path, now, telemetryFlushInterval, telemetrySyncInterval)
}

func newJSONLTelemetrySinkWithIntervals(path string, now func() time.Time, flushInterval, syncInterval time.Duration) (*JSONLTelemetrySink, error) {
	return newJSONLTelemetrySinkWithLimits(path, now, flushInterval, syncInterval, maxTelemetryMemoryBytes, maxTelemetryFileBytes)
}

// newJSONLTelemetrySinkWithLimits is the full constructor; the byte budgets are
// parameters so tests can exercise the load- and write-time trimming paths
// without materialising a multi-megabyte file.
func newJSONLTelemetrySinkWithLimits(path string, now func() time.Time, flushInterval, syncInterval time.Duration, memoryLimit, fileLimit int64) (*JSONLTelemetrySink, error) {
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
		telemetryRetained: telemetryRetained{memoryLimit: memoryLimit}, fileLimit: fileLimit,
	}
	needsPrivacyCompaction, err := sink.loadRecent()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	sink.writer = bufio.NewWriter(file)
	if needsPrivacyCompaction {
		err = sink.compactLocked()
	} else {
		err = sink.maybeCompactLocked()
	}
	if err != nil {
		_ = sink.file.Close()
		return nil, fmt.Errorf("compact telemetry file: %w", err)
	}
	go sink.persistenceLoop(flushInterval, syncInterval)
	return sink, nil
}

func (s *JSONLTelemetrySink) loadRecent() (bool, error) {
	info, err := s.file.Stat()
	if err != nil {
		return false, fmt.Errorf("stat telemetry file: %w", err)
	}
	s.fileBytes = info.Size()

	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return false, fmt.Errorf("seek telemetry file: %w", err)
	}
	cutoff := s.now().UTC().Add(-telemetryRetention)
	scanner := bufio.NewScanner(s.file)
	scanner.Buffer(make([]byte, 64<<10), maxTelemetryBodyBytes*2)
	line := 0
	needsPrivacyCompaction := false
	for scanner.Scan() {
		line++
		var record TelemetryRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return false, fmt.Errorf("decode telemetry record on line %d: %w", line, err)
		}
		// Legacy files may hold raw application_connection rows from before the
		// rollup existed. Skip every such row, including one already outside
		// retention, and force a startup compaction so its browsing metadata is
		// physically removed even when the file is below the normal size limit.
		if record.Event.Event == telemetryAppConnectionEvent {
			needsPrivacyCompaction = true
			continue
		}
		if retentionTime(record).Before(cutoff) {
			continue
		}
		size := int64(len(scanner.Bytes()) + 1)
		s.append(record, size)
		// Keep memory bounded during the scan, but trim only after overshooting the
		// budget by 2×, not on every line. dropOldestOverBudget re-copies the
		// surviving slice, so calling it per line made loading an oversized file
		// O(n²) — a 169 MB file hung broker startup for minutes. Amortising the trim
		// over a 2× overshoot keeps the whole load O(n) (each record is copied O(1)
		// times) while memory still peaks at ~2× the budget instead of the whole
		// file. The final trim below restores the steady-state ≤ budget invariant.
		if s.memoryBytes > 2*s.memoryLimit {
			s.dropOldestOverBudget()
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("read telemetry file: %w", err)
	}
	s.dropOldestOverBudget()
	_, err = s.file.Seek(0, io.SeekEnd)
	return needsPrivacyCompaction, err
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

	now := s.now().UTC()
	cutoff := now.Add(-telemetryRetention)
	// Free expired app buckets before accepting this batch so stale entries can
	// never make the cap reject the first count after a quiet period.
	s.appRollup.prune(cutoff)
	droppedAppCounts := 0
	evictedAppCountsBefore := s.appRollup.evictedEntries
	for _, record := range records {
		if record.Event.Event == telemetryAppConnectionEvent {
			// Rolled up, never persisted — see telemetryAppConnectionEvent. The
			// in-memory rollup is this sink's only copy, so top-apps counts
			// restart empty; the Postgres store persists its rollup instead.
			if !s.appRollup.add(telemetryAppRollupHour(record, now), record.Event.Application, 1) {
				droppedAppCounts++
			}
			continue
		}
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
		s.append(record, size)
		s.fileBytes += size
	}
	s.prune(cutoff)
	// Programmatic callers can supply records with an old ReceivedAt even though
	// the HTTP handler always stamps the current time. Remove any such bucket
	// added by this batch as well as pruning before it for capacity.
	s.appRollup.prune(cutoff)
	s.dropOldestOverBudget()
	if err := s.maybeCompactLocked(); err != nil {
		s.writeErr = fmt.Errorf("compact telemetry file: %w", err)
		return s.writeErr
	}
	if droppedAppCounts > 0 {
		slog.Warn("dropped application-connection counts over rollup entry cap", "dropped", droppedAppCounts, "store", "jsonl")
	}
	if evicted := s.appRollup.evictedEntries - evictedAppCountsBefore; evicted > 0 {
		slog.Warn("evicted oldest application-connection rollup entries to preserve current counts", "evicted", evicted, "store", "jsonl")
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
	return s.recordsSince(since)
}

// AppConnectionCounts serves the dashboard's top-apps panel from the in-memory
// hourly rollup (see telemetryAppConnectionEvent).
func (s *JSONLTelemetrySink) AppConnectionCounts(now time.Time, window time.Duration) map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.appRollup.countsIn(now, window)
}

// compactLocked rewrites the JSONL file to contain only the records still
// retained in memory and swaps the live handle to the rewritten file. Without
// this the append-only file grows without bound.
func (s *JSONLTelemetrySink) compactLocked() error {
	if err := s.flushLocked(false); err != nil {
		return err
	}
	dir, base := filepath.Dir(s.path), filepath.Base(s.path)
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
		if err := attestTelemetryRelayNodeClasses(r.Context(), relayMetrics, records, now); err != nil {
			slog.Error("could not attest telemetry relay classes", "records", len(records), "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not resolve telemetry relay classes")
			return
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

// attestTelemetryRelayNodeClasses snapshots each referenced relay's active,
// broker-attested class onto the server-owned telemetry envelope. The client
// never supplies this field. Retaining it with the event keeps historical
// dashboard attribution intact after the descriptor's short lease expires.
func attestTelemetryRelayNodeClasses(ctx context.Context, store RelayStore, records []TelemetryRecord, now time.Time) error {
	if store == nil {
		return nil
	}
	seen := make(map[string]struct{})
	ids := make([]string, 0)
	for _, record := range records {
		id := record.Event.RelayID
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	classes, err := store.RelayNodeClasses(ctx, ids, now)
	if err != nil {
		return err
	}
	for i := range records {
		records[i].RelayNodeClass = classes[records[i].Event.RelayID]
	}
	return nil
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
