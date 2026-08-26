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
//
// telemetryEventsCTEWith builds the CTE with an optional extra join (the
// two-phase session page narrows it to one page of sessions); the join clause
// is a compile-time string from this file, never input.
//
// The per-field `payload->'attributes'->>...` chains look redundant but are
// not worth collapsing into a jsonb_to_record lateral: jsonb is stored already
// parsed, so each `->` is a binary search over the object rather than a
// re-parse, and measuring both shapes against production telemetry made the
// lateral version SLOWER (18.6s vs 17.1s for the overview). The lateral's
// function-scan cost estimate is also low enough to talk the planner out of
// parallelising the scan, which is where the rest of the loss came from.
func telemetryEventsCTEWith(join string) string {
	return `
events AS (
	SELECT
		e.received_at,
		e.occurred_at,
		e.event,
		e.client_id,
		e.session_id,
		COALESCE(e.relay_id, '') AS relay_id,
		COALESCE(e.relay_node_class, '') AS relay_node_class,
		host(e.source_ip) AS source_ip,
		-- failure_stage/detail use NULLIF so the sessions CTE IS NOT NULL
		-- filter mirrors the accumulator's plain != "" test (a present-but-
		-- empty later connection_failed must not clobber an earlier non-empty
		-- value). NULLIF leaves the failure_stages count unchanged: its CASE
		-- already treats '' as 'unknown'.
		NULLIF(e.payload->'attributes'->>'failure_stage', '') AS failure_stage,
		NULLIF(e.payload->'attributes'->>'failure_detail', '') AS failure_detail,
		-- failure_reason mirrors firstNonEmpty(failure_reason, error_type):
		-- the btrim CASE tests the trimmed value but keeps the original, and
		-- yields NULL when both are empty (like country above).
		CASE
			WHEN btrim(COALESCE(e.payload->'attributes'->>'failure_reason', '')) <> '' THEN e.payload->'attributes'->>'failure_reason'
			WHEN btrim(COALESCE(e.payload->'attributes'->>'error_type', '')) <> '' THEN e.payload->'attributes'->>'error_type'
		END AS failure_reason,
		COALESCE(
			NULLIF(e.payload->'attributes'->>'operating_system', ''),
			'iOS ' || NULLIF(e.payload->'attributes'->>'ios_version', ''),
			'Android (API ' || NULLIF(e.payload->'attributes'->>'android_api', '') || ')'
		) AS os_label,
		NULLIF(e.payload->'attributes'->>'device_manufacturer', '') AS device_manufacturer,
		NULLIF(e.payload->'attributes'->>'device_model', '') AS device_model,
		NULLIF(e.payload->'attributes'->>'app_version', '') AS app_version,
		CASE
			WHEN btrim(COALESCE(e.payload->'attributes'->>'country', '')) <> '' THEN e.payload->'attributes'->>'country'
			WHEN btrim(COALESCE(e.payload->'attributes'->>'country_code', '')) <> '' THEN e.payload->'attributes'->>'country_code'
		END AS country,
		NULLIF(e.payload->'attributes'->>'city', '') AS city,
		NULLIF(e.payload->'attributes'->>'organization', '') AS organization,
		NULLIF(e.payload->'attributes'->>'asn', '') AS asn,
		NULLIF(e.payload->'attributes'->>'isp', '') AS isp,
		NULLIF(e.payload->'attributes'->>'client_ip', '') AS reported_client_ip,
		(e.payload->'measurements'->>'session_duration_ms')::bigint AS session_duration_ms,
		(e.payload->'measurements'->>'bytes_sent')::bigint AS bytes_sent,
		(e.payload->'measurements'->>'bytes_received')::bigint AS bytes_received,
		(e.payload->'measurements'->>'download_mbps_milli')::bigint AS download_mbps_milli,
		(e.payload->'measurements'->>'time_to_first_byte_ms')::bigint AS ttfb_ms
	FROM telemetry_events e
	` + join + `
	-- application_connection is excluded belt-and-braces: since the hourly
	-- rollup landed those events are never inserted as rows, but partitions
	-- written before the rollup (or by an older broker) may still hold them,
	-- and at production volume they were ~95% of all rows.
	WHERE e.received_at > $1 AND e.occurred_at >= $2 AND e.occurred_at <= $3
		AND e.event <> 'application_connection'
)`
}

