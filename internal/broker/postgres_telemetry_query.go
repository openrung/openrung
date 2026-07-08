package broker

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// The dashboard queries below reproduce buildTelemetryOverview in SQL, bounded
// by the dashboard window so their cost tracks the window, not stored history.
// They share two CTEs: `events` extracts the payload fields the aggregator
// reads, and `sessions` groups them per session with the same semantics as
// sessionAccumulator. All queries take the same leading parameters:
//
//	$1 pruned-after  — received_at lower bound; occurred_at is what the
//	                   aggregation filters on, but received_at prunes daily
//	                   partitions. The caller widens it by
//	                   maxTelemetryFutureSkew so no event the occurred_at
//	                   filter would keep is lost to pruning.
//	$2 window start  — occurred_at lower bound (inclusive)
//	$3 now           — occurred_at upper bound (inclusive)
//	$4 active-after  — heartbeat threshold (now - activeSessionTimeout);
//	                   only session-level queries reference it.
//
// NULLIF mirrors the accumulator's plain `!= ""` checks; the btrim CASEs
// mirror firstNonEmpty, which trims for the emptiness test but keeps the
// original value.
const telemetryEventsCTE = `
events AS (
	SELECT
		received_at,
		occurred_at,
		event,
		client_id,
		session_id,
		COALESCE(relay_id, '') AS relay_id,
		host(source_ip) AS source_ip,
		NULLIF(payload->>'application_package', '') AS application,
		payload->'attributes'->>'failure_stage' AS failure_stage,
		COALESCE(
			NULLIF(payload->'attributes'->>'operating_system', ''),
			'iOS ' || NULLIF(payload->'attributes'->>'ios_version', ''),
			'Android (API ' || NULLIF(payload->'attributes'->>'android_api', '') || ')'
		) AS os_label,
		NULLIF(payload->'attributes'->>'device_manufacturer', '') AS device_manufacturer,
		NULLIF(payload->'attributes'->>'device_model', '') AS device_model,
		NULLIF(payload->'attributes'->>'app_version', '') AS app_version,
		CASE
			WHEN btrim(COALESCE(payload->'attributes'->>'country', '')) <> '' THEN payload->'attributes'->>'country'
			WHEN btrim(COALESCE(payload->'attributes'->>'country_code', '')) <> '' THEN payload->'attributes'->>'country_code'
		END AS country,
		NULLIF(payload->'attributes'->>'city', '') AS city,
		NULLIF(payload->'attributes'->>'organization', '') AS organization,
		NULLIF(payload->'attributes'->>'asn', '') AS asn,
		NULLIF(payload->'attributes'->>'isp', '') AS isp,
		NULLIF(payload->'attributes'->>'client_ip', '') AS reported_client_ip,
		(payload->'measurements'->>'session_duration_ms')::bigint AS session_duration_ms,
		(payload->'measurements'->>'bytes_sent')::bigint AS bytes_sent,
		(payload->'measurements'->>'bytes_received')::bigint AS bytes_received,
		(payload->'measurements'->>'download_mbps_milli')::bigint AS download_mbps_milli,
		(payload->'measurements'->>'time_to_first_byte_ms')::bigint AS ttfb_ms
	FROM telemetry_events
	WHERE received_at > $1 AND occurred_at >= $2 AND occurred_at <= $3
)`

