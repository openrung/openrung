package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// postgresTelemetryFlushThreshold flushes the write buffer as soon as it
	// holds one full HTTP batch, so the 5s ticker only has to sweep up the
	// trickle of single records (e.g. client_seen pings from relay-list polls).
	postgresTelemetryFlushThreshold = maxTelemetryEvents

	// postgresTelemetryInsertChunk bounds the rows per multi-row INSERT; at 9
	// parameters per row this stays far below Postgres's 65535-parameter cap.
	postgresTelemetryInsertChunk = 500

	// maxTelemetryPendingBytes bounds records buffered while Postgres is
	// unreachable; the oldest unwritten records are dropped beyond it.
	maxTelemetryPendingBytes = 16 << 20

	postgresTelemetryFlushTimeout = 15 * time.Second
	postgresTelemetryQueryTimeout = 30 * time.Second

	// telemetryPartitionPrefix is the fixed prefix of a daily partition's
	// table name; the suffix is the UTC day as YYYYMMDD. Both creating a
	// partition and finding aged-out ones to drop route through it, so the
	// naming scheme lives in exactly one place.
	telemetryPartitionPrefix = "telemetry_events_"
)

// PostgresTelemetrySink drops whole aged-out daily partitions rather than
// pruning on write, so it satisfies TelemetryPruner for the maintenance loop.
var _ TelemetryPruner = (*PostgresTelemetrySink)(nil)

// The envelope every query filters or sorts on lives in real columns; the
// flexible remainder (attributes, measurements, destination, app identity,
// schema_version) rides in one jsonb payload. Daily partitions on received_at
// make retention a cheap DROP TABLE instead of a bulk DELETE (see
// PruneTelemetry); partitions themselves are created on demand (see
// ensurePartition), since CREATE TABLE cannot be templated here. Indexes are
// defined on the parent so every partition inherits them. Deliberately NO GIN
// index on payload — the production box is small and nothing queries into the
// payload yet.
const postgresTelemetrySchema = `
CREATE TABLE IF NOT EXISTS telemetry_events (
	received_at timestamptz NOT NULL,
	source_ip inet,
	event_id text NOT NULL,
	event text NOT NULL,
	occurred_at timestamptz NOT NULL,
	client_id text NOT NULL,
	session_id text NOT NULL,
	relay_id text,
	payload jsonb NOT NULL
) PARTITION BY RANGE (received_at);

CREATE INDEX IF NOT EXISTS telemetry_events_received_at_idx
	ON telemetry_events (received_at);

CREATE INDEX IF NOT EXISTS telemetry_events_session_occurred_idx
	ON telemetry_events (session_id, occurred_at);
`

const telemetryInsertColumns = `received_at, source_ip, event_id, event, occurred_at, client_id, session_id, relay_id, payload`

// telemetryEventPayload is the jsonb remainder of a TelemetryEvent — every
// field that is not an envelope column on telemetry_events.
type telemetryEventPayload struct {
	SchemaVersion   int               `json:"schema_version"`
	Application     string            `json:"application_package,omitempty"`
	ApplicationID   int               `json:"application_uid,omitempty"`
	DestinationIP   string            `json:"destination_ip,omitempty"`
	DestinationPort int               `json:"destination_port,omitempty"`
	Protocol        string            `json:"protocol,omitempty"`
	Attributes      map[string]string `json:"attributes,omitempty"`
	Measurements    map[string]int64  `json:"measurements,omitempty"`
}

// PostgresTelemetrySink persists telemetry events to a partitioned Postgres
// table. Writes are buffered and inserted in batches (flushed every
// telemetryFlushInterval, or immediately at a full HTTP batch). Reads happen
// in Postgres too — TelemetryQuerier aggregation for the dashboard and a
// short-window SELECT for the operational status log — so broker memory never
// scales with telemetry volume and startup reads no old events.
type PostgresTelemetrySink struct {
	pool *pgxpool.Pool
	now  func() time.Time

	mu           sync.RWMutex
	pending      []storedTelemetryRecord
	pendingBytes int64
	pendingLimit int64
	// partitions caches the daily partitions this process has already ensured
	// so steady-state flushes skip the DDL round-trip.
	partitions map[string]struct{}
	closed     bool

	// flushMu serialises flushes so a ticker flush and a threshold flush
	// cannot reorder or interleave batches.
	flushMu   sync.Mutex
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func NewPostgresTelemetrySink(ctx context.Context, databaseURL string) (*PostgresTelemetrySink, error) {
	return newPostgresTelemetrySink(ctx, databaseURL, time.Now, telemetryFlushInterval)
}

// newPostgresTelemetrySink is the full constructor; the flush interval is a
// parameter so tests can keep the ticker out of the way and flush explicitly.
func newPostgresTelemetrySink(ctx context.Context, databaseURL string, now func() time.Time, flushInterval time.Duration) (*PostgresTelemetrySink, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("telemetry database URL is required")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open telemetry database: %w", err)
	}
	sink := &PostgresTelemetrySink{
		pool:         pool,
		now:          now,
		pendingLimit: maxTelemetryPendingBytes,
		partitions:   make(map[string]struct{}),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping telemetry database: %w", err)
	}
	if _, err := pool.Exec(ctx, postgresTelemetrySchema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate telemetry database: %w", err)
	}
	// Pre-create today's and tomorrow's partitions so the first write after
	// startup — and the first one after a midnight rollover — never pays (or
	// races on) partition DDL.
	today := now().UTC()
	for _, day := range []time.Time{today, today.Add(24 * time.Hour)} {
		if err := sink.ensurePartition(ctx, day); err != nil {
			pool.Close()
			return nil, err
		}
	}
	go sink.flushLoop(flushInterval)
	return sink, nil
}

