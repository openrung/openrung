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
		-- failure_stage/detail use NULLIF so the sessions CTE IS NOT NULL
		-- filter mirrors the accumulator's plain != "" test (a present-but-
		-- empty later connection_failed must not clobber an earlier non-empty
		-- value). NULLIF leaves the failure_stages count unchanged: its CASE
		-- already treats '' as 'unknown'.
		NULLIF(payload->'attributes'->>'failure_stage', '') AS failure_stage,
		NULLIF(payload->'attributes'->>'failure_detail', '') AS failure_detail,
		-- failure_reason mirrors firstNonEmpty(failure_reason, error_type):
		-- the btrim CASE tests the trimmed value but keeps the original, and
		-- yields NULL when both are empty (like country above).
		CASE
			WHEN btrim(COALESCE(payload->'attributes'->>'failure_reason', '')) <> '' THEN payload->'attributes'->>'failure_reason'
			WHEN btrim(COALESCE(payload->'attributes'->>'error_type', '')) <> '' THEN payload->'attributes'->>'error_type'
		END AS failure_reason,
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
		-- Failure fields come only from connection_failed events; latest
		-- non-empty wins, matching sessionSummary's per-field accumulation.
		COALESCE((array_agg(failure_stage ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE event = 'connection_failed' AND failure_stage IS NOT NULL))[1], '') AS failure_stage,
		COALESCE((array_agg(failure_reason ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE event = 'connection_failed' AND failure_reason IS NOT NULL))[1], '') AS failure_reason,
		COALESCE((array_agg(failure_detail ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE event = 'connection_failed' AND failure_detail IS NOT NULL))[1], '') AS failure_detail,
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