// The `latest non-empty wins` aggregations order by (received_at, occurred_at)
// as the canonical event order. The in-memory accumulator iterates records in
// arrival order; rows from one uploaded batch share a received_at, so
// occurred_at breaks those ties. bytes_sent/bytes_received are cumulative per
// session, hence MAX; the GREATEST(..., 0) mirrors the accumulator never
// letting a negative report beat its zero initial value.
const telemetrySessionsCTE = `
sessions AS (
	SELECT
		session_id,
		(array_agg(client_id ORDER BY received_at, occurred_at))[1] AS client_id,
		MIN(occurred_at) AS started_at,
		MAX(received_at) AS last_seen_at,
		COALESCE((array_agg(relay_id ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE relay_id <> ''))[1], '') AS relay_id,
		COALESCE((array_agg(os_label ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE os_label IS NOT NULL))[1], '') AS operating_system,
		COALESCE((array_agg(device_manufacturer ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE device_manufacturer IS NOT NULL))[1], '') AS device_manufacturer,
		COALESCE((array_agg(device_model ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE device_model IS NOT NULL))[1], '') AS device_model,
		COALESCE((array_agg(app_version ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE app_version IS NOT NULL))[1], '') AS app_version,
		COALESCE((array_agg(country ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE country IS NOT NULL))[1], '') AS country,
		COALESCE((array_agg(city ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE city IS NOT NULL))[1], '') AS city,
		COALESCE((array_agg(organization ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE organization IS NOT NULL))[1], '') AS organization,
		COALESCE((array_agg(asn ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE asn IS NOT NULL))[1], '') AS asn,
		COALESCE((array_agg(isp ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE isp IS NOT NULL))[1], '') AS isp,
		COALESCE((array_agg(source_ip ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE event = 'client_seen' AND source_ip IS NOT NULL))[1], '') AS observed_client_ip,
		COALESCE((array_agg(reported_client_ip ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE reported_client_ip IS NOT NULL))[1], '') AS reported_client_ip,
		COALESCE((array_agg(source_ip ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE source_ip IS NOT NULL))[1], '') AS fallback_source_ip,
		MAX(received_at) FILTER (WHERE event = 'session_heartbeat') AS last_heartbeat_at,
		GREATEST(COALESCE(MAX(session_duration_ms) FILTER (WHERE event = 'session_heartbeat'), 0), 0) AS running_duration_ms,
		COALESCE((array_agg(COALESCE(session_duration_ms, 0) ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE event = 'connection_ended'))[1], 0) AS ended_duration_ms,
		GREATEST(COALESCE(MAX(bytes_sent), 0), 0) AS bytes_sent,
		GREATEST(COALESCE(MAX(bytes_received), 0), 0) AS bytes_received,
		bool_or(event = 'connection_attempted') AS attempted,
		bool_or(event = 'connection_succeeded') AS succeeded,
		bool_or(event = 'connection_failed') AS failed,
		bool_or(event IN ('connection_failed', 'connection_ended', 'tunnel_stopped')) AS terminal,
		MAX(received_at) FILTER (WHERE event = 'session_heartbeat') > $4 AND NOT bool_or(event IN ('connection_failed', 'connection_ended', 'tunnel_stopped')) AS active
	FROM events
	GROUP BY session_id
)`

// isp precedence mirrors firstNonEmpty(isp, organization, asn).
const telemetrySessionISPLabel = `
	CASE
		WHEN btrim(isp) <> '' THEN isp
		WHEN btrim(organization) <> '' THEN organization
		WHEN btrim(asn) <> '' THEN asn
		ELSE ''
	END`

const telemetryTotalsQuery = `WITH ` + telemetryEventsCTE + `, ` + telemetrySessionsCTE + `
SELECT
	(SELECT COUNT(DISTINCT client_id) FROM events) AS clients,
	COUNT(*) AS sessions,
	COUNT(*) FILTER (WHERE attempted) AS attempts,
	COUNT(*) FILTER (WHERE succeeded) AS successes,
	COUNT(*) FILTER (WHERE failed) AS failures,
	COUNT(DISTINCT client_id) FILTER (WHERE active) AS active_clients,
	COUNT(*) FILTER (WHERE active) AS active_sessions
FROM sessions`

const telemetryTrendQuery = `WITH ` + telemetryEventsCTE + `
SELECT
	date_trunc('hour', occurred_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS hour,
	COUNT(*) FILTER (WHERE event = 'connection_attempted') AS attempts,
	COUNT(*) FILTER (WHERE event = 'connection_succeeded') AS successes,
	COUNT(*) FILTER (WHERE event = 'connection_failed') AS failures
FROM events
WHERE event IN ('connection_attempted', 'connection_succeeded', 'connection_failed')
GROUP BY 1`

// Event-level count groups, discriminated by kind; the caller feeds each kind
// through sortedCounts (or the relay merge) so top-N selection and tiebreaks
// stay identical to the in-memory path.
const telemetryEventCountsQuery = `WITH ` + telemetryEventsCTE + `
SELECT 'top_applications' AS kind, application AS name, COUNT(*) AS count
FROM events WHERE application IS NOT NULL GROUP BY application
UNION ALL
SELECT 'failure_stages',
	CASE WHEN btrim(COALESCE(failure_stage, '')) <> '' THEN failure_stage ELSE 'unknown' END,
	COUNT(*)
FROM events WHERE event = 'connection_failed' GROUP BY 2
UNION ALL
SELECT 'relay_successes', relay_id, COUNT(*)
FROM events WHERE event = 'connection_succeeded' AND relay_id <> '' GROUP BY relay_id
UNION ALL
SELECT 'relay_failures', relay_id, COUNT(*)
FROM events WHERE event = 'relay_attempt_failed' AND relay_id <> '' GROUP BY relay_id`