// telemetryRecordSize is the record's encoded JSON size, used to bound the
// unflushed write buffer.
func telemetryRecordSize(record TelemetryRecord) int64 {
	line, err := json.Marshal(record)
	if err != nil {
		return int64(len(record.SourceIP) + len(record.Event.EventID))
	}
	return int64(len(line) + 1)
}

func (s *PostgresTelemetrySink) WriteTelemetry(ctx context.Context, records []TelemetryRecord) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("telemetry storage is closed")
	}
	for _, record := range records {
		size := telemetryRecordSize(record)
		s.pending = append(s.pending, storedTelemetryRecord{record: record, bytes: size})
		s.pendingBytes += size
	}
	shouldFlush := len(s.pending) >= postgresTelemetryFlushThreshold
	s.mu.Unlock()

	// The insert itself is asynchronous (buffered like the JSONL sink's
	// bufio writer): a failed flush is retried by the ticker, so the handler
	// acks batches without waiting on Postgres.
	if shouldFlush {
		if err := s.flush(); err != nil {
			slog.Error("could not flush telemetry to postgres", "error", err)
		}
	}
	return nil
}

// TelemetryRecords satisfies TelemetryReader for the periodic operational
// status log, which reads a short (minutes-wide) activity window; result size
// scales with that window, not with stored history. The dashboard does NOT go
// through this — it uses the aggregated TelemetryQuerier methods. Reads are
// best-effort like the JSONL sink's: on query failure it logs and reports no
// activity rather than failing the caller.
func (s *PostgresTelemetrySink) TelemetryRecords(since time.Time) []TelemetryRecord {
	if err := s.flush(); err != nil {
		slog.Error("could not flush telemetry before read", "error", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresTelemetryQueryTimeout)
	defer cancel()

	// received_at prunes partitions; the extra hour mirrors
	// maxTelemetryFutureSkew so no event the occurred_at filter keeps is lost.
	rows, err := s.pool.Query(ctx, `
		SELECT received_at, COALESCE(host(source_ip), ''), event_id, event, occurred_at, client_id, session_id, COALESCE(relay_id, ''), payload
		FROM telemetry_events
		WHERE received_at > $1 AND occurred_at >= $2
		ORDER BY received_at
	`, since.Add(-maxTelemetryFutureSkew), since)
	if err != nil {
		slog.Error("could not read telemetry records", "error", err)
		return nil
	}
	defer rows.Close()

	var records []TelemetryRecord
	for rows.Next() {
		record, err := scanTelemetryRecord(rows)
		if err != nil {
			slog.Error("could not scan telemetry record", "error", err)
			return nil
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		slog.Error("could not read telemetry records", "error", err)
		return nil
	}
	return records
}

func scanTelemetryRecord(row pgx.Row) (TelemetryRecord, error) {
	var record TelemetryRecord
	var payload []byte
	if err := row.Scan(
		&record.ReceivedAt,
		&record.SourceIP,
		&record.Event.EventID,
		&record.Event.Event,
		&record.Event.OccurredAt,
		&record.Event.ClientID,
		&record.Event.SessionID,
		&record.Event.RelayID,
		&payload,
	); err != nil {
		return TelemetryRecord{}, fmt.Errorf("scan telemetry event: %w", err)
	}
	var remainder telemetryEventPayload
	if err := json.Unmarshal(payload, &remainder); err != nil {
		return TelemetryRecord{}, fmt.Errorf("decode telemetry payload: %w", err)
	}
	record.ReceivedAt = record.ReceivedAt.UTC()
	record.Event.OccurredAt = record.Event.OccurredAt.UTC()
	record.Event.SchemaVersion = remainder.SchemaVersion
	record.Event.Application = remainder.Application
	record.Event.ApplicationID = remainder.ApplicationID
	record.Event.DestinationIP = remainder.DestinationIP
	record.Event.DestinationPort = remainder.DestinationPort
	record.Event.Protocol = remainder.Protocol
	record.Event.Attributes = remainder.Attributes
	record.Event.Measurements = remainder.Measurements
	return record, nil
}

func (s *PostgresTelemetrySink) flushLoop(flushInterval time.Duration) {
	ticker := time.NewTicker(flushInterval)
	defer func() {
		ticker.Stop()
		close(s.done)
	}()
	for {
		select {
		case <-ticker.C:
			if err := s.flush(); err != nil {
				slog.Error("could not flush telemetry to postgres", "error", err)
			}
		case <-s.stop:
			return
		}
	}
}

// flush inserts the pending buffer. On failure the batch is put back at the
// front of the buffer (capped by pendingLimit) so the next flush retries it —
// a chunk that had already been inserted before a later chunk failed is
// retried too and may duplicate rows, which the schema tolerates by design
// (no unique constraint spans partitions).
func (s *PostgresTelemetrySink) flush() error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.mu.Lock()
	batch, batchBytes := s.pending, s.pendingBytes
	s.pending, s.pendingBytes = nil, 0
	s.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), postgresTelemetryFlushTimeout)
	defer cancel()
	if err := s.insertBatch(ctx, batch); err != nil {
		s.mu.Lock()
		s.pending = append(batch, s.pending...)
		s.pendingBytes += batchBytes
		dropped := s.dropOldestPendingOverCapLocked()
		s.mu.Unlock()
		if dropped > 0 {
			slog.Warn("dropped buffered telemetry records over pending cap", "dropped", dropped)
		}
		return err
	}
	return nil
}