// telemetryEventAggregatesQuery collapses every event-grain panel — trend, the
// event-level count groups, relay failure reasons, and speed tests — into a
// single scan of the events CTE. Because all branches reference `events`, the
// CTE is materialized once, so the ~25-field JSONB extraction runs one time per
// overview rather than once per panel. Rows are discriminated by `kind` and
// share a generic column shape so heterogeneous panels can travel together:
//
//	kind  the panel this row feeds
//	k1    first text key    — count name / relay_id; NULL for trend
//	k2    second text key   — relay failure reason; NULL otherwise
//	v1    first value       — count, tests, or (for trend) the hour as epoch
//	                          seconds in a bigint
//	v2..v4  extra values    — trend success/failure counts, speed-test sums
//
// The Go dispatcher feeds each kind through the same helpers the in-memory path
// uses (trend bucket filling, sortedCounts, topRelaySummaries, sortSpeedTests)
// so ranking and tiebreaks stay byte-identical.
const telemetryEventAggregatesQuery = `WITH ` + telemetryEventsCTE + `
-- trend: hourly attempt/success/failure counts. The bucket key rides in v1 as
-- epoch seconds (date_trunc is UTC-aligned like the Go buckets); the caller
-- rebuilds the hour and slots it by index into the pre-filled bucket slice.
SELECT
	'trend'::text AS kind,
	NULL::text AS k1,
	NULL::text AS k2,
	extract(epoch FROM date_trunc('hour', occurred_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC')::bigint AS v1,
	COUNT(*) FILTER (WHERE event = 'connection_attempted')::bigint AS v2,
	COUNT(*) FILTER (WHERE event = 'connection_succeeded')::bigint AS v3,
	COUNT(*) FILTER (WHERE event = 'connection_failed')::bigint AS v4
FROM events
WHERE event IN ('connection_attempted', 'connection_succeeded', 'connection_failed')
GROUP BY 4
UNION ALL
-- top_applications through relay_failures: the event-level count groups, each a
-- (name, count) pair that sortedCounts / topRelaySummaries rank and truncate.
SELECT 'top_applications', application, NULL::text, COUNT(*)::bigint, NULL::bigint, NULL::bigint, NULL::bigint
FROM events WHERE application IS NOT NULL GROUP BY application
UNION ALL
SELECT 'failure_stages',
	CASE WHEN btrim(COALESCE(failure_stage, '')) <> '' THEN failure_stage ELSE 'unknown' END,
	NULL::text, COUNT(*)::bigint, NULL::bigint, NULL::bigint, NULL::bigint
FROM events WHERE event = 'connection_failed' GROUP BY 2
UNION ALL
SELECT 'failure_reasons',
	(CASE WHEN btrim(COALESCE(failure_stage, '')) <> '' THEN failure_stage ELSE 'unknown' END)
		|| ' · ' ||
		(CASE WHEN btrim(COALESCE(failure_reason, '')) <> '' THEN failure_reason ELSE 'unknown' END),
	NULL::text, COUNT(*)::bigint, NULL::bigint, NULL::bigint, NULL::bigint
FROM events WHERE event = 'connection_failed' GROUP BY 2
UNION ALL
SELECT 'relay_successes', relay_id, NULL::text, COUNT(*)::bigint, NULL::bigint, NULL::bigint, NULL::bigint
FROM events WHERE event IN ('connection_succeeded', 'relay_failover') AND relay_id <> '' GROUP BY relay_id
UNION ALL
SELECT 'relay_failures', relay_id, NULL::text, COUNT(*)::bigint, NULL::bigint, NULL::bigint, NULL::bigint
FROM events WHERE event = 'relay_attempt_failed' AND relay_id <> '' GROUP BY relay_id
UNION ALL
-- relay_failure_reasons: per (relay_id, reason) counts feeding topFailureReason;
-- the reason falls back to 'unknown' exactly as the in-memory
-- firstNonEmpty(failure_reason, error_type, "unknown"), and ties resolve
-- lexicographically in Go.
SELECT 'relay_failure_reasons', relay_id,
	CASE WHEN btrim(COALESCE(failure_reason, '')) <> '' THEN failure_reason ELSE 'unknown' END,
	COUNT(*)::bigint, NULL::bigint, NULL::bigint, NULL::bigint
FROM events WHERE event = 'relay_attempt_failed' AND relay_id <> '' GROUP BY 2, 3
UNION ALL
-- speed_tests: per-relay test count plus the sums the caller averages. v1=tests,
-- v2=Σ download_mbps_milli, v3=Σ ttfb_ms.
SELECT 'speed_tests', relay_id, NULL::text,
	COUNT(*)::bigint,
	SUM(COALESCE(download_mbps_milli, 0))::bigint,
	SUM(COALESCE(ttfb_ms, 0))::bigint,
	NULL::bigint
FROM events WHERE event = 'speed_test_completed' AND relay_id <> '' GROUP BY relay_id`