var telemetryEventsCTE = telemetryEventsCTEWith("")

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
		-- Keep the class paired with the same latest relay-bearing event. An empty
		-- class on that event must not borrow the class of an earlier failover relay.
		COALESCE((array_agg(relay_node_class ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE relay_id <> ''))[1], '') AS relay_node_class,
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

// telemetryOverviewAggregatesQuery collapses the WHOLE overview — headline
// totals, every session-grain and event-grain count panel, trend, relay
// failure reasons, retained relay classes, and speed tests — into a single
// statement. The events CTE (and its payload extraction) materializes once and
// the sessions CTE once, where the previous two-statement split scanned and
// extracted the window's events twice per overview. Rows are discriminated by
// `kind` and share a generic column shape so heterogeneous panels can travel
// together:
//
//	kind  the panel this row feeds
//	k1    first text key    — count name / relay_id; NULL for trend
//	k2    second text key   — relay failure reason / retained class; NULL otherwise
//	v1    first value       — count, tests, or (for trend) the hour as epoch
//	                          seconds in a bigint
//	v2..v4  extra values    — trend success/failure counts, speed-test sums
//
// The Go dispatcher feeds each kind through the same helpers the in-memory path
// uses (trend bucket filling, sortedCounts, topRelaySummaries, sortSpeedTests)
// so ranking and tiebreaks stay byte-identical.
var telemetryOverviewAggregatesQuery = `WITH ` + telemetryEventsCTE + `, ` + telemetrySessionsCTE + `
-- totals: one row per headline metric. clients counts distinct client_ids over
-- the raw events (a client seen only outside a session still counts), matching
-- the in-memory clients set; the rest reduce the sessions CTE, and
-- active_clients is the distinct-client count among active sessions.
SELECT 'totals'::text AS kind, 'clients'::text AS k1, NULL::text AS k2,
	COUNT(DISTINCT client_id)::bigint AS v1, NULL::bigint AS v2, NULL::bigint AS v3, NULL::bigint AS v4
FROM events
UNION ALL
SELECT 'totals', 'sessions', NULL, COUNT(*), NULL, NULL, NULL FROM sessions
UNION ALL
SELECT 'totals', 'attempts', NULL, COUNT(*) FILTER (WHERE attempted), NULL, NULL, NULL FROM sessions
UNION ALL
SELECT 'totals', 'successes', NULL, COUNT(*) FILTER (WHERE succeeded), NULL, NULL, NULL FROM sessions
UNION ALL
SELECT 'totals', 'failures', NULL, COUNT(*) FILTER (WHERE failed), NULL, NULL, NULL FROM sessions
UNION ALL
SELECT 'totals', 'active_clients', NULL, COUNT(DISTINCT client_id) FILTER (WHERE active), NULL, NULL, NULL FROM sessions
UNION ALL
SELECT 'totals', 'active_sessions', NULL, COUNT(*) FILTER (WHERE active), NULL, NULL, NULL FROM sessions
UNION ALL
-- session-grain count groups, one distinct value per session (btrim mirrors the
-- accumulator's non-empty test); sortedCounts ranks and truncates each kind.
SELECT 'top_countries', country, NULL, COUNT(*), NULL, NULL, NULL FROM sessions WHERE btrim(country) <> '' GROUP BY country
UNION ALL
SELECT 'top_cities', city, NULL, COUNT(*), NULL, NULL, NULL FROM sessions WHERE btrim(city) <> '' GROUP BY city
UNION ALL
SELECT 'top_isps', ` + telemetrySessionISPLabel + `, NULL, COUNT(*), NULL, NULL, NULL
FROM sessions WHERE btrim(` + telemetrySessionISPLabel + `) <> '' GROUP BY 2
UNION ALL
SELECT 'active_by_relay', relay_id, NULL, COUNT(*), NULL, NULL, NULL FROM sessions WHERE active AND btrim(relay_id) <> '' GROUP BY relay_id
UNION ALL
SELECT 'active_by_country', country, NULL, COUNT(*), NULL, NULL, NULL FROM sessions WHERE active AND btrim(country) <> '' GROUP BY country
UNION ALL
SELECT 'active_by_city', city, NULL, COUNT(*), NULL, NULL, NULL FROM sessions WHERE active AND btrim(city) <> '' GROUP BY city
UNION ALL
SELECT 'active_by_isp', ` + telemetrySessionISPLabel + `, NULL, COUNT(*), NULL, NULL, NULL
FROM sessions WHERE active AND btrim(` + telemetrySessionISPLabel + `) <> '' GROUP BY 2
UNION ALL
SELECT 'active_by_os', operating_system, NULL, COUNT(*), NULL, NULL, NULL FROM sessions WHERE active AND btrim(operating_system) <> '' GROUP BY operating_system
UNION ALL
-- trend: hourly attempt/success/failure counts. The bucket key rides in v1 as
-- epoch seconds (date_trunc is UTC-aligned like the Go buckets); the caller
-- rebuilds the hour and slots it by index into the pre-filled bucket slice.
SELECT 'trend', NULL, NULL,
	extract(epoch FROM date_trunc('hour', occurred_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC')::bigint,
	COUNT(*) FILTER (WHERE event = 'connection_attempted')::bigint,
	COUNT(*) FILTER (WHERE event = 'connection_succeeded')::bigint,
	COUNT(*) FILTER (WHERE event = 'connection_failed')::bigint
FROM events
WHERE event IN ('connection_attempted', 'connection_succeeded', 'connection_failed')
GROUP BY 4
UNION ALL
-- top_applications reads the hourly rollup, not the events CTE:
-- application_connection events are folded into telemetry_app_counts at
-- ingestion and never stored as rows. The window edge is hour-granular (the
-- truncated start hour is included whole), matching telemetryAppRollup's
-- countsIn; the date_trunc runs in UTC like the trend buckets.
SELECT 'top_applications', application, NULL, SUM(connections)::bigint, NULL, NULL, NULL
FROM telemetry_app_counts
WHERE hour >= date_trunc('hour', $2::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AND hour <= $3
GROUP BY application
UNION ALL
-- failure_stages through relay_failures: the event-level count groups, each a
-- (name, count) pair that sortedCounts / topRelaySummaries rank and truncate.
SELECT 'failure_stages',
	CASE WHEN btrim(COALESCE(failure_stage, '')) <> '' THEN failure_stage ELSE 'unknown' END,
	NULL, COUNT(*), NULL, NULL, NULL
FROM events WHERE event = 'connection_failed' GROUP BY 2
UNION ALL
SELECT 'failure_reasons',
	(CASE WHEN btrim(COALESCE(failure_stage, '')) <> '' THEN failure_stage ELSE 'unknown' END)
		|| ' · ' ||
		(CASE WHEN btrim(COALESCE(failure_reason, '')) <> '' THEN failure_reason ELSE 'unknown' END),
	NULL, COUNT(*), NULL, NULL, NULL
FROM events WHERE event = 'connection_failed' GROUP BY 2
UNION ALL
SELECT 'relay_successes', relay_id, NULL, COUNT(*), NULL, NULL, NULL
FROM events WHERE event IN ('connection_succeeded', 'relay_failover') AND relay_id <> '' GROUP BY relay_id
UNION ALL
SELECT 'relay_failures', relay_id, NULL, COUNT(*), NULL, NULL, NULL
FROM events WHERE event = 'relay_attempt_failed' AND relay_id <> '' GROUP BY relay_id
UNION ALL
-- relay_failure_reasons: per (relay_id, reason) counts feeding topFailureReason;
-- the reason falls back to 'unknown' exactly as the in-memory
-- firstNonEmpty(failure_reason, error_type, "unknown"), and ties resolve
-- lexicographically in Go.
SELECT 'relay_failure_reasons', relay_id,
	CASE WHEN btrim(COALESCE(failure_reason, '')) <> '' THEN failure_reason ELSE 'unknown' END,
	COUNT(*), NULL, NULL, NULL
FROM events WHERE event = 'relay_attempt_failed' AND relay_id <> '' GROUP BY 2, 3
UNION ALL
-- relay_classes: the broker-attested class retained with any event for this
-- immutable relay ID. The latest non-empty value covers mixed legacy/new
-- telemetry without splitting the relay's aggregate counts.
SELECT 'relay_classes', relay_id,
	(array_agg(relay_node_class ORDER BY received_at DESC, occurred_at DESC)
		FILTER (WHERE relay_node_class <> ''))[1],
	NULL, NULL, NULL, NULL
FROM events WHERE relay_id <> '' GROUP BY relay_id
UNION ALL
-- speed_tests: per-relay test count plus the sums the caller averages. v1=tests,
-- v2=Σ download_mbps_milli, v3=Σ ttfb_ms.
SELECT 'speed_tests', relay_id, NULL,
	COUNT(*),
	SUM(COALESCE(download_mbps_milli, 0))::bigint,
	SUM(COALESCE(ttfb_ms, 0))::bigint,
	NULL
FROM events WHERE event = 'speed_test_completed' AND relay_id <> '' GROUP BY relay_id`