func (s *PostgresTelemetrySink) dropOldestPendingOverCapLocked() int {
	drop := 0
	for drop < len(s.pending) && s.pendingBytes > s.pendingLimit {
		s.pendingBytes -= s.pending[drop].bytes
		drop++
	}
	if drop == 0 {
		return 0
	}
	s.pending = append(s.pending[:0], s.pending[drop:]...)
	return drop
}

func (s *PostgresTelemetrySink) insertBatch(ctx context.Context, batch []storedTelemetryRecord) error {
	now := s.now().UTC()
	days := make(map[time.Time]struct{})
	for _, stored := range batch {
		days[telemetryPartitionDay(insertReceivedAt(stored.record, now))] = struct{}{}
	}
	for day := range days {
		if err := s.ensurePartition(ctx, day); err != nil {
			return err
		}
	}

	for start := 0; start < len(batch); start += postgresTelemetryInsertChunk {
		chunk := batch[start:min(start+postgresTelemetryInsertChunk, len(batch))]
		args := make([]any, 0, len(chunk)*9)
		for _, stored := range chunk {
			args = append(args, telemetryInsertArgs(stored.record, now)...)
		}
		if _, err := s.pool.Exec(ctx, telemetryInsertStatement(len(chunk)), args...); err != nil {
			return fmt.Errorf("insert telemetry events: %w", err)
		}
	}
	return nil
}

// insertReceivedAt is the value stored in the partition key: the
// server-assigned receipt time, falling back to the event time and finally
// the current time so a record can never miss every partition.
func insertReceivedAt(record TelemetryRecord, now time.Time) time.Time {
	if at := retentionTime(record); !at.IsZero() {
		return at.UTC()
	}
	return now
}

func telemetryInsertStatement(rows int) string {
	var b strings.Builder
	b.WriteString(`INSERT INTO telemetry_events (` + telemetryInsertColumns + `) VALUES `)
	for row := 0; row < rows; row++ {
		if row > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('(')
		for column := 0; column < 9; column++ {
			if column > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "$%d", row*9+column+1)
		}
		b.WriteByte(')')
	}
	return b.String()
}

func telemetryInsertArgs(record TelemetryRecord, now time.Time) []any {
	event := record.Event
	// The payload marshals maps, strings and ints only, so this cannot fail.
	payload, _ := json.Marshal(telemetryEventPayload{
		SchemaVersion:   event.SchemaVersion,
		Application:     event.Application,
		ApplicationID:   event.ApplicationID,
		DestinationIP:   event.DestinationIP,
		DestinationPort: event.DestinationPort,
		Protocol:        event.Protocol,
		Attributes:      event.Attributes,
		Measurements:    event.Measurements,
	})
	var relayID any
	if event.RelayID != "" {
		relayID = event.RelayID
	}
	return []any{
		insertReceivedAt(record, now),
		telemetrySourceIP(record.SourceIP),
		event.EventID,
		event.Event,
		event.OccurredAt.UTC(),
		event.ClientID,
		event.SessionID,
		relayID,
		payload,
	}
}