const telemetrySessionCountsQuery = `WITH ` + telemetryEventsCTE + `, ` + telemetrySessionsCTE + `
SELECT 'top_countries' AS kind, country AS name, COUNT(*) AS count
FROM sessions WHERE btrim(country) <> '' GROUP BY country
UNION ALL
SELECT 'top_cities', city, COUNT(*) FROM sessions WHERE btrim(city) <> '' GROUP BY city
UNION ALL
SELECT 'top_isps', ` + telemetrySessionISPLabel + `, COUNT(*)
FROM sessions WHERE btrim(` + telemetrySessionISPLabel + `) <> '' GROUP BY 2
UNION ALL
SELECT 'active_by_relay', relay_id, COUNT(*) FROM sessions WHERE active AND btrim(relay_id) <> '' GROUP BY relay_id
UNION ALL
SELECT 'active_by_country', country, COUNT(*) FROM sessions WHERE active AND btrim(country) <> '' GROUP BY country
UNION ALL
SELECT 'active_by_city', city, COUNT(*) FROM sessions WHERE active AND btrim(city) <> '' GROUP BY city
UNION ALL
SELECT 'active_by_isp', ` + telemetrySessionISPLabel + `, COUNT(*)
FROM sessions WHERE active AND btrim(` + telemetrySessionISPLabel + `) <> '' GROUP BY 2
UNION ALL
SELECT 'active_by_os', operating_system, COUNT(*) FROM sessions WHERE active AND btrim(operating_system) <> '' GROUP BY operating_system`

const telemetrySpeedTestsQuery = `WITH ` + telemetryEventsCTE + `
SELECT
	relay_id,
	COUNT(*) AS tests,
	SUM(COALESCE(download_mbps_milli, 0)) AS mbps_milli,
	SUM(COALESCE(ttfb_ms, 0)) AS ttfb_ms
FROM events
WHERE event = 'speed_test_completed' AND relay_id <> ''
GROUP BY relay_id`

// The (last_seen_at DESC, session_id) order matches the in-memory sort so
// pages stay stable across requests when batched uploads share a received_at.
const telemetrySessionPageQuery = `WITH ` + telemetryEventsCTE + `, ` + telemetrySessionsCTE + `
SELECT
	session_id, client_id, started_at, last_seen_at, relay_id, operating_system,
	device_manufacturer, device_model, app_version, country, city, organization, asn, isp,
	observed_client_ip, reported_client_ip, fallback_source_ip,
	last_heartbeat_at, running_duration_ms, ended_duration_ms, bytes_sent, bytes_received,
	attempted, succeeded, failed, terminal
FROM sessions
ORDER BY last_seen_at DESC, session_id
LIMIT $5 OFFSET $6`

const telemetrySessionCountQuery = `WITH ` + telemetryEventsCTE + `, ` + telemetrySessionsCTE + `
SELECT COUNT(*) FROM sessions`

// telemetryWindowArgs is the shared parameter list documented on the CTEs.
// Queries that only touch the events CTE must take eventArgs — Postgres
// rejects bound parameters a statement never references.
func telemetryWindowArgs(now time.Time, window time.Duration) (eventArgs, sessionArgs []any) {
	start := now.Add(-window)
	sessionArgs = []any{start.Add(-maxTelemetryFutureSkew), start, now, now.Add(-activeSessionTimeout)}
	return sessionArgs[:3], sessionArgs
}