// telemetrySessionAggregatesQuery collapses the headline totals and every
// session-grain count panel into one scan of the sessions CTE (itself one scan
// of events). Both CTEs are referenced by several branches, so each materializes
// once and the per-session array_agg work runs a single time per overview. Rows
// share the (kind, name, count) shape of the event count groups, so
// queryTelemetryCounts scans this query too; the totals ride along as
// kind='totals' rows keyed by metric name.
const telemetrySessionAggregatesQuery = `WITH ` + telemetryEventsCTE + `, ` + telemetrySessionsCTE + `
-- totals: one row per headline metric. clients counts distinct client_ids over
-- the raw events (a client seen only outside a session still counts), matching
-- the in-memory clients set; the rest reduce the sessions CTE, and
-- active_clients is the distinct-client count among active sessions.
SELECT 'totals'::text AS kind, 'clients'::text AS name, COUNT(DISTINCT client_id)::bigint AS count FROM events
UNION ALL
SELECT 'totals', 'sessions', COUNT(*) FROM sessions
UNION ALL
SELECT 'totals', 'attempts', COUNT(*) FILTER (WHERE attempted) FROM sessions
UNION ALL
SELECT 'totals', 'successes', COUNT(*) FILTER (WHERE succeeded) FROM sessions
UNION ALL
SELECT 'totals', 'failures', COUNT(*) FILTER (WHERE failed) FROM sessions
UNION ALL
SELECT 'totals', 'active_clients', COUNT(DISTINCT client_id) FILTER (WHERE active) FROM sessions
UNION ALL
SELECT 'totals', 'active_sessions', COUNT(*) FILTER (WHERE active) FROM sessions
UNION ALL
-- session-grain count groups, one distinct value per session (btrim mirrors the
-- accumulator's non-empty test); sortedCounts ranks and truncates each kind.
SELECT 'top_countries', country, COUNT(*) FROM sessions WHERE btrim(country) <> '' GROUP BY country
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

// The (last_seen_at DESC, session_id) order matches the in-memory sort so
// pages stay stable across requests when batched uploads share a received_at.
// COUNT(*) OVER () rides on every returned row: window functions are evaluated
// before LIMIT/OFFSET, so it is the window's full session count, folding what
// used to be a separate telemetrySessionCountQuery into this one statement. It
// is absent only when the page itself is empty (offset at/past the end, or a
// window with no sessions), which the caller handles by falling back to the
// standalone count.
const telemetrySessionPageQuery = `WITH ` + telemetryEventsCTE + `, ` + telemetrySessionsCTE + `
SELECT
	session_id, client_id, started_at, last_seen_at, relay_id, operating_system,
	device_manufacturer, device_model, app_version, country, city, organization, asn, isp,
	failure_stage, failure_reason, failure_detail,
	observed_client_ip, reported_client_ip, fallback_source_ip,
	last_heartbeat_at, running_duration_ms, ended_duration_ms, bytes_sent, bytes_received,
	attempted, succeeded, failed, terminal,
	COUNT(*) OVER () AS total_count
