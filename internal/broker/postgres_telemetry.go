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
	"sync/atomic"
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

	// telemetryDashboardWorkMem is the per-connection work_mem for the telemetry
	// pool, raised from the 4MB default so the dashboard's aggregation sorts stay
	// in memory instead of spilling to disk (see newPostgresTelemetrySink).
	telemetryDashboardWorkMem = "32MB"

	// telemetryRollupMinWindow is the shortest dashboard window served from
	// the session/event-count rollup tables; shorter windows (1h) aggregate
	// the window's events directly, where the exact per-event window edge is
	// cheap and the rollup's per-session membership would be coarse.
	telemetryRollupMinWindow = 24 * time.Hour

	// telemetryRollupRetention is how long the rollup tables outlive the raw
	// events (telemetryRetention), so the long dashboard windows keep counts
	// and trends after the partitions are dropped. Rollup sessions older than
	// telemetryRetention keep no IP-level fields (see PruneTelemetry).
	telemetryRollupRetention = 31 * 24 * time.Hour

	// The rollup backfill folds stored events into the rollup tables one
	// received_at hour at a time, backing off on errors.
	telemetryRollupBackfillChunk = time.Hour
	telemetryRollupBackfillRetry = 10 * time.Second

	// maxTelemetrySessionClientIDs caps the distinct client IDs one
	// telemetry_sessions row retains for the headline clients total.
	maxTelemetrySessionClientIDs = 32

	// telemetryPartitionPrefix is the fixed prefix of a daily partition's
	// table name; the suffix is the UTC day as YYYYMMDD. Both creating a
	// partition and finding aged-out ones to drop route through it, so the
	// naming scheme lives in exactly one place.
	telemetryPartitionPrefix = "telemetry_events_"
)

// telemetryRollupBackfillPause separates backfill chunks so the two production
// vCPUs keep serving relay polls while the rollup catches up. A variable so
// tests can run the backfill flat out.
var telemetryRollupBackfillPause = 250 * time.Millisecond

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
	relay_node_class text,
	payload jsonb NOT NULL
) PARTITION BY RANGE (received_at);

ALTER TABLE telemetry_events
	ADD COLUMN IF NOT EXISTS relay_node_class text;

CREATE INDEX IF NOT EXISTS telemetry_events_received_at_idx
	ON telemetry_events (received_at);

CREATE INDEX IF NOT EXISTS telemetry_events_session_occurred_idx
	ON telemetry_events (session_id, occurred_at);

-- application_connection events are never stored as telemetry_events rows —
-- only this hourly per-application rollup survives ingestion (see
-- telemetryAppConnectionEvent). It is what the dashboard's top-apps panel
-- reads, and it is tiny: one row per (hour, application) seen. connections is
-- the sum of each event's bounded connection_count (one for legacy events).
CREATE TABLE IF NOT EXISTS telemetry_app_counts (
	hour timestamptz NOT NULL,
	application text NOT NULL,
	connections bigint NOT NULL,
	PRIMARY KEY (hour, application)
);

