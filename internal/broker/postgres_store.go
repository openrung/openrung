package broker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"openrung/internal/relay"
)

const descriptorColumns = `
	id,
	public_host,
	public_port,
	protocol,
	client_id,
	reality_public_key,
	short_id,
	server_name,
	flow,
	exit_mode,
	max_sessions,
	max_mbps,
	volunteer_version,
	registered_at,
	last_heartbeat_at,
	expires_at,
	label,
	transport,
	punch_capable,
	punch_endpoint,
	city,
	country,
	country_code,
	latitude,
	longitude,
	exit_host,
	node_class,
	identity_public_key,
	lease_token
`

const postgresOperationTimeout = 5 * time.Second

const postgresSchema = `
CREATE TABLE IF NOT EXISTS relay_descriptors (
	id text PRIMARY KEY,
	public_host text NOT NULL,
	public_port integer NOT NULL CHECK (public_port BETWEEN 1 AND 65535),
	protocol text NOT NULL,
	client_id text NOT NULL,
	reality_public_key text NOT NULL,
	short_id text NOT NULL,
	server_name text NOT NULL,
	flow text NOT NULL,
	exit_mode text NOT NULL,
	max_sessions integer NOT NULL CHECK (max_sessions > 0),
	max_mbps integer NOT NULL CHECK (max_mbps > 0),
	volunteer_version text NOT NULL,
	registered_at timestamptz NOT NULL,
	last_heartbeat_at timestamptz NOT NULL,
	expires_at timestamptz NOT NULL,
	label text NOT NULL DEFAULT '',
	is_ipv6 boolean NOT NULL,
	transport text NOT NULL DEFAULT 'direct',
	punch_capable boolean NOT NULL DEFAULT false,
	punch_endpoint text NOT NULL DEFAULT '',
	city text NOT NULL DEFAULT '',
	country text NOT NULL DEFAULT '',
	country_code text NOT NULL DEFAULT '',
	latitude double precision NOT NULL DEFAULT 0,
	longitude double precision NOT NULL DEFAULT 0,
	exit_host text NOT NULL DEFAULT '',
	node_class text NOT NULL DEFAULT 'volunteer',
	identity_public_key text NOT NULL DEFAULT '',
	lease_token text NOT NULL DEFAULT '',
	attributes jsonb NOT NULL DEFAULT '{}'::jsonb,
	UNIQUE (public_host, public_port)
);

ALTER TABLE relay_descriptors
	ADD COLUMN IF NOT EXISTS attributes jsonb NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE relay_descriptors
	ADD COLUMN IF NOT EXISTS label text NOT NULL DEFAULT '';

ALTER TABLE relay_descriptors
	ADD COLUMN IF NOT EXISTS transport text NOT NULL DEFAULT 'direct';

ALTER TABLE relay_descriptors
	ADD COLUMN IF NOT EXISTS punch_capable boolean NOT NULL DEFAULT false;

ALTER TABLE relay_descriptors
	ADD COLUMN IF NOT EXISTS punch_endpoint text NOT NULL DEFAULT '';

ALTER TABLE relay_descriptors
	ADD COLUMN IF NOT EXISTS city text NOT NULL DEFAULT '';

ALTER TABLE relay_descriptors
	ADD COLUMN IF NOT EXISTS country text NOT NULL DEFAULT '';

ALTER TABLE relay_descriptors
	ADD COLUMN IF NOT EXISTS country_code text NOT NULL DEFAULT '';

ALTER TABLE relay_descriptors
	ADD COLUMN IF NOT EXISTS latitude double precision NOT NULL DEFAULT 0;

ALTER TABLE relay_descriptors
	ADD COLUMN IF NOT EXISTS longitude double precision NOT NULL DEFAULT 0;

ALTER TABLE relay_descriptors
	ADD COLUMN IF NOT EXISTS exit_host text NOT NULL DEFAULT '';

ALTER TABLE relay_descriptors
	ADD COLUMN IF NOT EXISTS node_class text NOT NULL DEFAULT 'volunteer';

ALTER TABLE relay_descriptors
	ADD COLUMN IF NOT EXISTS identity_public_key text NOT NULL DEFAULT '';

ALTER TABLE relay_descriptors
	ADD COLUMN IF NOT EXISTS lease_token text NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS relay_descriptors_active_idx
	ON relay_descriptors (expires_at DESC, last_heartbeat_at DESC);

CREATE TABLE IF NOT EXISTS relay_sessions (
	session_id text PRIMARY KEY,
	client_id text NOT NULL,
	relay_id text NOT NULL,
	last_heartbeat_at timestamptz NOT NULL,
	terminal_at timestamptz,
	updated_at timestamptz NOT NULL,
	attributes jsonb NOT NULL DEFAULT '{}'::jsonb
);

ALTER TABLE relay_sessions
	ADD COLUMN IF NOT EXISTS attributes jsonb NOT NULL DEFAULT '{}'::jsonb;

CREATE INDEX IF NOT EXISTS relay_sessions_active_by_relay_idx
	ON relay_sessions (relay_id, last_heartbeat_at DESC)
	WHERE terminal_at IS NULL;

CREATE TABLE IF NOT EXISTS relay_metrics (
	event_id text PRIMARY KEY,
	relay_id text NOT NULL,
	observed_at timestamptz NOT NULL,
	success_count integer NOT NULL DEFAULT 0,
	failure_count integer NOT NULL DEFAULT 0,
	tcp_ms bigint,
	tunnel_start_ms bigint,
	internet_probe_ms bigint,
	ttfb_ms bigint,
	download_mbps_milli bigint,
	speed_test_count integer NOT NULL DEFAULT 0,
	measurements jsonb NOT NULL DEFAULT '{}'::jsonb
);

ALTER TABLE relay_metrics
	ADD COLUMN IF NOT EXISTS measurements jsonb NOT NULL DEFAULT '{}'::jsonb;

CREATE INDEX IF NOT EXISTS relay_metrics_recent_by_relay_idx
	ON relay_metrics (relay_id, observed_at DESC);
`