// telemetrySessionPageQuery pages sessions in two phases so the expensive part
// only ever touches one page. The `page` CTE ranks the window's sessions with
// a slim scan that never reads the payload — (last_seen_at DESC, session_id),
// the same order the in-memory querier sorts by, with MAX(received_at) exactly
// the sessions CTE's last_seen_at — and applies LIMIT/OFFSET there. Only the
// surviving page's events then flow through the payload extraction and the
// per-session array_agg work, via the (session_id, occurred_at) index. The
// COUNT(*) OVER () window function is evaluated after GROUP BY but before
// LIMIT/OFFSET, so ranked_total is the window's full session count riding on
// every returned row, folding what used to be a separate
// telemetrySessionCountQuery into this one statement. It is absent only when
// the page itself is empty (offset at/past the end, or a window with no
// sessions), which the caller handles by falling back to the standalone count.
var telemetrySessionPageQuery = `WITH page AS (
	SELECT
		session_id,
		MAX(received_at) AS ranked_last_seen,
		COUNT(*) OVER () AS ranked_total
	FROM telemetry_events
	WHERE received_at > $1 AND occurred_at >= $2 AND occurred_at <= $3
		AND event <> 'application_connection'
	GROUP BY session_id
	ORDER BY ranked_last_seen DESC, session_id
	LIMIT $5 OFFSET $6
), ` + telemetryEventsCTEWith("JOIN page USING (session_id)") + `, ` + telemetrySessionsCTE + `
SELECT
	session_id, client_id, started_at, last_seen_at, relay_id, relay_node_class, operating_system,
	device_manufacturer, device_model, app_version, country, city, organization, asn, isp,
	failure_stage, failure_reason, failure_detail,
	observed_client_ip, reported_client_ip, fallback_source_ip,
	last_heartbeat_at, running_duration_ms, ended_duration_ms, bytes_sent, bytes_received,
	attempted, succeeded, failed, terminal,
	page.ranked_total AS total_count
FROM sessions
JOIN page USING (session_id)
ORDER BY last_seen_at DESC, session_id`