FROM sessions
ORDER BY last_seen_at DESC, session_id
LIMIT $5 OFFSET $6`

// telemetrySessionCountQuery is the empty-page fallback for the window count
// that telemetrySessionPageQuery normally carries inline.
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
// Postgres with just two statements — one session-grain, one event-grain — each
// scanning (and materializing) its CTE once. Only per-group counts travel back
// to Go, so response size tracks the diversity of the window, not its event
// count. The sessions themselves come from the dedicated sessions endpoint, so
// the overview no longer queries or returns them.
func (s *PostgresTelemetrySink) TelemetryOverview(now time.Time, window time.Duration) (telemetryOverview, error) {
	if err := s.flush(); err != nil {
		slog.Error("could not flush telemetry before read", "error", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresTelemetryQueryTimeout)
	defer cancel()
	eventArgs, sessionArgs := telemetryWindowArgs(now, window)

	overview := telemetryOverview{GeneratedAt: now, Window: window.String()}

	// Session-grain statement: the headline totals plus every session-level
	// count panel, all reducing the sessions CTE materialized once here. The
	// totals arrive as kind='totals' rows keyed by metric name.
	sessionCounts, err := s.queryTelemetryCounts(ctx, telemetrySessionAggregatesQuery, sessionArgs)
	if err != nil {
		return telemetryOverview{}, fmt.Errorf("query telemetry session aggregates: %w", err)
	}
	totals := sessionCounts["totals"]
	overview.Totals = overviewTotals{
		Clients:        totals["clients"],
		Sessions:       totals["sessions"],
		Attempts:       totals["attempts"],
		Successes:      totals["successes"],
		Failures:       totals["failures"],
		ActiveClients:  totals["active_clients"],
		ActiveSessions: totals["active_sessions"],
	}
	if overview.Totals.Attempts > 0 {
		overview.Totals.SuccessRate = float64(overview.Totals.Successes) / float64(overview.Totals.Attempts)
	}
	overview.TopCountries = sortedCounts(sessionCounts["top_countries"], 10)
	overview.TopCities = sortedCounts(sessionCounts["top_cities"], 10)
	overview.TopISPs = sortedCounts(sessionCounts["top_isps"], 10)
	overview.ActiveRelays = sortedCounts(sessionCounts["active_by_relay"], 10)
	overview.ActiveCountries = sortedCounts(sessionCounts["active_by_country"], 10)
	overview.ActiveCities = sortedCounts(sessionCounts["active_by_city"], 10)
	overview.ActiveISPs = sortedCounts(sessionCounts["active_by_isp"], 10)
	overview.ActiveOS = sortedCounts(sessionCounts["active_by_os"], 10)

	// Event-grain statement: trend, the event-level count panels, relay failure
	// reasons, and speed tests, all reducing the events CTE materialized once.
	trend, eventCounts, relayFailureReasons, speedTests, err := s.queryTelemetryEventAggregates(ctx, eventArgs, now, window)
	if err != nil {
		return telemetryOverview{}, err
	}
	overview.Trend = trend
	overview.TopApps = sortedCounts(eventCounts["top_applications"], 10)
	overview.FailureStages = sortedCounts(eventCounts["failure_stages"], 10)
	overview.FailureReasons = sortedCounts(eventCounts["failure_reasons"], 10)
	overview.TopRelays = topRelaySummaries(eventCounts["relay_successes"], eventCounts["relay_failures"], relayFailureReasons)
	overview.SpeedTests = speedTests
	return overview, nil
}

// TelemetrySessions implements TelemetryQuerier with LIMIT/OFFSET pagination.
// The page query carries the window's total session count inline via
// COUNT(*) OVER (), so the common case runs a single statement; only an empty
// page falls back to a standalone count for the handler's offset clamp.
func (s *PostgresTelemetrySink) TelemetrySessions(now time.Time, window time.Duration, offset, limit int) ([]sessionSummary, int, error) {
	if err := s.flush(); err != nil {
		slog.Error("could not flush telemetry before read", "error", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresTelemetryQueryTimeout)
	defer cancel()
	_, sessionArgs := telemetryWindowArgs(now, window)

	page, total, err := s.queryTelemetrySessionPage(ctx, sessionArgs, now, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	if len(page) == 0 {
		// An empty page carries no COUNT(*) OVER () row, so fetch the total
		// directly — the handler's offset>total clamp still needs the real
		// count. Matches the in-memory querier, which reports the full count
		// even when the requested offset lands past the end.
		page = []sessionSummary{}
		if err := s.pool.QueryRow(ctx, telemetrySessionCountQuery, sessionArgs...).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("count telemetry sessions: %w", err)
		}
	}
	return page, total, nil
}

// queryTelemetryEventAggregates runs the single event-grain statement and
// demultiplexes its kind-tagged rows back into the shapes the overview builder
// expects: the hourly trend (pre-filled with empty buckets so gaps render as
// zeros), the event-level count groups, the per-relay failure-reason counts,
// and the speed-test summaries. Because every panel is derived from one scan,
// the events CTE — and its ~25-field JSONB extraction — is materialized once.
func (s *PostgresTelemetrySink) queryTelemetryEventAggregates(ctx context.Context, args []any, now time.Time, window time.Duration) (trend []trendPoint, counts, relayFailureReasons map[string]map[string]int, speedTests []speedTestSummary, err error) {
	// Pre-fill every hour bucket like the in-memory path so the trend spans the
	// whole window even where no events landed; matching rows overwrite by index.
	first := now.Add(-window).Truncate(time.Hour)
	for bucket := first; !bucket.After(now); bucket = bucket.Add(time.Hour) {
		trend = append(trend, trendPoint{Time: bucket})
	}
	counts = make(map[string]map[string]int)
	relayFailureReasons = make(map[string]map[string]int)

	rows, err := s.pool.Query(ctx, telemetryEventAggregatesQuery, args...)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("query telemetry event aggregates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		// k1/k2 and v2..v4 are NULL for the panels that do not use them, so scan
		// into pointers and read them back through the nil-safe helpers.
		var kind string
		var k1, k2 *string
		var v1, v2, v3, v4 *int64
		if err := rows.Scan(&kind, &k1, &k2, &v1, &v2, &v3, &v4); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("scan telemetry event aggregate: %w", err)
		}
		switch kind {
		case "trend":
			// v1 is the bucket hour as epoch seconds; reconstruct the instant and
			// place it by index into the pre-filled bucket slice.
			hour := time.Unix(int64Value(v1), 0).UTC()
			index := int(hour.Sub(first) / time.Hour)
			if index < 0 || index >= len(trend) {
				continue
			}
			trend[index].Attempts = int(int64Value(v2))
			trend[index].Successes = int(int64Value(v3))
			trend[index].Failures = int(int64Value(v4))
		case "relay_failure_reasons":
			addCount(relayFailureReasons, stringValue(k1), stringValue(k2), int(int64Value(v1)))
		case "speed_tests":
			tests := int(int64Value(v1))
			speedTests = append(speedTests, speedTestSummary{
				RelayID:       stringValue(k1),
				Tests:         tests,
				AverageMbps:   float64(int64Value(v2)) / float64(tests) / 1000,
				AverageTTFBMS: float64(int64Value(v3)) / float64(tests),
			})
		default:
			// The remaining kinds are the event-level count groups: (name, count).
			addCount(counts, kind, stringValue(k1), int(int64Value(v1)))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, nil, err
	}
	sortSpeedTests(speedTests)
	return trend, counts, relayFailureReasons, speedTests, nil
}

// addCount records counts[outer][inner] = count, allocating the inner map on
// first use. Shared by the count-group demultiplexers.
func addCount(counts map[string]map[string]int, outer, inner string, count int) {
	if counts[outer] == nil {
		counts[outer] = make(map[string]int)
	}
	counts[outer][inner] = count
}

func stringValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func int64Value(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
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
// relay with a success or failure, ordered by total volume, top ten kept. The
// modal failure reason reuses the shared topFailureReason helper so ties resolve
// lexicographically just like the in-memory path.
func topRelaySummaries(successes, failures map[string]int, failureReasons map[string]map[string]int) []relaySummary {
	relays := make(map[string]*relaySummary)
	for relayID, count := range successes {
		relayFor(relays, relayID).Successes = count
	}
	for relayID, count := range failures {
		relayFor(relays, relayID).Failures = count
	}
	var top []relaySummary
	for _, relay := range relays {
		relay.TopFailureReason = topFailureReason(failureReasons[relay.RelayID])
		top = append(top, *relay)
	}
	sortTopRelays(top)
	if len(top) > 10 {
		top = top[:10]
	}
	return top
}

// queryTelemetrySessionPage returns one ordered page of the window's sessions
// and the window's total session count. The total rides on each row via
// COUNT(*) OVER () in telemetrySessionPageQuery, so it is only meaningful when
// the page is non-empty; the caller supplies the count for an empty page.
func (s *PostgresTelemetrySink) queryTelemetrySessionPage(ctx context.Context, args []any, now time.Time, limit, offset int) ([]sessionSummary, int, error) {
	rows, err := s.pool.Query(ctx, telemetrySessionPageQuery, append(append([]any{}, args...), limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("query telemetry sessions: %w", err)
	}
	defer rows.Close()

	var sessions []sessionSummary
	var total int
	for rows.Next() {
		var acc sessionAccumulator
		var startedAt, lastSeenAt time.Time
		var lastHeartbeatAt *time.Time
		var endedDurationMS int64
		var rowTotal int64
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
			&acc.summary.FailureStage,
			&acc.summary.FailureReason,
			&acc.summary.FailureDetail,
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
			&rowTotal,
		); err != nil {
			return nil, 0, fmt.Errorf("scan telemetry session: %w", err)
		}
		// Every row of a non-empty page carries the same window total.
		total = int(rowTotal)
		acc.summary.Status = "seen"
		acc.summary.StartedAt = startedAt.UTC()
		acc.summary.LastSeenAt = lastSeenAt.UTC()
		acc.summary.DurationMS = endedDurationMS
		if lastHeartbeatAt != nil {
			acc.lastHeartbeatAt = lastHeartbeatAt.UTC()
		}
		sessions = append(sessions, acc.finalize(now))
	}
	return sessions, total, rows.Err()
}