type PostgresStore struct {
	pool        *pgxpool.Pool
	rankingMode RankingMode
}

func NewPostgresStore(ctx context.Context, databaseURL string, rankingMode RankingMode) (*PostgresStore, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("relay database URL is required")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open relay database: %w", err)
	}
	store := &PostgresStore{pool: pool, rankingMode: normalizeRankingMode(rankingMode)}
	if err := store.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := store.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

func (s *PostgresStore) Register(req relay.RegisterRequest, now time.Time, ttl time.Duration) (relay.Descriptor, error) {
	ctx, cancel := postgresOperationContext()
	defer cancel()

	// Verification lives in the store, not the handler, so no future caller
	// can register an identity-bearing request without the possession proof —
	// the derived ID is only ever handed to a registrant holding the private
	// key. Legacy requests (no identity fields) return a nil key and keep the
	// random mint below.
	identityKey, err := relay.VerifyIdentity(req, now)
	if err != nil {
		return relay.Descriptor{}, err
	}
	var id string
	if identityKey != nil {
		id = relay.DeriveRelayID(identityKey)
	} else if id, err = newRelayID(); err != nil {
		return relay.Descriptor{}, err
	}
	var leaseToken string
	if identityKey != nil {
		leaseToken, err = newRelayLeaseToken()
		if err != nil {
			return relay.Descriptor{}, err
		}
	}
	desc := relay.Descriptor{
		ID:                id,
		IdentityPublicKey: req.IdentityPublicKey,
		LeaseToken:        leaseToken,
		Label:             req.Label,
		NodeClass:         normalizeNodeClass(req.NodeClass),
		PublicHost:        req.PublicHost,
		PublicPort:        req.PublicPort,
		ExitHost:          req.ExitHost,
		Protocol:          req.Protocol,
		ClientID:          req.ClientID,
		RealityPublicKey:  req.RealityPublicKey,
		ShortID:           req.ShortID,
		ServerName:        req.ServerName,
		Flow:              req.Flow,
		ExitMode:          req.ExitMode,
		MaxSessions:       req.MaxSessions,
		MaxMbps:           req.MaxMbps,
		RelayVersion:      req.RelayVersion,
		Transport:         normalizeTransport(req.Transport),
		PunchCapable:      req.PunchCapable,
		PunchEndpoint:     req.PunchEndpoint,
		RegisteredAt:      now,
		LastHeartbeatAt:   now,
		ExpiresAt:         now.Add(ttl),
	}

	// Geo columns are deliberately absent from the INSERT: a re-registration
	// at the same host:port keeps its previously resolved location until the
	// handler's next successful UpdateGeo — except when the exit host changed
	// (a hub port reused by a different relay), where the upsert clears
	// the stale location so the handler resolves the new exit.
	//
	// The DO UPDATE WHERE guards a LIVE foundation endpoint: a foundation row
	// may only be overwritten by another foundation-class registration.
	// public_host:public_port is client-supplied and foundation endpoints are
	// public in the signed list, so without this guard an anonymous
	// registration could seize a foundation relay's row (new id, attacker's
	// keys, downgraded class) and knock the real relay into a re-registration
	// race. The handler already gates node_class=foundation behind the
	// foundation token, so "EXCLUDED.node_class = foundation" can only be true
	// for a token-authorized caller. The expires_at disjunct keeps this to
	// live rows — an already-expired foundation row (not yet pruned) is
	// reclaimable, matching the in-memory Store's ExpiresAt.After(now) guard.
	// A suppressed update returns no row (pgx.ErrNoRows), which we map to
	// ErrNodeClassForbidden.
	//
	// Identity registrations run inside a transaction so a relay moving to a
	// new endpoint atomically abandons its old row: the id is the PRIMARY KEY,
	// so without the DELETE the insert at the new endpoint would collide with
	// the relay's own previous row. The DELETE can only ever remove rows owned
	// by this identity (the id is derived from the key the registrant just
	// proved possession of), and a rollback — e.g. the foundation-endpoint
	// guard suppressing the upsert — preserves the old row untouched.
	querier := pgxQuerier(s.pool)
	if identityKey != nil {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return relay.Descriptor{}, fmt.Errorf("begin relay registration: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		// A stable identity re-registering from a new endpoint abandons its old
		// row. Tunnel relays hit this on most reconnects (the hub round-robins
		// their public port), and the upsert below can still be refused and
		// rolled back by the foundation-endpoint guard, so this is deliberately
		// silent — logging here would be both noisy and, on a rollback, false.
		if _, err := tx.Exec(ctx, `
			DELETE FROM relay_descriptors
			WHERE id = $1 AND (public_host <> $2 OR public_port <> $3)`,
			id, desc.PublicHost, desc.PublicPort); err != nil {
			return relay.Descriptor{}, fmt.Errorf("evict moved relay identity: %w", err)
		}
		querier = tx
	}
	desc, err = scanDescriptor(querier.QueryRow(ctx, `
		INSERT INTO relay_descriptors (
			id,
			public_host,
			public_port,
			protocol,
			client_id,
			reality_public_key,
			short_id,
			server_name,
			flow,
			exit_mode,
			max_sessions,
			max_mbps,
			volunteer_version,
			registered_at,
			last_heartbeat_at,
			expires_at,
			is_ipv6,
			label,
			transport,
			punch_capable,
			punch_endpoint,
			exit_host,
			node_class,
			identity_public_key,
			lease_token
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25
		)
		ON CONFLICT (public_host, public_port) DO UPDATE SET
			id = EXCLUDED.id,
			protocol = EXCLUDED.protocol,
			client_id = EXCLUDED.client_id,
			reality_public_key = EXCLUDED.reality_public_key,
			short_id = EXCLUDED.short_id,
			server_name = EXCLUDED.server_name,
			flow = EXCLUDED.flow,
			exit_mode = EXCLUDED.exit_mode,
			max_sessions = EXCLUDED.max_sessions,
			max_mbps = EXCLUDED.max_mbps,
			volunteer_version = EXCLUDED.volunteer_version,
			registered_at = EXCLUDED.registered_at,
			last_heartbeat_at = EXCLUDED.last_heartbeat_at,
			expires_at = EXCLUDED.expires_at,
			is_ipv6 = EXCLUDED.is_ipv6,
			label = EXCLUDED.label,
			transport = EXCLUDED.transport,
			punch_capable = EXCLUDED.punch_capable,
			punch_endpoint = EXCLUDED.punch_endpoint,
			city = CASE WHEN relay_descriptors.exit_host = EXCLUDED.exit_host THEN relay_descriptors.city ELSE '' END,
			country = CASE WHEN relay_descriptors.exit_host = EXCLUDED.exit_host THEN relay_descriptors.country ELSE '' END,
			country_code = CASE WHEN relay_descriptors.exit_host = EXCLUDED.exit_host THEN relay_descriptors.country_code ELSE '' END,
			latitude = CASE WHEN relay_descriptors.exit_host = EXCLUDED.exit_host THEN relay_descriptors.latitude ELSE 0 END,
			longitude = CASE WHEN relay_descriptors.exit_host = EXCLUDED.exit_host THEN relay_descriptors.longitude ELSE 0 END,
			exit_host = EXCLUDED.exit_host,
			node_class = EXCLUDED.node_class,
			identity_public_key = EXCLUDED.identity_public_key,
			lease_token = EXCLUDED.lease_token
		WHERE relay_descriptors.node_class <> $26
			OR EXCLUDED.node_class = $26
			OR relay_descriptors.expires_at <= EXCLUDED.registered_at
		RETURNING `+descriptorColumns,
		desc.ID,
		desc.PublicHost,
		desc.PublicPort,
		desc.Protocol,
		desc.ClientID,
		desc.RealityPublicKey,
		desc.ShortID,
		desc.ServerName,
		desc.Flow,
		desc.ExitMode,
		desc.MaxSessions,
		desc.MaxMbps,
		desc.RelayVersion,
		desc.RegisteredAt,
		desc.LastHeartbeatAt,
		desc.ExpiresAt,
		relay.IsIPv6Host(desc.PublicHost),
		desc.Label,
		desc.Transport,
		desc.PunchCapable,
		desc.PunchEndpoint,
		desc.ExitHost,
		desc.NodeClass,
		desc.IdentityPublicKey,
		desc.LeaseToken,
		relay.NodeClassFoundation,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		// The upsert matched an existing row but the WHERE suppressed the
		// update: the endpoint is held by a foundation relay and this
		// (non-foundation) registration may not take it over.
		return relay.Descriptor{}, ErrNodeClassForbidden
	}
	if err != nil {
		return relay.Descriptor{}, err
	}
	if tx, ok := querier.(pgx.Tx); ok {
		if err := tx.Commit(ctx); err != nil {
			return relay.Descriptor{}, fmt.Errorf("commit relay registration: %w", err)
		}
	}
	return desc, nil
}

// pgxQuerier is the intersection of pgxpool.Pool and pgx.Tx that Register
// needs, so the identity path can run its statements inside a transaction
// while the legacy path keeps hitting the pool directly.
type pgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *PostgresStore) Heartbeat(id, leaseToken, maxClass string, now time.Time, ttl time.Duration) (relay.Descriptor, error) {
	ctx, cancel := postgresOperationContext()
	defer cancel()

	// The class guard lives inside the UPDATE's WHERE so an unauthorized
	// heartbeat never extends the lease, not even transiently.
	desc, err := scanDescriptor(s.pool.QueryRow(ctx, `
		UPDATE relay_descriptors
		SET last_heartbeat_at = $3, expires_at = $4
		WHERE id = $1
			AND (identity_public_key = '' OR (lease_token <> '' AND lease_token = $2))
			AND (node_class <> $5 OR $6)
		RETURNING `+descriptorColumns,
		id,
		leaseToken,
		now,
		now.Add(ttl),
		relay.NodeClassFoundation,
		maxClass == relay.NodeClassFoundation,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		// No row updated: either the relay is gone or the guard blocked a
		// foundation row. Distinguish so the handler can answer 403 vs 404
		// (the 404 drives client re-registration and must stay accurate).
		return relay.Descriptor{}, heartbeatMissError(
			s.pool.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM relay_descriptors WHERE id = $1),
					EXISTS (SELECT 1 FROM relay_descriptors WHERE id = $1 AND node_class = $2)`,
				id, relay.NodeClassFoundation),
			maxClass,
		)
	}
	return desc, err
}

func heartbeatMissError(row pgx.Row, maxClass string) error {
	var exists, foundation bool
	if err := row.Scan(&exists, &foundation); err != nil {
		return err
	}
	if exists && foundation && maxClass != relay.NodeClassFoundation {
		return ErrNodeClassForbidden
	}
	return ErrRelayNotFound
}

func (s *PostgresStore) UpdateGeo(id, leaseToken string, geo relay.GeoLocation) error {
	ctx, cancel := postgresOperationContext()
	defer cancel()

	tag, err := s.pool.Exec(ctx, `
		UPDATE relay_descriptors
		SET city = $3, country = $4, country_code = $5, latitude = $6, longitude = $7
		WHERE id = $1
			AND (identity_public_key = '' OR (lease_token <> '' AND lease_token = $2))
	`, id, leaseToken, geo.City, geo.Country, geo.CountryCode, geo.Latitude, geo.Longitude)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrRelayNotFound
	}
	return nil
}

func (s *PostgresStore) List(now time.Time, limit int) ([]relay.Descriptor, error) {
	ctx, cancel := postgresOperationContext()
	defer cancel()

	rows, err := s.pool.Query(ctx, `SELECT `+descriptorColumns+` FROM relay_descriptors WHERE expires_at > $1`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var relays []relay.Descriptor
	for rows.Next() {
		desc, err := scanDescriptor(rows)
		if err != nil {
			return nil, err
		}
		relays = append(relays, desc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	snapshots, err := s.metricSnapshots(ctx, now)
	if err != nil {
		return nil, err
	}
	sortRelayCandidates(relays, snapshots, s.rankingMode)
	if limit > 0 && len(relays) > limit {
		return relays[:limit], nil
	}
	return relays, nil
}

func (s *PostgresStore) RelayNodeClasses(parent context.Context, ids []string, now time.Time) (map[string]string, error) {
	classes := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return classes, nil
	}

	ctx, cancel := context.WithTimeout(parent, postgresOperationTimeout)
	defer cancel()
	rows, err := s.pool.Query(ctx, `
		SELECT id, node_class
		FROM relay_descriptors
		WHERE id = ANY($1) AND expires_at > $2
	`, ids, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, nodeClass string
		if err := rows.Scan(&id, &nodeClass); err != nil {
			return nil, err
		}
		classes[id] = nodeClass
	}
	return classes, rows.Err()
}

func (s *PostgresStore) Stats(now time.Time) (StoreStats, error) {
	ctx, cancel := postgresOperationContext()
	defer cancel()

	var active int
	var capacity sql.NullInt64
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(max_sessions), 0)::bigint
		FROM relay_descriptors
		WHERE expires_at > $1
	`, now).Scan(&active, &capacity); err != nil {
		return StoreStats{}, err
	}
	return StoreStats{ActiveRelays: active, AdvertisedSessionCapacity: int(capacity.Int64)}, nil
}

func (s *PostgresStore) Prune(now time.Time) ([]relay.Descriptor, error) {
	ctx, cancel := postgresOperationContext()
	defer cancel()

	rows, err := s.pool.Query(ctx, `DELETE FROM relay_descriptors WHERE expires_at <= $1 RETURNING `+descriptorColumns, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var expired []relay.Descriptor
	for rows.Next() {
		desc, err := scanDescriptor(rows)
		if err != nil {
			return nil, err
		}
		expired = append(expired, desc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	cutoff := now.Add(-rankingWindow)
	if _, err := s.pool.Exec(ctx, `DELETE FROM relay_metrics WHERE observed_at < $1`, cutoff); err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM relay_sessions WHERE terminal_at IS NOT NULL AND terminal_at < $1`, cutoff); err != nil {
		return nil, err
	}
	return expired, nil
}

func (s *PostgresStore) RecordRelayTelemetry(ctx context.Context, records []TelemetryRecord, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for _, record := range records {
		if err := s.recordRelayTelemetry(ctx, tx, record, now); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping relay database: %w", err)
	}
	return nil
}

func (s *PostgresStore) Close() error {
	s.pool.Close()
	return nil
}

func (s *PostgresStore) migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, postgresSchema); err != nil {
		return fmt.Errorf("migrate relay database: %w", err)
	}
	return nil
}

func (s *PostgresStore) metricSnapshots(ctx context.Context, now time.Time) (map[string]RelayMetricsSnapshot, error) {
	snapshots := make(map[string]RelayMetricsSnapshot)
	activeRows, err := s.pool.Query(ctx, `
		SELECT relay_id, COUNT(*)::bigint
		FROM relay_sessions
		WHERE terminal_at IS NULL AND last_heartbeat_at > $1
		GROUP BY relay_id
	`, now.Add(-activeSessionTimeout))
	if err != nil {
		return nil, err
	}
	for activeRows.Next() {
		var relayID string
		var active int64
		if err := activeRows.Scan(&relayID, &active); err != nil {
			activeRows.Close()
			return nil, err
		}
		snapshot := snapshots[relayID]
		snapshot.ActiveSessions = int(active)
		snapshots[relayID] = snapshot
	}
	if err := activeRows.Err(); err != nil {
		activeRows.Close()
		return nil, err
	}
	activeRows.Close()

	metricRows, err := s.pool.Query(ctx, `
		SELECT
			relay_id,
			COALESCE(SUM(success_count), 0)::bigint,
			COALESCE(SUM(failure_count), 0)::bigint,
			COALESCE(SUM(tcp_ms), 0)::bigint,
			COUNT(tcp_ms)::bigint,
			COALESCE(SUM(tunnel_start_ms), 0)::bigint,
			COUNT(tunnel_start_ms)::bigint,
			COALESCE(SUM(internet_probe_ms), 0)::bigint,
			COUNT(internet_probe_ms)::bigint,
			COALESCE(SUM(ttfb_ms), 0)::bigint,
			COUNT(ttfb_ms)::bigint,
			COALESCE(SUM(speed_test_count), 0)::bigint,
			COALESCE(SUM(download_mbps_milli), 0)::bigint
		FROM relay_metrics
		WHERE observed_at >= $1 AND observed_at <= $2
		GROUP BY relay_id
	`, now.Add(-rankingWindow), now)
	if err != nil {
		return nil, err
	}
	defer metricRows.Close()
	for metricRows.Next() {
		var relayID string
		var successes, failures int64
		var tcpTotal, tcpCount, tunnelTotal, tunnelCount int64
		var internetTotal, internetCount, ttfbTotal, ttfbCount int64
		var speedTests, speedTotal int64
		if err := metricRows.Scan(
			&relayID,
			&successes,
			&failures,
			&tcpTotal,
			&tcpCount,
			&tunnelTotal,
			&tunnelCount,
			&internetTotal,
			&internetCount,
			&ttfbTotal,
			&ttfbCount,
			&speedTests,
			&speedTotal,
		); err != nil {
			return nil, err
		}
		snapshot := snapshots[relayID]
		snapshot.Successes = int(successes)
		snapshot.Failures = int(failures)
		snapshot.TCPMS = metricValue{total: float64(tcpTotal), count: int(tcpCount)}
		snapshot.TunnelStartMS = metricValue{total: float64(tunnelTotal), count: int(tunnelCount)}
		snapshot.InternetProbeMS = metricValue{total: float64(internetTotal), count: int(internetCount)}
		snapshot.TTFBMS = metricValue{total: float64(ttfbTotal), count: int(ttfbCount)}
		snapshot.SpeedTests = int(speedTests)
		snapshot.DownloadMbpsTotal = float64(speedTotal) / 1000
		snapshots[relayID] = snapshot
	}
	return snapshots, metricRows.Err()
}

func (s *PostgresStore) recordRelayTelemetry(ctx context.Context, tx pgx.Tx, record TelemetryRecord, now time.Time) error {
	event := record.Event
	observedAt := record.ReceivedAt
	if observedAt.IsZero() {
		observedAt = event.OccurredAt
	}
	if observedAt.IsZero() {
		observedAt = now
	}

	switch event.Event {
	case "session_heartbeat":
		if event.SessionID == "" || event.RelayID == "" {
			return nil
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO relay_sessions (session_id, client_id, relay_id, last_heartbeat_at, terminal_at, updated_at)
			VALUES ($1, $2, $3, $4, NULL, $5)
			ON CONFLICT (session_id) DO UPDATE SET
				client_id = EXCLUDED.client_id,
				relay_id = EXCLUDED.relay_id,
				last_heartbeat_at = GREATEST(relay_sessions.last_heartbeat_at, EXCLUDED.last_heartbeat_at),
				terminal_at = CASE
					WHEN relay_sessions.terminal_at IS NOT NULL
						AND relay_sessions.terminal_at >= EXCLUDED.last_heartbeat_at
					THEN relay_sessions.terminal_at
					ELSE NULL
				END,
				updated_at = EXCLUDED.updated_at
		`, event.SessionID, event.ClientID, event.RelayID, observedAt, now)
		return err
	case "connection_succeeded", "relay_failover":
		if event.RelayID == "" {
			return nil
		}
		return s.insertMetric(ctx, tx, event.EventID, event.RelayID, observedAt, true, false, event.Measurements)
	case "relay_attempt_failed":
		if event.RelayID == "" {
			return nil
		}
		return s.insertMetric(ctx, tx, event.EventID, event.RelayID, observedAt, false, true, event.Measurements)
	case "speed_test_completed":
		if event.RelayID == "" {
			return nil
		}
		return s.insertMetric(ctx, tx, event.EventID, event.RelayID, observedAt, false, false, event.Measurements)
	case "connection_ended", "tunnel_stopped", "connection_failed":
		return s.markSessionTerminal(ctx, tx, event, observedAt, now)
	default:
		return nil
	}
}

func (s *PostgresStore) insertMetric(ctx context.Context, tx pgx.Tx, eventID, relayID string, observedAt time.Time, success, failure bool, measurements map[string]int64) error {
	successCount, failureCount := 0, 0
	if success {
		successCount = 1
	}
	if failure {
		failureCount = 1
	}
	speedTestCount := 0
	if measurements["download_mbps_milli"] > 0 {
		speedTestCount = 1
	}

	_, err := tx.Exec(ctx, `
		INSERT INTO relay_metrics (
			event_id,
			relay_id,
			observed_at,
			success_count,
			failure_count,
			tcp_ms,
			tunnel_start_ms,
			internet_probe_ms,
			ttfb_ms,
			download_mbps_milli,
			speed_test_count
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (event_id) DO NOTHING
	`,
		eventID,
		relayID,
		observedAt,
		successCount,
		failureCount,
		positiveInt64(measurements["relay_tcp_ms"]),
		positiveInt64(measurements["tunnel_start_ms"]),
		positiveInt64(measurements["internet_probe_ms"]),
		positiveInt64(measurements["time_to_first_byte_ms"]),
		positiveInt64(measurements["download_mbps_milli"]),
		speedTestCount,
	)
	return err
}

func (s *PostgresStore) markSessionTerminal(ctx context.Context, tx pgx.Tx, event TelemetryEvent, observedAt, now time.Time) error {
	if event.SessionID == "" {
		return nil
	}
	if event.RelayID == "" {
		_, err := tx.Exec(ctx, `
			UPDATE relay_sessions
			SET terminal_at = CASE
					WHEN terminal_at IS NULL OR terminal_at < $2 THEN $2
					ELSE terminal_at
				END,
				updated_at = $3
			WHERE session_id = $1
		`, event.SessionID, observedAt, now)
		return err
	}

	_, err := tx.Exec(ctx, `
		INSERT INTO relay_sessions (session_id, client_id, relay_id, last_heartbeat_at, terminal_at, updated_at)
		VALUES ($1, $2, $3, $4, $4, $5)
		ON CONFLICT (session_id) DO UPDATE SET
			client_id = CASE WHEN EXCLUDED.client_id = '' THEN relay_sessions.client_id ELSE EXCLUDED.client_id END,
			relay_id = CASE WHEN EXCLUDED.relay_id = '' THEN relay_sessions.relay_id ELSE EXCLUDED.relay_id END,
			terminal_at = CASE
				WHEN relay_sessions.terminal_at IS NULL OR relay_sessions.terminal_at < EXCLUDED.terminal_at
				THEN EXCLUDED.terminal_at
				ELSE relay_sessions.terminal_at
			END,
			updated_at = EXCLUDED.updated_at
	`, event.SessionID, event.ClientID, event.RelayID, observedAt, now)
	return err
}

func scanDescriptor(row pgx.Row) (relay.Descriptor, error) {
	var desc relay.Descriptor
	err := row.Scan(
		&desc.ID,
		&desc.PublicHost,
		&desc.PublicPort,
		&desc.Protocol,
		&desc.ClientID,
		&desc.RealityPublicKey,
		&desc.ShortID,
		&desc.ServerName,
		&desc.Flow,
		&desc.ExitMode,
		&desc.MaxSessions,
		&desc.MaxMbps,
		&desc.RelayVersion,
		&desc.RegisteredAt,
		&desc.LastHeartbeatAt,
		&desc.ExpiresAt,
		&desc.Label,
		&desc.Transport,
		&desc.PunchCapable,
		&desc.PunchEndpoint,
		&desc.City,
		&desc.Country,
		&desc.CountryCode,
		&desc.Latitude,
		&desc.Longitude,
		&desc.ExitHost,
		&desc.NodeClass,
		&desc.IdentityPublicKey,
		&desc.LeaseToken,
	)
	return desc, err
}

func positiveInt64(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}

func postgresOperationContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), postgresOperationTimeout)
}