// telemetrySessionCountQuery is the empty-page fallback for the window count
// that telemetrySessionPageQuery normally carries inline. It needs no payload
// fields, so it counts distinct sessions directly off the table; it takes the
// three event-window parameters only.
const telemetrySessionCountQuery = `
SELECT COUNT(DISTINCT session_id)
FROM telemetry_events
WHERE received_at > $1 AND occurred_at >= $2 AND occurred_at <= $3
	AND event <> 'application_connection'`

// telemetryRelayStatsQuery aggregates the window per relay for the relays
// page: event-grain counts (successes, failures, speed-test sums, last seen,
// retained class), session-grain counts (sessions, distinct clients, active
// sessions and clients), and the modal relay_attempt_failed reason.
// relay_events covers every relay any event named, so the LEFT JOINs lose
// nothing: a relay in relay_sessions or relay_top_reasons necessarily has
// relay-bearing events. Only rows whose retained node_class is non-empty are
// returned — ingestion stamps the class solely for relays holding a live
// registration, so the filter is what keeps anonymous telemetry naming
// fabricated relay IDs from minting one row each (see relayTelemetryStats).
// The trailing sentinel row (relay_id = ”) carries the overall distinct
// count of clients active through any attested relay, which the per-relay
// rows cannot express — one client may be active on several relays.
var telemetryRelayStatsQuery = `WITH ` + telemetryEventsCTE + `, ` + telemetrySessionsCTE + `,
relay_events AS (
	SELECT
		relay_id,
		COUNT(*) FILTER (WHERE event IN ('connection_succeeded', 'relay_failover'))::bigint AS successes,
		COUNT(*) FILTER (WHERE event = 'relay_attempt_failed')::bigint AS failures,
		COUNT(*) FILTER (WHERE event = 'speed_test_completed')::bigint AS speed_tests,
		COALESCE(SUM(download_mbps_milli) FILTER (WHERE event = 'speed_test_completed'), 0)::bigint AS mbps_milli_sum,
		COALESCE(SUM(ttfb_ms) FILTER (WHERE event = 'speed_test_completed'), 0)::bigint AS ttfb_ms_sum,
		MAX(received_at) AS last_seen_at,
		COALESCE((array_agg(relay_node_class ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE relay_node_class <> ''))[1], '') AS node_class
	FROM events
	WHERE relay_id <> ''
	GROUP BY relay_id
),
relay_sessions AS (
	SELECT
		relay_id,
		COUNT(*)::bigint AS sessions,
		COUNT(DISTINCT client_id)::bigint AS clients,
		COUNT(*) FILTER (WHERE active)::bigint AS active_sessions,
		COUNT(DISTINCT client_id) FILTER (WHERE active)::bigint AS active_clients
	FROM sessions
	WHERE relay_id <> ''
	GROUP BY relay_id
),
-- The modal relay_attempt_failed reason per relay; the (occurrences DESC,
-- reason) order makes DISTINCT ON resolve count ties to the lexicographically
-- smallest reason, matching topFailureReason in Go.
relay_top_reasons AS (
	SELECT DISTINCT ON (relay_id) relay_id, reason
	FROM (
		SELECT relay_id,
			CASE WHEN btrim(COALESCE(failure_reason, '')) <> '' THEN failure_reason ELSE 'unknown' END AS reason,
			COUNT(*) AS occurrences
		FROM events
		WHERE event = 'relay_attempt_failed' AND relay_id <> ''
		GROUP BY 1, 2
	) reasons
	ORDER BY relay_id, occurrences DESC, reason
)
SELECT
	e.relay_id, e.node_class, e.successes, e.failures,
	COALESCE(r.reason, '') AS top_failure_reason,
	COALESCE(s.sessions, 0) AS sessions,
	COALESCE(s.clients, 0) AS clients,
	COALESCE(s.active_sessions, 0) AS active_sessions,
	COALESCE(s.active_clients, 0) AS active_clients,
	e.speed_tests, e.mbps_milli_sum, e.ttfb_ms_sum,
	e.last_seen_at
FROM relay_events e
LEFT JOIN relay_sessions s USING (relay_id)
LEFT JOIN relay_top_reasons r USING (relay_id)
-- $5 is the broker's own set of currently registered relay IDs: a live
-- registration attests an ID exactly like an in-window stamp does, so an
-- online relay keeps its telemetry even when every window event was received
-- during a lease gap. Telemetry-only (offline) IDs still need the stamp.
WHERE e.node_class <> '' OR e.relay_id = ANY($5)
UNION ALL
SELECT '', '', 0, 0, '', 0, 0, 0,
	COUNT(DISTINCT sessions.client_id) FILTER (WHERE sessions.active)::bigint,
	0, 0, 0, NULL
FROM sessions
JOIN relay_events trusted ON trusted.relay_id = sessions.relay_id
	AND (trusted.node_class <> '' OR trusted.relay_id = ANY($5))`