-- The dashboard rollup (see telemetryRollupStatement): one row per session
-- carrying the sessions CTE's raw accumulation, and hourly event-grain counts
-- keyed by (hour, kind, k1, k2). Both are maintained in the same statement
-- that inserts a batch's events, and backfilled from stored partitions on the
-- first start after their introduction (telemetry_rollup_state tracks it).
-- telemetry_sessions deliberately carries no B-tree secondary index: every
-- heartbeat updates its row, and the lower fillfactor keeps those updates HOT
-- so the table does not bloat. The one exception is a BRIN index on
-- last_seen_at: the rollup keeps 31 days, so the short windows would otherwise
-- scan the whole table, and since Postgres 16 an update that changes only
-- BRIN-indexed columns stays HOT. Rows land roughly in last_seen_at order, so
-- the 24h window reads a small slice of the table; the long windows still
-- (cheaply) scan it all.
CREATE TABLE IF NOT EXISTS telemetry_sessions (
	session_id text PRIMARY KEY,
	client_id text NOT NULL,
	client_ids text[] NOT NULL,
	started_at timestamptz NOT NULL,
	last_occurred_at timestamptz NOT NULL,
	last_seen_at timestamptz NOT NULL,
	relay_id text NOT NULL,
	relay_node_class text NOT NULL,
	operating_system text NOT NULL,
	device_manufacturer text NOT NULL,
	device_model text NOT NULL,
	app_version text NOT NULL,
	country text NOT NULL,
	city text NOT NULL,
	organization text NOT NULL,
	asn text NOT NULL,
	isp text NOT NULL,
	failure_stage text NOT NULL,
	failure_reason text NOT NULL,
	failure_detail text NOT NULL,
	observed_client_ip text NOT NULL,
	reported_client_ip text NOT NULL,
	fallback_source_ip text NOT NULL,
	last_heartbeat_at timestamptz,
	running_duration_ms bigint NOT NULL,
	ended_duration_ms bigint NOT NULL,
	bytes_sent bigint NOT NULL,
	bytes_received bigint NOT NULL,
	attempted boolean NOT NULL,
	succeeded boolean NOT NULL,
	failed boolean NOT NULL,
	terminal boolean NOT NULL,
	-- Per-field (received_at, occurred_at) of the supplying event, which the
	-- incremental merge orders on (see telemetrySessionOrderColumns); NULL
	-- where the value is blank.
	first_seen_at timestamptz NOT NULL,
	first_occurred_at timestamptz NOT NULL,
	relay_at timestamptz,
	relay_occurred_at timestamptz,
	ended_at timestamptz,
	ended_occurred_at timestamptz,
	operating_system_at timestamptz,
	operating_system_occurred_at timestamptz,
	device_manufacturer_at timestamptz,
	device_manufacturer_occurred_at timestamptz,
	device_model_at timestamptz,
	device_model_occurred_at timestamptz,
	app_version_at timestamptz,
	app_version_occurred_at timestamptz,
	country_at timestamptz,
	country_occurred_at timestamptz,
	city_at timestamptz,
	city_occurred_at timestamptz,
	organization_at timestamptz,
	organization_occurred_at timestamptz,
	asn_at timestamptz,
	asn_occurred_at timestamptz,
	isp_at timestamptz,
	isp_occurred_at timestamptz,
	failure_stage_at timestamptz,
	failure_stage_occurred_at timestamptz,
	failure_reason_at timestamptz,
	failure_reason_occurred_at timestamptz,
	failure_detail_at timestamptz,
	failure_detail_occurred_at timestamptz,
	observed_client_ip_at timestamptz,
	observed_client_ip_occurred_at timestamptz,
	reported_client_ip_at timestamptz,
	reported_client_ip_occurred_at timestamptz,
	fallback_source_ip_at timestamptz,
	fallback_source_ip_occurred_at timestamptz
) WITH (fillfactor = 80);

CREATE INDEX IF NOT EXISTS telemetry_sessions_last_seen_brin
	ON telemetry_sessions USING brin (last_seen_at) WITH (autosummarize = on);

CREATE TABLE IF NOT EXISTS telemetry_event_counts (
	hour timestamptz NOT NULL,
	kind text NOT NULL,
	k1 text NOT NULL,
	k2 text NOT NULL,
	n bigint NOT NULL,
	s1 bigint NOT NULL,
	s2 bigint NOT NULL,
	last_at timestamptz,
	v text,
	v_at timestamptz,
	v_occurred_at timestamptz,
	PRIMARY KEY (hour, kind, k1, k2)
);