// TelemetryOverview implements TelemetryQuerier by aggregating the window in
// Postgres. Only per-group counts and the newest sessions travel back to Go,
// so response size tracks the diversity of the window, not its event count.
func (s *PostgresTelemetrySink) TelemetryOverview(now time.Time, window time.Duration) (telemetryOverview, error) {
	if err := s.flush(); err != nil {
		slog.Error("could not flush telemetry before read", "error", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresTelemetryQueryTimeout)
	defer cancel()
	eventArgs, sessionArgs := telemetryWindowArgs(now, window)

	overview := telemetryOverview{GeneratedAt: now, Window: window.String()}
	if err := s.pool.QueryRow(ctx, telemetryTotalsQuery, sessionArgs...).Scan(
		&overview.Totals.Clients,
		&overview.Totals.Sessions,
		&overview.Totals.Attempts,
		&overview.Totals.Successes,
		&overview.Totals.Failures,
		&overview.Totals.ActiveClients,
		&overview.Totals.ActiveSessions,
	); err != nil {
		return telemetryOverview{}, fmt.Errorf("query telemetry totals: %w", err)
	}
	if overview.Totals.Attempts > 0 {
		overview.Totals.SuccessRate = float64(overview.Totals.Successes) / float64(overview.Totals.Attempts)
	}

	trend, err := s.queryTelemetryTrend(ctx, eventArgs, now, window)
	if err != nil {
		return telemetryOverview{}, err
	}
	overview.Trend = trend

	eventCounts, err := s.queryTelemetryCounts(ctx, telemetryEventCountsQuery, eventArgs)
	if err != nil {
		return telemetryOverview{}, fmt.Errorf("query telemetry event counts: %w", err)
	}
	overview.TopApps = sortedCounts(eventCounts["top_applications"], 10)
	overview.FailureStages = sortedCounts(eventCounts["failure_stages"], 10)
	overview.TopRelays = topRelaySummaries(eventCounts["relay_successes"], eventCounts["relay_failures"])

	sessionCounts, err := s.queryTelemetryCounts(ctx, telemetrySessionCountsQuery, sessionArgs)
	if err != nil {
		return telemetryOverview{}, fmt.Errorf("query telemetry session counts: %w", err)
	}
	overview.TopCountries = sortedCounts(sessionCounts["top_countries"], 10)
	overview.TopCities = sortedCounts(sessionCounts["top_cities"], 10)
	overview.TopISPs = sortedCounts(sessionCounts["top_isps"], 10)
	overview.ActiveRelays = sortedCounts(sessionCounts["active_by_relay"], 10)
	overview.ActiveCountries = sortedCounts(sessionCounts["active_by_country"], 10)
	overview.ActiveCities = sortedCounts(sessionCounts["active_by_city"], 10)
	overview.ActiveISPs = sortedCounts(sessionCounts["active_by_isp"], 10)
	overview.ActiveOS = sortedCounts(sessionCounts["active_by_os"], 10)

	speedTests, err := s.queryTelemetrySpeedTests(ctx, eventArgs)
	if err != nil {
		return telemetryOverview{}, err
	}
	overview.SpeedTests = speedTests

	recent, err := s.queryTelemetrySessionPage(ctx, sessionArgs, now, overviewRecentSessions, 0)
	if err != nil {
		return telemetryOverview{}, err
	}
	// Match the in-memory path: no sessions marshals as null, not [].
	if len(recent) > 0 {
		overview.Recent = recent
	}
	return overview, nil
}

// TelemetrySessions implements TelemetryQuerier with LIMIT/OFFSET pagination.
func (s *PostgresTelemetrySink) TelemetrySessions(now time.Time, window time.Duration, offset, limit int) ([]sessionSummary, int, error) {
	if err := s.flush(); err != nil {
		slog.Error("could not flush telemetry before read", "error", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresTelemetryQueryTimeout)
	defer cancel()
	_, sessionArgs := telemetryWindowArgs(now, window)

	var total int
	if err := s.pool.QueryRow(ctx, telemetrySessionCountQuery, sessionArgs...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count telemetry sessions: %w", err)
	}
	page, err := s.queryTelemetrySessionPage(ctx, sessionArgs, now, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	if page == nil {
		page = []sessionSummary{}
	}
	return page, total, nil
}

func (s *PostgresTelemetrySink) queryTelemetryTrend(ctx context.Context, args []any, now time.Time, window time.Duration) ([]trendPoint, error) {
	first := now.Add(-window).Truncate(time.Hour)
	var trend []trendPoint
	for bucket := first; !bucket.After(now); bucket = bucket.Add(time.Hour) {
		trend = append(trend, trendPoint{Time: bucket})
	}

	rows, err := s.pool.Query(ctx, telemetryTrendQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("query telemetry trend: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var hour time.Time
		var attempts, successes, failures int
		if err := rows.Scan(&hour, &attempts, &successes, &failures); err != nil {
			return nil, fmt.Errorf("scan telemetry trend: %w", err)
		}
		index := int(hour.UTC().Sub(first) / time.Hour)
		if index < 0 || index >= len(trend) {
			continue
		}
		trend[index].Attempts = attempts
		trend[index].Successes = successes
		trend[index].Failures = failures
	}
	return trend, rows.Err()
}

func (s *PostgresTelemetrySink) queryTelemetryCounts(ctx context.Context, query string, args []any) (map[string]map[string]int, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]map[string]int)
	for rows.Next() {
		var kind, name string
		var count int
		if err := rows.Scan(&kind, &name, &count); err != nil {
			return nil, err
		}
		if counts[kind] == nil {
			counts[kind] = make(map[string]int)
		}
		counts[kind][name] = count
	}
	return counts, rows.Err()
}

// topRelaySummaries mirrors the in-memory relay ranking: entries exist for any
// relay with a success or failure, ordered by total volume, top ten kept.
func topRelaySummaries(successes, failures map[string]int) []relaySummary {
	relays := make(map[string]*relaySummary)
	for relayID, count := range successes {
		relayFor(relays, relayID).Successes = count
	}
	for relayID, count := range failures {
		relayFor(relays, relayID).Failures = count
	}
	var top []relaySummary
	for _, relay := range relays {
		top = append(top, *relay)
	}
	sortTopRelays(top)
	if len(top) > 10 {
		top = top[:10]
	}
	return top
}

func (s *PostgresTelemetrySink) queryTelemetrySpeedTests(ctx context.Context, args []any) ([]speedTestSummary, error) {
	rows, err := s.pool.Query(ctx, telemetrySpeedTestsQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("query telemetry speed tests: %w", err)
	}
	defer rows.Close()
	var speedTests []speedTestSummary
	for rows.Next() {
		var relayID string
		var tests int
		var mbpsMilli, ttfbMS int64
		if err := rows.Scan(&relayID, &tests, &mbpsMilli, &ttfbMS); err != nil {
			return nil, fmt.Errorf("scan telemetry speed tests: %w", err)
		}
		speedTests = append(speedTests, speedTestSummary{
			RelayID:       relayID,
			Tests:         tests,
			AverageMbps:   float64(mbpsMilli) / float64(tests) / 1000,
			AverageTTFBMS: float64(ttfbMS) / float64(tests),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortSpeedTests(speedTests)
	return speedTests, nil
}

func (s *PostgresTelemetrySink) queryTelemetrySessionPage(ctx context.Context, args []any, now time.Time, limit, offset int) ([]sessionSummary, error) {
	rows, err := s.pool.Query(ctx, telemetrySessionPageQuery, append(append([]any{}, args...), limit, offset)...)
	if err != nil {
		return nil, fmt.Errorf("query telemetry sessions: %w", err)
	}
	defer rows.Close()

	var sessions []sessionSummary
	for rows.Next() {
		var acc sessionAccumulator
		var startedAt, lastSeenAt time.Time
		var lastHeartbeatAt *time.Time
		var endedDurationMS int64
		if err := rows.Scan(
			&acc.summary.SessionID,
			&acc.summary.ClientID,
			&startedAt,
			&lastSeenAt,
			&acc.summary.RelayID,
			&acc.summary.OperatingSystem,
			&acc.deviceManufacturer,
			&acc.deviceModel,
			&acc.summary.AppVersion,
			&acc.summary.Country,
			&acc.summary.City,
			&acc.summary.Organization,
			&acc.summary.ASN,
			&acc.isp,
			&acc.observedClientIP,
			&acc.reportedClientIP,
			&acc.fallbackSourceIP,
			&lastHeartbeatAt,
			&acc.runningDurationMS,
			&endedDurationMS,
			&acc.summary.BytesSent,
			&acc.summary.BytesReceived,
			&acc.attempted,
			&acc.succeeded,
			&acc.failed,
			&acc.terminal,
		); err != nil {
			return nil, fmt.Errorf("scan telemetry session: %w", err)
		}
		acc.summary.Status = "seen"
		acc.summary.StartedAt = startedAt.UTC()
		acc.summary.LastSeenAt = lastSeenAt.UTC()
		acc.summary.DurationMS = endedDurationMS
		if lastHeartbeatAt != nil {
			acc.lastHeartbeatAt = lastHeartbeatAt.UTC()
		}
		sessions = append(sessions, acc.finalize(now))
	}
	return sessions, rows.Err()
}