// telemetryWindowArgs is the shared parameter list documented on the CTEs.
// Queries that only touch the events CTE must take eventArgs — Postgres
// rejects bound parameters a statement never references.
func telemetryWindowArgs(now time.Time, window time.Duration) (eventArgs, sessionArgs []any) {
	start := now.Add(-window)
	sessionArgs = []any{start.Add(-maxTelemetryFutureSkew), start, now, now.Add(-activeSessionTimeout)}
	return sessionArgs[:3], sessionArgs
}

// TelemetryOverview implements TelemetryQuerier by aggregating the window in
// Postgres with one statement, scanning and materializing the events CTE
// exactly once (the sessions CTE built on top of it likewise) where the
// previous session-grain/event-grain split scanned the window twice per
// overview. Only per-group counts travel back to Go, so response size tracks
// the diversity of the window, not its event count. The sessions themselves
// come from the dedicated sessions endpoint, so the overview no longer queries
// or returns them.
func (s *PostgresTelemetrySink) TelemetryOverview(now time.Time, window time.Duration) (telemetryOverview, error) {
	if err := s.flush(); err != nil {
		slog.Error("could not flush telemetry before read", "error", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresTelemetryQueryTimeout)
	defer cancel()
	_, sessionArgs := telemetryWindowArgs(now, window)

	trend, counts, relayFailureReasons, relayClasses, speedTests, err := s.queryTelemetryOverviewAggregates(ctx, sessionArgs, now, window)
	if err != nil {
		return telemetryOverview{}, err
	}

	overview := telemetryOverview{GeneratedAt: now, Window: window.String()}
	totals := counts["totals"]
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
	overview.Trend = trend
	overview.TopApps = sortedCounts(counts["top_applications"], 10)
	overview.TopCountries = sortedCounts(counts["top_countries"], 10)
	overview.TopCities = sortedCounts(counts["top_cities"], 10)
	overview.TopISPs = sortedCounts(counts["top_isps"], 10)
	overview.ActiveRelays = sortedCounts(counts["active_by_relay"], 10)
	overview.ActiveCountries = sortedCounts(counts["active_by_country"], 10)
	overview.ActiveCities = sortedCounts(counts["active_by_city"], 10)
	overview.ActiveISPs = sortedCounts(counts["active_by_isp"], 10)
	overview.ActiveOS = sortedCounts(counts["active_by_os"], 10)
	overview.FailureStages = sortedCounts(counts["failure_stages"], 10)
	overview.FailureReasons = sortedCounts(counts["failure_reasons"], 10)
	overview.TopRelays = topRelaySummaries(counts["relay_successes"], counts["relay_failures"], relayFailureReasons)
	overview.SpeedTests = speedTests
	applyTelemetryRelayClasses(&overview, relayClasses)
	return overview, nil
}

// TelemetrySessions implements TelemetryQuerier with LIMIT/OFFSET pagination.
// The page query ranks sessions with a payload-free scan, aggregates only the
// requested page, and carries the window's total session count inline via
// COUNT(*) OVER (), so the common case runs a single statement; only an empty
// page falls back to a standalone count for the handler's offset clamp.
func (s *PostgresTelemetrySink) TelemetrySessions(now time.Time, window time.Duration, offset, limit int) ([]sessionSummary, int, error) {
	if err := s.flush(); err != nil {
		slog.Error("could not flush telemetry before read", "error", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresTelemetryQueryTimeout)
	defer cancel()
	eventArgs, sessionArgs := telemetryWindowArgs(now, window)

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
		if err := s.pool.QueryRow(ctx, telemetrySessionCountQuery, eventArgs...).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("count telemetry sessions: %w", err)
		}
	}
	return page, total, nil
}