// telemetrySourceIP maps the resolver's source string onto the nullable inet
// column; anything unparseable is stored as NULL rather than failing a batch.
func telemetrySourceIP(source string) any {
	addr, err := netip.ParseAddr(strings.TrimSpace(source))
	if err != nil {
		return nil
	}
	return addr.WithZone("")
}

func telemetryPartitionDay(at time.Time) time.Time {
	return at.UTC().Truncate(24 * time.Hour)
}

func telemetryPartitionName(day time.Time) string {
	return telemetryPartitionPrefix + telemetryPartitionDay(day).Format("20060102")
}

// telemetryPartitionNameDay recovers the UTC day a partition covers from its
// table name, reporting false for any name that is not one of ours (so a
// hand-created or unrelated child table is never mistaken for a drop target).
func telemetryPartitionNameDay(name string) (time.Time, bool) {
	suffix, ok := strings.CutPrefix(name, telemetryPartitionPrefix)
	if !ok {
		return time.Time{}, false
	}
	day, err := time.ParseInLocation("20060102", suffix, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return day, true
}

func (s *PostgresTelemetrySink) ensurePartition(ctx context.Context, at time.Time) error {
	day := telemetryPartitionDay(at)
	name := telemetryPartitionName(day)

	s.mu.Lock()
	_, ensured := s.partitions[name]
	s.mu.Unlock()
	if ensured {
		return nil
	}

	// The identifier and bounds are derived from a time.Time, never from
	// client input, so string-building the DDL is safe (DDL cannot take bind
	// parameters).
	ddl := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF telemetry_events FOR VALUES FROM ('%s') TO ('%s')`,
		name,
		day.Format(time.RFC3339),
		day.Add(24*time.Hour).Format(time.RFC3339),
	)
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		// Two brokers racing the same CREATE can fail one of them even with
		// IF NOT EXISTS; the partition existing is all that matters.
		var exists *string
		if checkErr := s.pool.QueryRow(ctx, `SELECT to_regclass($1)::text`, name).Scan(&exists); checkErr != nil || exists == nil {
			return fmt.Errorf("create telemetry partition %s: %w", name, err)
		}
	}

	s.mu.Lock()
	s.partitions[name] = struct{}{}
	s.mu.Unlock()
	return nil
}

// PruneTelemetry drops the daily partitions whose entire received_at range has
// aged out of the retention window, reclaiming their storage with a cheap DROP
// TABLE rather than a bulk DELETE. It returns the names of the partitions it
// dropped. Today's and any partition still holding a row inside the window are
// left untouched, so the dashboard's 7-day view and the status-log reader
// never lose data they would query.
func (s *PostgresTelemetrySink) PruneTelemetry(now time.Time) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresTelemetryQueryTimeout)
	defer cancel()

	// A partition covers [day, day+24h). It is safe to drop once its upper
	// bound has reached the retention cutoff: every row it can hold is then
	// strictly older than the window anything ever reads.
	cutoff := now.UTC().Add(-telemetryRetention)
	expired, err := s.expiredPartitions(ctx, cutoff)
	if err != nil {
		return nil, err
	}

	var dropped []string
	for _, name := range expired {
		// name came from the catalog and matched the daily-partition pattern,
		// so interpolating it into the DDL is safe (DROP takes no parameters).
		if _, err := s.pool.Exec(ctx, `DROP TABLE IF EXISTS `+name); err != nil {
			return dropped, fmt.Errorf("drop telemetry partition %s: %w", name, err)
		}
		dropped = append(dropped, name)
		s.mu.Lock()
		delete(s.partitions, name)
		s.mu.Unlock()
	}
	return dropped, nil
}

// expiredPartitions lists the telemetry partitions whose day is fully older
// than cutoff. It enumerates the catalog rather than the in-process cache so
// partitions left behind by an earlier broker process are collected too.
func (s *PostgresTelemetrySink) expiredPartitions(ctx context.Context, cutoff time.Time) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.relname
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'telemetry_events'::regclass
	`)
	if err != nil {
		return nil, fmt.Errorf("list telemetry partitions: %w", err)
	}
	defer rows.Close()

	var expired []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan telemetry partition: %w", err)
		}
		if day, ok := telemetryPartitionNameDay(name); ok && !day.Add(24*time.Hour).After(cutoff) {
			expired = append(expired, name)
		}
	}
	return expired, rows.Err()
}

func (s *PostgresTelemetrySink) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.closeErr = s.flush()
		s.pool.Close()
	})
	return s.closeErr
}