CREATE TABLE IF NOT EXISTS telemetry_rollup_state (
	key text PRIMARY KEY,
	value timestamptz NOT NULL
);
`

const telemetryInsertColumns = `received_at, source_ip, event_id, event, occurred_at, client_id, session_id, relay_id, relay_node_class, payload`

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
	// pendingAppCounts buffers the hourly application-connection rollup between
	// flushes (see telemetryAppConnectionEvent); flush upserts it into
	// telemetry_app_counts. The telemetryAppRollup entry cap bounds it while
	// Postgres is unreachable, the analog of the pending byte cap for records.
	pendingAppCounts telemetryAppRollup
	// partitions caches the daily partitions this process has already ensured
	// so steady-state flushes skip the DDL round-trip.
	partitions map[string]struct{}
	closed     bool

	// rollupReady reports that telemetry_sessions / telemetry_event_counts
	// cover every stored event, i.e. the one-off backfill has completed (this
	// process or an earlier one ran it to the end); until then the dashboard
	// aggregates every window from events. backfillDone is closed at that
	// point; backfillExited when the backfill goroutine has returned.
	rollupReady    atomic.Bool
	backfillDone   chan struct{}
	backfillExited chan struct{}

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
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse telemetry database URL: %w", err)
	}
	// The dashboard aggregation sorts a lot: the sessions CTE runs ~15
	// array_agg(... ORDER BY ...) per session group and the window's CTEs
	// materialize, both of which spill to disk at the stock 4MB work_mem and
	// dominate the multi-second query time. Raise it pool-wide — the insert
	// path never sorts, so it consumes none of this, and dashboard concurrency
	// is tiny (one admin, a 30s refresh), so the extra memory ceiling is safe
	// on the small production box.
	config.ConnConfig.RuntimeParams["work_mem"] = telemetryDashboardWorkMem
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open telemetry database: %w", err)
	}
	sink := &PostgresTelemetrySink{
		pool:           pool,
		now:            now,
		pendingLimit:   maxTelemetryPendingBytes,
		partitions:     make(map[string]struct{}),
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
		backfillDone:   make(chan struct{}),
		backfillExited: make(chan struct{}),
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
	complete, err := sink.initRollupBackfill(ctx)
	if err != nil {
		pool.Close()
		return nil, err
	}
	if complete {
		sink.markRollupReady()
		close(sink.backfillExited)
	} else {
		go sink.backfillLoop()
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
	now := s.now().UTC()
	dropped := 0
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("telemetry storage is closed")
	}
	evictedBefore := s.pendingAppCounts.evictedEntries
	appCounts := make(telemetryAppConnectionBatchCounter)
	for _, record := range records {
		if record.Event.Event == telemetryAppConnectionEvent {
			// Rolled up, never inserted as a row — see telemetryAppConnectionEvent.
			if !s.pendingAppCounts.add(
				telemetryAppRollupHour(record, now),
				record.Event.Application,
				appCounts.take(record.Event),
			) {
				dropped++
			}
			continue
		}
		size := telemetryRecordSize(record)
		s.pending = append(s.pending, storedTelemetryRecord{record: record, bytes: size})
		s.pendingBytes += size
	}
	shouldFlush := len(s.pending) >= postgresTelemetryFlushThreshold
	evicted := s.pendingAppCounts.evictedEntries - evictedBefore
	s.mu.Unlock()
	if dropped > 0 {
		slog.Warn("dropped application-connection counts over rollup entry cap", "dropped", dropped)
	}
	if evicted > 0 {
		slog.Warn("evicted oldest application-connection rollup entries to preserve current counts", "evicted", evicted, "store", "postgres-buffer")
	}

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
		SELECT received_at, COALESCE(host(source_ip), ''), event_id, event, occurred_at, client_id, session_id,
			COALESCE(relay_id, ''), COALESCE(relay_node_class, ''), payload
		FROM telemetry_events
		WHERE received_at > $1 AND occurred_at >= $2
			AND event <> 'application_connection'
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
		&record.RelayNodeClass,
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

// flush inserts the pending buffer and upserts the pending app-count rollup.
// On failure each is put back (records at the front of the buffer, capped by
// pendingLimit; counts re-merged into the rollup, capped by its entry limit)
// so the next flush retries it — a chunk that had already been written before
// a later chunk failed is retried too and may duplicate rows or double-count
// (its rollup included: the rollup rides in the chunk's insert statement),
// which the store tolerates by design (no unique constraint spans partitions,
// and the counts are dashboard-grade aggregates).
func (s *PostgresTelemetrySink) flush() error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.mu.Lock()
	batch, batchBytes := s.pending, s.pendingBytes
	s.pending, s.pendingBytes = nil, 0
	counts := s.pendingAppCounts
	s.pendingAppCounts = telemetryAppRollup{}
	s.mu.Unlock()
	if len(batch) == 0 && counts.entries == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), postgresTelemetryFlushTimeout)
	defer cancel()
	var errs []error
	if len(batch) > 0 {
		if err := s.insertBatch(ctx, batch); err != nil {
			s.mu.Lock()
			s.pending = append(batch, s.pending...)
			s.pendingBytes += batchBytes
			dropped := s.dropOldestPendingOverCapLocked()
			s.mu.Unlock()
			if dropped > 0 {
				slog.Warn("dropped buffered telemetry records over pending cap", "dropped", dropped)
			}
			errs = append(errs, err)
		}
	}
	if counts.entries > 0 {
		if err := s.upsertAppCounts(ctx, counts); err != nil {
			s.mu.Lock()
			dropped := 0
			evictedBefore := s.pendingAppCounts.evictedEntries
			for hour, apps := range counts.hours {
				for application, count := range apps {
					if !s.pendingAppCounts.add(hour, application, count) {
						dropped++
					}
				}
			}
			evicted := s.pendingAppCounts.evictedEntries - evictedBefore
			s.mu.Unlock()
			if dropped > 0 {
				slog.Warn("dropped application-connection counts over rollup entry cap", "dropped", dropped)
			}
			if evicted > 0 {
				slog.Warn("evicted oldest application-connection rollup entries while restoring a failed flush", "evicted", evicted, "store", "postgres-buffer")
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// upsertAppCounts adds the flushed rollup into telemetry_app_counts, chunked
// like record inserts so a giant rollup cannot approach the parameter cap.
func (s *PostgresTelemetrySink) upsertAppCounts(ctx context.Context, counts telemetryAppRollup) error {
	type appCountRow struct {
		hour        time.Time
		application string
		connections int64
	}
	rows := make([]appCountRow, 0, counts.entries)
	for hour, apps := range counts.hours {
		for application, count := range apps {
			rows = append(rows, appCountRow{hour: hour, application: application, connections: count})
		}
	}
	for start := 0; start < len(rows); start += postgresTelemetryInsertChunk {
		chunk := rows[start:min(start+postgresTelemetryInsertChunk, len(rows))]
		var b strings.Builder
		b.WriteString(`INSERT INTO telemetry_app_counts (hour, application, connections) VALUES `)
		args := make([]any, 0, len(chunk)*3)
		for i, row := range chunk {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "($%d,$%d,$%d)", i*3+1, i*3+2, i*3+3)
			args = append(args, row.hour, row.application, row.connections)
		}
		b.WriteString(` ON CONFLICT (hour, application) DO UPDATE SET connections = telemetry_app_counts.connections + EXCLUDED.connections`)
		if _, err := s.pool.Exec(ctx, b.String(), args...); err != nil {
			return fmt.Errorf("upsert telemetry app counts: %w", err)
		}
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
		args := make([]any, 0, len(chunk)*10)
		for _, stored := range chunk {
			args = append(args, telemetryInsertArgs(stored.record, now)...)
		}
		// One statement inserts the chunk and folds it into the dashboard
		// rollup, so the events and their rollup land (or fail) together.
		if _, err := s.pool.Exec(ctx, telemetryRollupInsertStatement(len(chunk)), args...); err != nil {
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
		for column := 0; column < 10; column++ {
			if column > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "$%d", row*10+column+1)
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
	var relayNodeClass any
	if record.RelayNodeClass != "" {
		relayNodeClass = record.RelayNodeClass
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
		relayNodeClass,
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
// never lose data they would query. The dashboard rollup is kept for the
// longer telemetryRollupRetention, minus the IP-level fields of every session
// older than telemetryRetention.
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
	// The app-count rollup ages out with the same cutoff as the partitions its
	// events would have landed in; hour-truncating mirrors the in-memory
	// rollup's prune so an hour bucket straddling the cutoff survives.
	if _, err := s.pool.Exec(ctx, `DELETE FROM telemetry_app_counts WHERE hour < $1`, cutoff.UTC().Truncate(time.Hour)); err != nil {
		return dropped, fmt.Errorf("prune telemetry app counts: %w", err)
	}
	// The dashboard rollup ages out the same way at its own, longer cutoff:
	// event-grain hours like the app counts, and a session once its latest
	// event is older than the cutoff (no window can still include it).
	rollupCutoff := now.UTC().Add(-telemetryRollupRetention)
	if _, err := s.pool.Exec(ctx, `DELETE FROM telemetry_event_counts WHERE hour < $1`, rollupCutoff.Truncate(time.Hour)); err != nil {
		return dropped, fmt.Errorf("prune telemetry event counts: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM telemetry_sessions WHERE last_seen_at < $1`, rollupCutoff); err != nil {
		return dropped, fmt.Errorf("prune telemetry sessions: %w", err)
	}
	// Sessions past the raw-event retention keep their counts but not their
	// IP-level fields, so per-client network detail is held no longer than the
	// events it came from.
	if _, err := s.pool.Exec(ctx, telemetrySessionBlankIPStatement, cutoff); err != nil {
		return dropped, fmt.Errorf("blank aged telemetry session IPs: %w", err)
	}
	return dropped, nil
}