// TelemetryRelayStats implements TelemetryQuerier with one statement: the
// events and sessions CTEs each materialize once and only per-relay rows (plus
// the sentinel totals row) travel back to Go.
func (s *PostgresTelemetrySink) TelemetryRelayStats(now time.Time, window time.Duration, registeredIDs []string) (relayTelemetryStats, error) {
	if err := s.flush(); err != nil {
		slog.Error("could not flush telemetry before read", "error", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresTelemetryQueryTimeout)
	defer cancel()
	_, sessionArgs := telemetryWindowArgs(now, window)
	if registeredIDs == nil {
		// pgx encodes a nil slice as NULL, and `= ANY(NULL)` matches nothing
		// only by accident of three-valued logic; be explicit.
		registeredIDs = []string{}
	}

	rows, err := s.pool.Query(ctx, telemetryRelayStatsQuery, append(append([]any{}, sessionArgs...), registeredIDs)...)
	if err != nil {
		return relayTelemetryStats{}, fmt.Errorf("query telemetry relay stats: %w", err)
	}
	defer rows.Close()

	stats := relayTelemetryStats{}
	for rows.Next() {
		var row relayStatRow
		var successes, failures, sessions, clients, activeSessions, activeClients, speedTests, mbpsMilliSum, ttfbMSSum int64
		var lastSeenAt *time.Time
		if err := rows.Scan(&row.RelayID, &row.NodeClass, &successes, &failures, &row.TopFailureReason,
			&sessions, &clients, &activeSessions, &activeClients,
			&speedTests, &mbpsMilliSum, &ttfbMSSum, &lastSeenAt); err != nil {
			return relayTelemetryStats{}, fmt.Errorf("scan telemetry relay stats: %w", err)
		}
		if row.RelayID == "" {
			// The sentinel row carries only the overall active-client count.
			stats.ActiveClients = int(activeClients)
			continue
		}
		row.Successes, row.Failures = int(successes), int(failures)
		row.Sessions, row.Clients = int(sessions), int(clients)
		row.ActiveSessions, row.ActiveClients = int(activeSessions), int(activeClients)
		row.SpeedTests = int(speedTests)
		applyRelaySpeedAverages(&row, mbpsMilliSum, ttfbMSSum)
		if lastSeenAt != nil {
			row.LastSeenAt = lastSeenAt.UTC()
		}
		stats.Relays = append(stats.Relays, row)
	}
	if err := rows.Err(); err != nil {
		return relayTelemetryStats{}, err
	}
	sortRelayStatRows(stats.Relays)
	return stats, nil
}

// queryTelemetryOverviewAggregates runs the single overview statement and
// demultiplexes its kind-tagged rows back into the shapes the overview builder
// expects: the hourly trend (pre-filled with empty buckets so gaps render as
// zeros), the totals and count groups, per-relay failure-reason counts and
// retained classes, and the speed-test summaries. Because every panel is
// derived from one scan, the events CTE — and its payload extraction — is
// materialized once per overview.
func (s *PostgresTelemetrySink) queryTelemetryOverviewAggregates(ctx context.Context, args []any, now time.Time, window time.Duration) (trend []trendPoint, counts, relayFailureReasons map[string]map[string]int, relayClasses map[string]string, speedTests []speedTestSummary, err error) {
	// Pre-fill every hour bucket like the in-memory path so the trend spans the
	// whole window even where no events landed; matching rows overwrite by index.
	first := now.Add(-window).Truncate(time.Hour)
	for bucket := first; !bucket.After(now); bucket = bucket.Add(time.Hour) {
		trend = append(trend, trendPoint{Time: bucket})
	}
	counts = make(map[string]map[string]int)
	relayFailureReasons = make(map[string]map[string]int)
	relayClasses = make(map[string]string)

	rows, err := s.pool.Query(ctx, telemetryOverviewAggregatesQuery, args...)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("query telemetry overview aggregates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		// k1/k2 and v2..v4 are NULL for the panels that do not use them, so scan
		// into pointers and read them back through the nil-safe helpers.
		var kind string
		var k1, k2 *string
		var v1, v2, v3, v4 *int64
		if err := rows.Scan(&kind, &k1, &k2, &v1, &v2, &v3, &v4); err != nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("scan telemetry overview aggregate: %w", err)
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
		case "relay_classes":
			relayClasses[stringValue(k1)] = stringValue(k2)
		case "speed_tests":
			tests := int(int64Value(v1))
			speedTests = append(speedTests, speedTestSummary{
				RelayID:       stringValue(k1),
				Tests:         tests,
				AverageMbps:   float64(int64Value(v2)) / float64(tests) / 1000,
				AverageTTFBMS: float64(int64Value(v3)) / float64(tests),
			})
		default:
			// The remaining kinds are (name, count) groups: the totals rows keyed
			// by metric name plus every session- and event-level count panel.
			addCount(counts, kind, stringValue(k1), int(int64Value(v1)))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, nil, nil, err
	}
	speedTests = rankSpeedTests(speedTests)
	return trend, counts, relayFailureReasons, relayClasses, speedTests, nil
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
			&acc.summary.RelayNodeClass,
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