// expiredPartitions lists the telemetry partitions whose day is fully older
// than cutoff. It enumerates the catalog rather than the in-process cache so
// partitions left behind by an earlier broker process are collected too.
func (s *PostgresTelemetrySink) expiredPartitions(ctx context.Context, cutoff time.Time) ([]string, error) {
	names, err := s.listPartitions(ctx)
	if err != nil {
		return nil, err
	}
	var expired []string
	for _, name := range names {
		if day, ok := telemetryPartitionNameDay(name); ok && !day.Add(24*time.Hour).After(cutoff) {
			expired = append(expired, name)
		}
	}
	return expired, nil
}

// listPartitions names every child table of telemetry_events in the catalog.
func (s *PostgresTelemetrySink) listPartitions(ctx context.Context) ([]string, error) {
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

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan telemetry partition: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// useRollup reports whether a dashboard window is served from the rollup
// tables: long enough to want it, and backfilled so it covers the window.
func (s *PostgresTelemetrySink) useRollup(window time.Duration) bool {
	return window >= telemetryRollupMinWindow && s.rollupReady.Load()
}

func (s *PostgresTelemetrySink) markRollupReady() {
	if s.rollupReady.CompareAndSwap(false, true) {
		close(s.backfillDone)
	}
}

// initRollupBackfill records, once per database, the range of stored events
// the rollup tables must be backfilled from: everything received before this
// start (`backfill_until`), walked upwards from the oldest partition's day
// (`backfill_done_through`). Rows this process inserts from now on are rolled
// up as they land, so the two ranges never overlap. A later start finds the
// rows already present and resumes (or, when done_through has reached until,
// reports completion at once); a fresh database with no partitions completes
// immediately. The state is deliberately never cleared, so a redeploy does
// not re-fold events that are already in the rollup.
func (s *PostgresTelemetrySink) initRollupBackfill(ctx context.Context) (complete bool, err error) {
	now := s.now().UTC()
	names, err := s.listPartitions(ctx)
	if err != nil {
		return false, err
	}
	from := now
	for _, name := range names {
		if day, ok := telemetryPartitionNameDay(name); ok && day.Before(from) {
			from = day
		}
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO telemetry_rollup_state (key, value)
		VALUES ('backfill_until', $1), ('backfill_done_through', $2)
		ON CONFLICT (key) DO NOTHING
	`, now, from); err != nil {
		return false, fmt.Errorf("initialise telemetry rollup state: %w", err)
	}
	var until, through time.Time
	if err := s.pool.QueryRow(ctx, `
		SELECT u.value, d.value
		FROM telemetry_rollup_state u, telemetry_rollup_state d
		WHERE u.key = 'backfill_until' AND d.key = 'backfill_done_through'
	`).Scan(&until, &through); err != nil {
		return false, fmt.Errorf("read telemetry rollup state: %w", err)
	}
	return !through.Before(until), nil
}

// backfillLoop drives rollupBackfillChunk until the watermark reaches the
// end, then switches the dashboard's long windows over to the rollup.
func (s *PostgresTelemetrySink) backfillLoop() {
	defer close(s.backfillExited)
	slog.Info("telemetry rollup backfill started")
	for {
		done, err := s.rollupBackfillChunk()
		wait := telemetryRollupBackfillPause
		switch {
		case err != nil:
			slog.Error("could not backfill telemetry rollup", "error", err)
			wait = telemetryRollupBackfillRetry
		case done:
			slog.Info("telemetry rollup backfill complete")
			s.markRollupReady()
			return
		}
		select {
		case <-time.After(wait):
		case <-s.stop:
			return
		}
	}
}

// rollupBackfillChunk folds the next hour of stored events into the rollup in
// one short transaction. The watermark row is locked for the transaction, so
// two brokers sharing a database take turns and never fold the same hour
// twice; the chunk statement and the watermark advance commit together, so a
// crash between them re-runs nothing. It reports done once the watermark has
// reached the end of the backfill range.
func (s *PostgresTelemetrySink) rollupBackfillChunk() (done bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresTelemetryQueryTimeout)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin telemetry rollup backfill: %w", err)
	}
	defer tx.Rollback(ctx)

	var until, through time.Time
	if err := tx.QueryRow(ctx, `
		SELECT u.value, d.value
		FROM telemetry_rollup_state u, telemetry_rollup_state d
		WHERE u.key = 'backfill_until' AND d.key = 'backfill_done_through'
		FOR UPDATE OF d
	`).Scan(&until, &through); err != nil {
		return false, fmt.Errorf("read telemetry rollup state: %w", err)
	}
	if !through.Before(until) {
		return true, nil
	}
	end := through.Add(telemetryRollupBackfillChunk)
	if end.After(until) {
		end = until
	}
	if _, err := tx.Exec(ctx, telemetryRollupBackfillStatement, through, end); err != nil {
		return false, fmt.Errorf("backfill telemetry rollup %s..%s: %w", through.Format(time.RFC3339), end.Format(time.RFC3339), err)
	}
	if _, err := tx.Exec(ctx, `UPDATE telemetry_rollup_state SET value = $1 WHERE key = 'backfill_done_through'`, end); err != nil {
		return false, fmt.Errorf("advance telemetry rollup backfill: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit telemetry rollup backfill: %w", err)
	}
	return !end.Before(until), nil
}

func (s *PostgresTelemetrySink) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
		<-s.backfillExited
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.closeErr = s.flush()
		s.pool.Close()
	})
	return s.closeErr
}
