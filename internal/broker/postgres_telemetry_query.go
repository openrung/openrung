package broker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// The dashboard queries below reproduce buildTelemetryOverview in SQL. They
// come in two flavours that share one panel-building body and differ only in
// where the window's rows come from:
//
//   - The event path (1h window, and every window until the rollup backfill
//     completes) scans telemetry_events bounded by the window. Its `events`
//     CTE extracts the payload fields the aggregator reads, `session_rows`
//     groups them per session with the same semantics as sessionAccumulator,
//     and `event_counts` reduces them to hourly event-grain counts.
//   - The rollup path (24h through 31d) reads the same two shapes straight from
//     telemetry_sessions and telemetry_event_counts, which ingestion maintains
//     incrementally (see telemetryRollupStatement), so a window costs one scan
//     over ~sessions rows rather than over every event. Window membership is
//     per session (a session is in the window when its latest event is) and
//     the event-grain edge is hour-granular, exactly like top_applications.
//
// All queries take the same leading parameters:
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
// telemetryEventsCTEFrom builds the events CTE over an arbitrary source (the
// window scan, the two-phase session page's narrowed join, a flush's freshly
// inserted rows, or a backfill chunk); every source is a compile-time string
// from this file, never input.
//
// The per-field `payload->'attributes'->>...` chains look redundant but are
// not worth collapsing into a jsonb_to_record lateral: jsonb is stored already
// parsed, so each `->` is a binary search over the object rather than a
// re-parse, and measuring both shapes against production telemetry made the
// lateral version SLOWER (18.6s vs 17.1s for the overview). The lateral's
// function-scan cost estimate is also low enough to talk the planner out of
// parallelising the scan, which is where the rest of the loss came from.
func telemetryEventsCTEFrom(from string) string {
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
		NULLIF(e.payload->'attributes'->>'access_outcome', '') AS access_outcome,
		NULLIF(e.payload->'attributes'->>'relay_source', '') AS relay_source,
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
	` + from + `
)`
}

// telemetryWindowEventsWhere bounds the event path to the dashboard window.
// application_connection is excluded belt-and-braces: since the hourly rollup
// landed those events are never inserted as rows, but partitions written
// before the rollup (or by an older broker) may still hold them, and at
// production volume they were ~95% of all rows.
const telemetryWindowEventsWhere = `
	WHERE e.received_at > $1 AND e.occurred_at >= $2 AND e.occurred_at <= $3
		AND e.event <> 'application_connection'`

var telemetryEventsCTE = telemetryEventsCTEFrom(`FROM telemetry_events e` + telemetryWindowEventsWhere)

// telemetrySessionColumns is the column set the session_rows CTE produces
// from events and the rollup path reads back from telemetry_sessions: the raw
// per-session accumulation that sessionAccumulator.finalize turns into a
// sessionSummary. client_ids (every distinct client the session named, for
// the headline clients total) and last_occurred_at (window membership) exist
// only for the rollup's sake.
const telemetrySessionColumns = `session_id, client_id, client_ids, started_at, last_occurred_at, last_seen_at,
	relay_id, relay_node_class, operating_system, device_manufacturer, device_model, app_version,
	country, city, organization, asn, isp, failure_stage, failure_reason, failure_detail,
	observed_client_ip, reported_client_ip, fallback_source_ip,
	last_heartbeat_at, running_duration_ms, ended_duration_ms, bytes_sent, bytes_received,
	attempted, succeeded, failed, terminal`

// telemetrySessionLatestField is one "latest non-empty wins" column of the
// session accumulation: the latest (by received_at, then occurred_at) event
// satisfying filter supplies expr. The rollup also keeps, per field, the
// received_at of that supplying event (<column>_at), which is what lets two
// partial aggregates of one session merge exactly whatever order they arrive
// in (see telemetryRollupStatement).
type telemetrySessionLatestField struct {
	column, expr, filter string
}

var telemetrySessionLatestFields = []telemetrySessionLatestField{
	{"operating_system", "os_label", "os_label IS NOT NULL"},
	{"device_manufacturer", "device_manufacturer", "device_manufacturer IS NOT NULL"},
	{"device_model", "device_model", "device_model IS NOT NULL"},
	{"app_version", "app_version", "app_version IS NOT NULL"},
	{"country", "country", "country IS NOT NULL"},
	{"city", "city", "city IS NOT NULL"},
	{"organization", "organization", "organization IS NOT NULL"},
	{"asn", "asn", "asn IS NOT NULL"},
	{"isp", "isp", "isp IS NOT NULL"},
	// Failure fields come only from connection_failed events; latest
	// non-empty wins, matching sessionSummary's per-field accumulation.
	{"failure_stage", "failure_stage", "event = 'connection_failed' AND failure_stage IS NOT NULL"},
	{"failure_reason", "failure_reason", "event = 'connection_failed' AND failure_reason IS NOT NULL"},
	{"failure_detail", "failure_detail", "event = 'connection_failed' AND failure_detail IS NOT NULL"},
	{"observed_client_ip", "source_ip", "event = 'client_seen' AND source_ip IS NOT NULL"},
	{"reported_client_ip", "reported_client_ip", "reported_client_ip IS NOT NULL"},
	{"fallback_source_ip", "source_ip", "source_ip IS NOT NULL"},
}

// telemetrySessionOrderColumns are the rollup-only ordering columns of
// telemetry_sessions: the (received_at, occurred_at) of the event that
// supplied a value — first_* for the first-client rule, relay_* for the
// relay/class pair, ended_* for the final duration, and <field>_* for each
// latest field. NULL wherever the corresponding value is blank. Both halves
// are kept because the canonical event order is (received_at, occurred_at):
// an upload's events share a received_at, and an upload can straddle an
// insert chunk boundary, so a receipt-time tie between two partial aggregates
// must still resolve on occurred_at exactly as one aggregate would.
var telemetrySessionOrderColumns = func() string {
	columns := []string{"first_seen_at", "first_occurred_at", "relay_at", "relay_occurred_at", "ended_at", "ended_occurred_at"}
	for _, field := range telemetrySessionLatestFields {
		columns = append(columns, field.column+"_at", field.column+"_occurred_at")
	}
	return strings.Join(columns, ", ")
}()

// telemetrySessionIPFields are the IP-level latest fields of
// telemetry_sessions; PruneTelemetry blanks them on sessions older than the
// raw-event retention (telemetrySessionBlankIPStatement). organization and asn
// go with isp, which the ISP label falls back to (telemetrySessionISPLabel).
var telemetrySessionIPFields = []string{"observed_client_ip", "reported_client_ip", "fallback_source_ip", "city", "isp", "organization", "asn"}

// telemetrySessionBlankIPStatement clears every IP-level field, and the
// ordering pair that supplied it, on sessions whose latest event was received
// before $1. A blank value carries NULL ordering columns exactly as one that
// was never reported, so a later event for the session still merges in. Rows
// already blank are skipped, so the daily prune rewrites only the newly aged.
var telemetrySessionBlankIPStatement = func() string {
	var set, nonEmpty []string
	for _, column := range telemetrySessionIPFields {
		set = append(set, column+" = ''", column+"_at = NULL", column+"_occurred_at = NULL")
		nonEmpty = append(nonEmpty, column+" <> ''")
	}
	return `UPDATE telemetry_sessions SET ` + strings.Join(set, ", ") +
		` WHERE last_seen_at < $1 AND (` + strings.Join(nonEmpty, " OR ") + `)`
}()

// telemetrySessionRowsCTEWith builds the per-session accumulation over the
// events CTE. The `latest non-empty wins` aggregations order by (received_at,
// occurred_at) as the canonical event order: the in-memory accumulator
// iterates records in arrival order, and rows from one uploaded batch share a
// received_at, so occurred_at breaks those ties. bytes_sent/bytes_received are
// cumulative per session, hence MAX; the GREATEST(..., 0) mirrors the
// accumulator never letting a negative report beat its zero initial value.
// With ordering, the CTE also emits telemetrySessionOrderColumns for the
// rollup merge; the dashboard queries do without them.
func telemetrySessionRowsCTEWith(ordering bool) string {
	var b strings.Builder
	b.WriteString(`
session_rows AS (
	SELECT
		session_id,
		(array_agg(client_id ORDER BY received_at, occurred_at))[1] AS client_id,
		array_agg(DISTINCT client_id) AS client_ids,
		MIN(occurred_at) AS started_at,
		MAX(occurred_at) AS last_occurred_at,
		MAX(received_at) AS last_seen_at,
		COALESCE((array_agg(relay_id ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE relay_id <> ''))[1], '') AS relay_id,
		-- Keep the class paired with the same latest relay-bearing event. An empty
		-- class on that event must not borrow the class of an earlier failover relay.
		COALESCE((array_agg(relay_node_class ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE relay_id <> ''))[1], '') AS relay_node_class,
`)
	for _, field := range telemetrySessionLatestFields {
		fmt.Fprintf(&b, "\t\tCOALESCE((array_agg(%s ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE %s))[1], '') AS %s,\n", field.expr, field.filter, field.column)
	}
	b.WriteString(`		MAX(received_at) FILTER (WHERE event = 'session_heartbeat') AS last_heartbeat_at,
		GREATEST(COALESCE(MAX(session_duration_ms) FILTER (WHERE event = 'session_heartbeat'), 0), 0) AS running_duration_ms,
		COALESCE((array_agg(COALESCE(session_duration_ms, 0) ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE event = 'connection_ended'))[1], 0) AS ended_duration_ms,
		GREATEST(COALESCE(MAX(bytes_sent), 0), 0) AS bytes_sent,
		GREATEST(COALESCE(MAX(bytes_received), 0), 0) AS bytes_received,
		bool_or(event = 'connection_attempted') AS attempted,
		bool_or(event = 'connection_succeeded') AS succeeded,
		bool_or(event = 'connection_failed') AS failed,
		bool_or(event IN ('connection_failed', 'connection_ended', 'tunnel_stopped')) AS terminal`)
	if ordering {
		// MAX(received_at) under a field's filter is exactly the received_at of
		// the event its array_agg ordering picked; its occurred_at comes from
		// the same ordering.
		b.WriteString(`,
		MIN(received_at) AS first_seen_at,
		(array_agg(occurred_at ORDER BY received_at, occurred_at))[1] AS first_occurred_at`)
		for _, order := range append([]telemetrySessionLatestField{
			{"relay", "", "relay_id <> ''"},
			{"ended", "", "event = 'connection_ended'"},
		}, telemetrySessionLatestFields...) {
			fmt.Fprintf(&b, ",\n\t\tMAX(received_at) FILTER (WHERE %s) AS %s_at", order.filter, order.column)
			fmt.Fprintf(&b, ",\n\t\t(array_agg(occurred_at ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE %s))[1] AS %s_occurred_at", order.filter, order.column)
		}
	}
	b.WriteString(`
	FROM events
	GROUP BY session_id
)`)
	return b.String()
}

var (
	telemetrySessionRowsCTE       = telemetrySessionRowsCTEWith(false)
	telemetrySessionRollupRowsCTE = telemetrySessionRowsCTEWith(true)
)

// telemetrySessionsCTE derives the window-dependent activity flag on top of
// session_rows, whichever source produced them.
const telemetrySessionsCTE = `
sessions AS (
	SELECT s.*, s.last_heartbeat_at > $4 AND NOT s.terminal AS active
	FROM session_rows s
)`

// telemetryOccurredHour is the UTC-aligned hour bucket the trend and the
// event-grain rollup key on (date_trunc on a timestamptz would otherwise
// follow the session time zone).
const telemetryOccurredHour = `date_trunc('hour', occurred_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`

const (
	telemetryFailureStageLabel  = `CASE WHEN btrim(COALESCE(failure_stage, '')) <> '' THEN failure_stage ELSE 'unknown' END`
	telemetryFailureReasonLabel = `CASE WHEN btrim(COALESCE(failure_reason, '')) <> '' THEN failure_reason ELSE 'unknown' END`
)

// telemetryEventCountsCTE reduces the events CTE to the hourly event-grain
// shape telemetry_event_counts stores, one row per (hour, kind, k1, k2):
//
//	trend       k1 = event                       n = events in the hour
//	failure     k1 = stage, k2 = reason labels   n = connection_failed events
//	access      k1 = access_outcome              n = terminal connect events carrying it
//	relay_source k1 = relay_source               n = connection_succeeded events carrying it
//	relay_ok    k1 = relay_id                    n = connection_succeeded + relay_failover
//	relay_fail  k1 = relay_id, k2 = reason label n = relay_attempt_failed
//	speed       k1 = relay_id                    n = tests, s1 = Σ download_mbps_milli, s2 = Σ ttfb_ms
//	relay_seen  k1 = relay_id                    n = relay-bearing events, last_at = latest
//	                                             received_at, v = latest non-empty node class,
//	                                             v_at / v_occurred_at = (received_at, occurred_at)
//	                                             of the event that stamped it
//
// The stage/reason labels fall back to 'unknown' exactly as the in-memory
// firstNonEmpty(..., "unknown") does, so panels can group on them directly.
// (v_at, v_occurred_at) is what orders the retained class across hours — by
// receipt then occurrence, exactly the accumulator's latest-non-empty rule,
// not by the hour an event claims to have occurred in.
const telemetryEventCountsCTE = `
event_counts AS (
	SELECT ` + telemetryOccurredHour + ` AS hour, 'trend'::text AS kind, event AS k1, ''::text AS k2,
		COUNT(*)::bigint AS n, 0::bigint AS s1, 0::bigint AS s2, NULL::timestamptz AS last_at, NULL::text AS v, NULL::timestamptz AS v_at, NULL::timestamptz AS v_occurred_at
	FROM events WHERE event IN ('connection_attempted', 'connection_succeeded', 'connection_failed') GROUP BY 1, 3
	UNION ALL
	SELECT ` + telemetryOccurredHour + `, 'failure', ` + telemetryFailureStageLabel + `, ` + telemetryFailureReasonLabel + `,
		COUNT(*), 0, 0, NULL, NULL, NULL, NULL
	FROM events WHERE event = 'connection_failed' GROUP BY 1, 3, 4
	UNION ALL
	SELECT ` + telemetryOccurredHour + `, 'access', access_outcome, '', COUNT(*), 0, 0, NULL, NULL, NULL, NULL
	FROM events WHERE event IN ('connection_succeeded', 'connection_failed') AND access_outcome IS NOT NULL GROUP BY 1, 3
	UNION ALL
	SELECT ` + telemetryOccurredHour + `, 'relay_source', relay_source, '', COUNT(*), 0, 0, NULL, NULL, NULL, NULL
	FROM events WHERE event = 'connection_succeeded' AND relay_source IS NOT NULL GROUP BY 1, 3
	UNION ALL
	SELECT ` + telemetryOccurredHour + `, 'relay_ok', relay_id, '', COUNT(*), 0, 0, NULL, NULL, NULL, NULL
	FROM events WHERE event IN ('connection_succeeded', 'relay_failover') AND relay_id <> '' GROUP BY 1, 3
	UNION ALL
	SELECT ` + telemetryOccurredHour + `, 'relay_fail', relay_id, ` + telemetryFailureReasonLabel + `, COUNT(*), 0, 0, NULL, NULL, NULL, NULL
	FROM events WHERE event = 'relay_attempt_failed' AND relay_id <> '' GROUP BY 1, 3, 4
	UNION ALL
	SELECT ` + telemetryOccurredHour + `, 'speed', relay_id, '', COUNT(*),
		SUM(COALESCE(download_mbps_milli, 0))::bigint, SUM(COALESCE(ttfb_ms, 0))::bigint, NULL, NULL, NULL, NULL
	FROM events WHERE event = 'speed_test_completed' AND relay_id <> '' GROUP BY 1, 3
	UNION ALL
	SELECT ` + telemetryOccurredHour + `, 'relay_seen', relay_id, '', COUNT(*), 0, 0, MAX(received_at),
		(array_agg(relay_node_class ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE relay_node_class <> ''))[1],
		MAX(received_at) FILTER (WHERE relay_node_class <> ''),
		(array_agg(occurred_at ORDER BY received_at DESC, occurred_at DESC) FILTER (WHERE relay_node_class <> ''))[1]
	FROM events WHERE relay_id <> '' GROUP BY 1, 3
)`

// telemetryEventPathCTEs is the event path's preamble: everything the shared
// bodies need, derived from the window's events.
var telemetryEventPathCTEs = `WITH ` + telemetryEventsCTE + `, ` + telemetrySessionRowsCTE + `, ` + telemetryEventCountsCTE + `, ` + telemetrySessionsCTE

// telemetryRollupSessionsWhere is the rollup path's window membership: a
// session belongs to the window when its latest event does (last_seen_at > $1
// mirrors the event path's partition-pruning bound, which every in-window
// event satisfies).
const telemetryRollupSessionsWhere = `WHERE last_seen_at > $1 AND last_occurred_at >= $2 AND started_at <= $3`

// telemetryRollupAggregateColumns are the session columns the overview and
// relay-stats bodies read. The rollup preamble selects only these: the
// sessions CTE is materialized and rescanned once per panel, which on a 31-day
// window dominates the query unless its rows stay narrow.
const telemetryRollupAggregateColumns = `client_id, client_ids, relay_id, operating_system,
	country, city, organization, asn, isp, last_heartbeat_at, attempted, succeeded, failed, terminal`

// telemetryRollupPathCTEs is the rollup path's preamble, reading the same two
// shapes from the tables ingestion maintains. The event-grain window edge is
// hour-granular: the truncated start hour is included whole, as for
// top_applications.
var telemetryRollupPathCTEs = `WITH
session_rows AS (
	SELECT ` + telemetryRollupAggregateColumns + `
	FROM telemetry_sessions
	` + telemetryRollupSessionsWhere + `
),
event_counts AS (
	SELECT hour, kind, k1, k2, n, s1, s2, last_at, v, v_at, v_occurred_at
	FROM telemetry_event_counts
	WHERE hour >= date_trunc('hour', $2::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AND hour <= $3
), ` + telemetrySessionsCTE

// isp precedence mirrors firstNonEmpty(isp, organization, asn).
const telemetrySessionISPLabel = `
	CASE
		WHEN btrim(isp) <> '' THEN isp
		WHEN btrim(organization) <> '' THEN organization
		WHEN btrim(asn) <> '' THEN asn
		ELSE ''
	END`

// telemetryOverviewBody collapses the WHOLE overview — headline totals, every
// session-grain and event-grain count panel, trend, relay failure reasons,
// retained relay classes, and speed tests — into a single statement over the
// `sessions` and `event_counts` CTEs, whichever preamble produced them. Rows
// are discriminated by `kind` and share a generic column shape so
// heterogeneous panels can travel together:
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
const telemetryOverviewBody = `
-- totals: one row per headline metric. clients counts every distinct client
-- any in-window session named (a client seen only outside a session still
-- keys its own session), matching the in-memory clients set; the rest reduce
-- the sessions CTE, and active_clients is the distinct-client count among
-- active sessions. Both distinct counts are written so the planner can hash
-- (SELECT DISTINCT) and filter first (WHERE, not FILTER) rather than sort every
-- session of a 31-day window, and clients reads sessions rather than
-- session_rows so session_rows is not materialized a second time.
SELECT 'totals'::text AS kind, 'clients'::text AS k1, NULL::text AS k2,
	(SELECT COUNT(*) FROM (SELECT DISTINCT c FROM sessions CROSS JOIN LATERAL unnest(sessions.client_ids) AS c) AS distinct_clients)::bigint AS v1,
	NULL::bigint AS v2, NULL::bigint AS v3, NULL::bigint AS v4
UNION ALL
SELECT 'totals', 'sessions', NULL, COUNT(*), NULL, NULL, NULL FROM sessions
UNION ALL
SELECT 'totals', 'attempts', NULL, COUNT(*) FILTER (WHERE attempted), NULL, NULL, NULL FROM sessions
UNION ALL
SELECT 'totals', 'successes', NULL, COUNT(*) FILTER (WHERE succeeded), NULL, NULL, NULL FROM sessions
UNION ALL
SELECT 'totals', 'failures', NULL, COUNT(*) FILTER (WHERE failed), NULL, NULL, NULL FROM sessions
UNION ALL
SELECT 'totals', 'active_clients', NULL, COUNT(DISTINCT client_id), NULL, NULL, NULL FROM sessions WHERE active
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
-- trend: hourly attempt/success/failure counts. The hour rides in v1 as epoch
-- seconds; the caller rebuilds it and adds it by index into the pre-filled
-- bucket slice, folding hours into days on the long windows.
SELECT 'trend', NULL, NULL,
	extract(epoch FROM hour)::bigint,
	COALESCE(SUM(n) FILTER (WHERE k1 = 'connection_attempted'), 0)::bigint,
	COALESCE(SUM(n) FILTER (WHERE k1 = 'connection_succeeded'), 0)::bigint,
	COALESCE(SUM(n) FILTER (WHERE k1 = 'connection_failed'), 0)::bigint
FROM event_counts WHERE kind = 'trend' GROUP BY hour
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
SELECT 'failure_stages', k1, NULL, SUM(n)::bigint, NULL, NULL, NULL
FROM event_counts WHERE kind = 'failure' GROUP BY k1
UNION ALL
SELECT 'failure_reasons', k1 || ' · ' || k2, NULL, SUM(n)::bigint, NULL, NULL, NULL
FROM event_counts WHERE kind = 'failure' GROUP BY 2
UNION ALL
-- access_outcomes / relay_sources: the per-user access path, counted over the
-- terminal connect events exactly as the in-memory accumulator does (an
-- event without the attribute — a client predating it — counts nowhere).
SELECT 'access_outcomes', k1, NULL, SUM(n)::bigint, NULL, NULL, NULL
FROM event_counts WHERE kind = 'access' GROUP BY k1
UNION ALL
SELECT 'relay_sources', k1, NULL, SUM(n)::bigint, NULL, NULL, NULL
FROM event_counts WHERE kind = 'relay_source' GROUP BY k1
UNION ALL
SELECT 'relay_successes', k1, NULL, SUM(n)::bigint, NULL, NULL, NULL
FROM event_counts WHERE kind = 'relay_ok' GROUP BY k1
UNION ALL
SELECT 'relay_failures', k1, NULL, SUM(n)::bigint, NULL, NULL, NULL
FROM event_counts WHERE kind = 'relay_fail' GROUP BY k1
UNION ALL
-- relay_failure_reasons: per (relay_id, reason) counts feeding topFailureReason;
-- ties resolve lexicographically in Go.
SELECT 'relay_failure_reasons', k1, k2, SUM(n)::bigint, NULL, NULL, NULL
FROM event_counts WHERE kind = 'relay_fail' GROUP BY k1, k2
UNION ALL
-- relay_classes: the broker-attested class retained with any event for this
-- immutable relay ID. The latest non-empty value covers mixed legacy/new
-- telemetry without splitting the relay's aggregate counts.
SELECT 'relay_classes', k1,
	(array_agg(v ORDER BY v_at DESC, v_occurred_at DESC) FILTER (WHERE v IS NOT NULL))[1],
	NULL, NULL, NULL, NULL
FROM event_counts WHERE kind = 'relay_seen' GROUP BY k1
UNION ALL
-- speed_tests: per-relay test count plus the sums the caller averages. v1=tests,
-- v2=Σ download_mbps_milli, v3=Σ ttfb_ms.
SELECT 'speed_tests', k1, NULL, SUM(n)::bigint, SUM(s1)::bigint, SUM(s2)::bigint, NULL
FROM event_counts WHERE kind = 'speed' GROUP BY k1`

var (
	telemetryOverviewEventsQuery = telemetryEventPathCTEs + telemetryOverviewBody
	telemetryOverviewRollupQuery = telemetryRollupPathCTEs + telemetryOverviewBody
)

// telemetrySessionPageColumns is what the session page scans: the raw
// accumulation, finalized in Go. It is telemetrySessionColumns without the
// rollup-only client_ids and last_occurred_at.
const telemetrySessionPageColumns = `
	session_id, client_id, started_at, last_seen_at, relay_id, relay_node_class, operating_system,
	device_manufacturer, device_model, app_version, country, city, organization, asn, isp,
	failure_stage, failure_reason, failure_detail,
	observed_client_ip, reported_client_ip, fallback_source_ip,
	last_heartbeat_at, running_duration_ms, ended_duration_ms, bytes_sent, bytes_received,
	attempted, succeeded, failed, terminal`

// telemetrySessionPageEventsQuery pages sessions in two phases so the
// expensive part only ever touches one page. The `page` CTE ranks the window's
// sessions with a slim scan that never reads the payload — (last_seen_at DESC,
// session_id), the same order the in-memory querier sorts by, with
// MAX(received_at) exactly the sessions CTE's last_seen_at — and applies
// LIMIT/OFFSET there. Only the surviving page's events then flow through the
// payload extraction and the per-session array_agg work, via the (session_id,
// occurred_at) index. The COUNT(*) OVER () window function is evaluated after
// GROUP BY but before LIMIT/OFFSET, so ranked_total is the window's full
// session count riding on every returned row. It is absent only when the page
// itself is empty (offset at/past the end, or a window with no sessions),
// which the caller handles by falling back to the standalone count. The page
// queries take the three event-window parameters plus $4 limit and $5 offset.
var telemetrySessionPageEventsQuery = `WITH page AS (
	SELECT
		session_id,
		MAX(received_at) AS ranked_last_seen,
		COUNT(*) OVER () AS ranked_total
	FROM telemetry_events
	WHERE received_at > $1 AND occurred_at >= $2 AND occurred_at <= $3
		AND event <> 'application_connection'
	GROUP BY session_id
	ORDER BY ranked_last_seen DESC, session_id
	LIMIT $4 OFFSET $5
), ` + telemetryEventsCTEFrom(`FROM telemetry_events e JOIN page USING (session_id)`+telemetryWindowEventsWhere) + `, ` + telemetrySessionRowsCTE + `
SELECT ` + telemetrySessionPageColumns + `,
	page.ranked_total AS total_count
FROM session_rows
JOIN page USING (session_id)
ORDER BY last_seen_at DESC, session_id`

// telemetrySessionPageRollupQuery is the same page straight off
// telemetry_sessions, with the window's total as a scalar subquery.
var telemetrySessionPageRollupQuery = `WITH session_rows AS (
	SELECT ` + telemetrySessionColumns + `
	FROM telemetry_sessions
	` + telemetryRollupSessionsWhere + `
	ORDER BY last_seen_at DESC, session_id
	LIMIT $4 OFFSET $5
)
SELECT ` + telemetrySessionPageColumns + `,
	(SELECT COUNT(*) FROM telemetry_sessions ` + telemetryRollupSessionsWhere + `)::bigint AS total_count
FROM session_rows
ORDER BY last_seen_at DESC, session_id`

// telemetrySessionCountEventsQuery is the empty-page fallback for the window
// count that the page query normally carries inline. It needs no payload
// fields, so it counts distinct sessions directly off the table; it takes the
// three event-window parameters only.
const telemetrySessionCountEventsQuery = `
SELECT COUNT(DISTINCT session_id)
FROM telemetry_events
WHERE received_at > $1 AND occurred_at >= $2 AND occurred_at <= $3
	AND event <> 'application_connection'`

const telemetrySessionCountRollupQuery = `SELECT COUNT(*) FROM telemetry_sessions ` + telemetryRollupSessionsWhere

// telemetryRelayStatsBody aggregates the window per relay for the relays
// page: event-grain counts (successes, failures, speed-test sums, last seen,
// retained class), session-grain counts (sessions, distinct clients, active
// sessions and clients), and the modal relay_attempt_failed reason.
// relay_events covers every relay any event named (every relay-bearing event
// leaves a relay_seen row), so the LEFT JOINs lose nothing. Only rows whose
// retained node_class is non-empty are returned — ingestion stamps the class
// solely for relays holding a live registration, so the filter is what keeps
// anonymous telemetry naming fabricated relay IDs from minting one row each
// (see relayTelemetryStats). The trailing sentinel row (relay_id = ”) carries
// the overall distinct count of clients active through any attested relay,
// which the per-relay rows cannot express — one client may be active on
// several relays.
const telemetryRelayStatsBody = `,
relay_events AS (
	SELECT
		k1 AS relay_id,
		COALESCE(SUM(n) FILTER (WHERE kind = 'relay_ok'), 0)::bigint AS successes,
		COALESCE(SUM(n) FILTER (WHERE kind = 'relay_fail'), 0)::bigint AS failures,
		COALESCE(SUM(n) FILTER (WHERE kind = 'speed'), 0)::bigint AS speed_tests,
		COALESCE(SUM(s1) FILTER (WHERE kind = 'speed'), 0)::bigint AS mbps_milli_sum,
		COALESCE(SUM(s2) FILTER (WHERE kind = 'speed'), 0)::bigint AS ttfb_ms_sum,
		MAX(last_at) AS last_seen_at,
		COALESCE((array_agg(v ORDER BY v_at DESC, v_occurred_at DESC) FILTER (WHERE kind = 'relay_seen' AND v IS NOT NULL))[1], '') AS node_class
	FROM event_counts
	WHERE kind IN ('relay_ok', 'relay_fail', 'speed', 'relay_seen')
	GROUP BY k1
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
		SELECT k1 AS relay_id, k2 AS reason, SUM(n) AS occurrences
		FROM event_counts
		WHERE kind = 'relay_fail'
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

var (
	telemetryRelayStatsEventsQuery = telemetryEventPathCTEs + telemetryRelayStatsBody
	telemetryRelayStatsRollupQuery = telemetryRollupPathCTEs + telemetryRelayStatsBody
)

// telemetryRollupStatement folds the events CTE built over `eventsFrom` into
// the two rollup tables in ONE statement: the session rows merge into
// telemetry_sessions and the hourly counts add into telemetry_event_counts.
// `prefix` opens the WITH list (a flush passes its INSERT ... RETURNING as the
// first CTE so the freshly inserted rows are the source; the backfill passes a
// bare WITH and a partition range). The data-modifying session CTE runs to
// completion whether or not the main INSERT reads it.
//
// The session merge reproduces the per-field "latest non-empty wins" rule
// incrementally and exactly, whatever order a session's partial aggregates
// arrive in: every such field carries the (received_at, occurred_at) of the
// event that supplied it (telemetrySessionOrderColumns), and the merge keeps
// whichever side's value is later in that order — the canonical event order
// the single-pass aggregation uses, so an upload split across insert chunks
// (its events share a received_at) resolves on occurred_at just as one
// aggregate would. So a backfill chunk never clobbers what live ingestion
// stored after it, and a later backfill chunk still overrides an earlier one
// even when a live event has already landed for the session. Monotone fields
// (started/last seen, heartbeat, maxima, flags) need no ordering; the first
// client follows the earliest (first_seen_at, first_occurred_at). client_ids
// is capped so anonymous telemetry naming fresh client IDs under one session
// cannot grow a row without bound; beyond the cap the headline clients total
// undercounts that session.
func telemetryRollupStatement(prefix, eventsFrom string) string {
	// later reports that the incoming side supplied the field later in event
	// order than the stored row did (or the stored row never did); the row
	// comparison is lexicographic, so equal receipt times fall through to
	// occurred_at. Ties on both go to the incoming side, which is only ever
	// a retried (identical) chunk.
	later := func(order string) string {
		return fmt.Sprintf(`EXCLUDED.%[1]s_at IS NOT NULL AND (t.%[1]s_at IS NULL OR (EXCLUDED.%[1]s_at, EXCLUDED.%[1]s_occurred_at) >= (t.%[1]s_at, t.%[1]s_occurred_at))`, order)
	}
	// pick keeps the later side's value and ordering columns together.
	pick := func(order string, columns ...string) string {
		var parts []string
		for _, column := range append(columns, order+"_at", order+"_occurred_at") {
			parts = append(parts, fmt.Sprintf(`%s = CASE WHEN %s THEN EXCLUDED.%s ELSE t.%s END`, column, later(order), column, column))
		}
		return strings.Join(parts, ",\n\t\t")
	}
	greatest := func(column string) string {
		return fmt.Sprintf(`%s = GREATEST(t.%s, EXCLUDED.%s)`, column, column, column)
	}
	either := func(column string) string {
		return fmt.Sprintf(`%s = t.%s OR EXCLUDED.%s`, column, column, column)
	}
	first := `(EXCLUDED.first_seen_at, EXCLUDED.first_occurred_at) < (t.first_seen_at, t.first_occurred_at)`
	sets := []string{
		`client_id = CASE WHEN ` + first + ` THEN EXCLUDED.client_id ELSE t.client_id END`,
		`first_seen_at = CASE WHEN ` + first + ` THEN EXCLUDED.first_seen_at ELSE t.first_seen_at END`,
		`first_occurred_at = CASE WHEN ` + first + ` THEN EXCLUDED.first_occurred_at ELSE t.first_occurred_at END`,
		fmt.Sprintf(`client_ids = CASE WHEN cardinality(t.client_ids) >= %d THEN t.client_ids
			ELSE ARRAY(SELECT DISTINCT c FROM unnest(t.client_ids || EXCLUDED.client_ids) AS c) END`, maxTelemetrySessionClientIDs),
		`started_at = LEAST(t.started_at, EXCLUDED.started_at)`,
		greatest("last_occurred_at"),
		greatest("last_seen_at"),
		// The class stays paired with the relay_id that wins.
		pick("relay", "relay_id", "relay_node_class"),
	}
	for _, field := range telemetrySessionLatestFields {
		sets = append(sets, pick(field.column, field.column))
	}
	sets = append(sets,
		greatest("last_heartbeat_at"),
		greatest("running_duration_ms"),
		pick("ended", "ended_duration_ms"),
		greatest("bytes_sent"),
		greatest("bytes_received"),
		either("attempted"),
		either("succeeded"),
		either("failed"),
		either("terminal"),
	)
	columns := telemetrySessionColumns + `, ` + telemetrySessionOrderColumns
	laterStamp := `EXCLUDED.v_at IS NOT NULL AND (c.v_at IS NULL OR (EXCLUDED.v_at, EXCLUDED.v_occurred_at) >= (c.v_at, c.v_occurred_at))`
	return prefix + telemetryEventsCTEFrom(eventsFrom) + `, ` + telemetrySessionRollupRowsCTE + `, ` + telemetryEventCountsCTE + `,
rolled_sessions AS (
	INSERT INTO telemetry_sessions AS t (` + columns + `)
	SELECT ` + columns + ` FROM session_rows
	ON CONFLICT (session_id) DO UPDATE SET
		` + strings.Join(sets, ",\n\t\t") + `
	RETURNING 1
)
INSERT INTO telemetry_event_counts AS c (hour, kind, k1, k2, n, s1, s2, last_at, v, v_at, v_occurred_at)
SELECT hour, kind, k1, k2, n, s1, s2, last_at, v, v_at, v_occurred_at FROM event_counts
ON CONFLICT (hour, kind, k1, k2) DO UPDATE SET
	n = c.n + EXCLUDED.n,
	s1 = c.s1 + EXCLUDED.s1,
	s2 = c.s2 + EXCLUDED.s2,
	last_at = GREATEST(c.last_at, EXCLUDED.last_at),
	-- The retained class follows the later stamp, like the session fields.
	v = CASE WHEN ` + laterStamp + ` THEN EXCLUDED.v ELSE c.v END,
	v_at = CASE WHEN ` + laterStamp + ` THEN EXCLUDED.v_at ELSE c.v_at END,
	v_occurred_at = CASE WHEN ` + laterStamp + ` THEN EXCLUDED.v_occurred_at ELSE c.v_occurred_at END`
}

// telemetryRollupInsertStatement wraps the multi-row event INSERT so the same
// statement rolls its rows up: no second read of the batch, no window in which
// the events exist but the rollup lags, and a failed flush leaves neither.
func telemetryRollupInsertStatement(rows int) string {
	return telemetryRollupStatement(`WITH ins AS (`+telemetryInsertStatement(rows)+` RETURNING *), `, `FROM ins e`)
}

// telemetryRollupBackfillStatement folds one received_at range of stored
// events ($1 inclusive to $2 exclusive) into the rollup; the backfill walks
// the ranges upwards an hour at a time (see rollupBackfillChunk).
var telemetryRollupBackfillStatement = telemetryRollupStatement(`WITH `, `FROM telemetry_events e
	WHERE e.received_at >= $1 AND e.received_at < $2
		AND e.event <> 'application_connection'`)

// telemetryWindowArgs is the shared parameter list documented on the CTEs.
// Queries that only touch the events CTE must take eventArgs — Postgres
// rejects bound parameters a statement never references.
func telemetryWindowArgs(now time.Time, window time.Duration) (eventArgs, sessionArgs []any) {
	start := now.Add(-window)
	sessionArgs = []any{start.Add(-maxTelemetryFutureSkew), start, now, now.Add(-activeSessionTimeout)}
	return sessionArgs[:3], sessionArgs
}

// TelemetryOverview implements TelemetryQuerier by aggregating the window in
// Postgres with one statement. Only per-group counts travel back to Go, so
// response size tracks the diversity of the window, not its event count. The
// sessions themselves come from the dedicated sessions endpoint, so the
// overview no longer queries or returns them.
func (s *PostgresTelemetrySink) TelemetryOverview(now time.Time, window time.Duration) (telemetryOverview, error) {
	if err := s.flush(); err != nil {
		slog.Error("could not flush telemetry before read", "error", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresTelemetryQueryTimeout)
	defer cancel()
	_, sessionArgs := telemetryWindowArgs(now, window)

	query := telemetryOverviewEventsQuery
	if s.useRollup(window) {
		query = telemetryOverviewRollupQuery
	}
	trend, counts, relayFailureReasons, relayClasses, speedTests, err := s.queryTelemetryOverviewAggregates(ctx, query, sessionArgs, now, window)
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
	overview.ActiveOS = sortedCounts(osFamilyCounts(counts["active_by_os"]), 10)
	overview.FailureStages = sortedCounts(counts["failure_stages"], 10)
	overview.FailureReasons = sortedCounts(counts["failure_reasons"], 10)
	overview.AccessOutcomes = sortedCounts(counts["access_outcomes"], accessPanelLimit)
	overview.RelaySources = sortedCounts(counts["relay_sources"], accessPanelLimit)
	overview.TopRelays = topRelaySummaries(counts["relay_successes"], counts["relay_failures"], relayFailureReasons)
	overview.SpeedTests = speedTests
	applyTelemetryRelayClasses(&overview, relayClasses)
	return overview, nil
}

// TelemetrySessions implements TelemetryQuerier with LIMIT/OFFSET pagination.
// The page query ranks sessions with a payload-free scan (or, on the rollup
// path, straight off telemetry_sessions), aggregates only the requested page,
// and carries the window's total session count inline, so the common case
// runs a single statement; only an empty page falls back to a standalone
// count for the handler's offset clamp.
func (s *PostgresTelemetrySink) TelemetrySessions(now time.Time, window time.Duration, offset, limit int) ([]sessionSummary, int, error) {
	if err := s.flush(); err != nil {
		slog.Error("could not flush telemetry before read", "error", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresTelemetryQueryTimeout)
	defer cancel()
	eventArgs, _ := telemetryWindowArgs(now, window)

	pageQuery, countQuery := telemetrySessionPageEventsQuery, telemetrySessionCountEventsQuery
	if s.useRollup(window) {
		pageQuery, countQuery = telemetrySessionPageRollupQuery, telemetrySessionCountRollupQuery
	}
	page, total, err := s.queryTelemetrySessionPage(ctx, pageQuery, eventArgs, now, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	if len(page) == 0 {
		// An empty page carries no total row, so fetch the count directly —
		// the handler's offset>total clamp still needs the real count. Matches
		// the in-memory querier, which reports the full count even when the
		// requested offset lands past the end.
		page = []sessionSummary{}
		if err := s.pool.QueryRow(ctx, countQuery, eventArgs...).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("count telemetry sessions: %w", err)
		}
	}
	return page, total, nil
}

// TelemetryRelayStats implements TelemetryQuerier with one statement: the
// window's sessions and event counts each materialize once and only per-relay
// rows (plus the sentinel totals row) travel back to Go.
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

	query := telemetryRelayStatsEventsQuery
	if s.useRollup(window) {
		query = telemetryRelayStatsRollupQuery
	}
	rows, err := s.pool.Query(ctx, query, append(append([]any{}, sessionArgs...), registeredIDs)...)
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
// expects: the trend (pre-filled with empty buckets so gaps render as
// zeros), the totals and count groups, per-relay failure-reason counts and
// retained classes, and the speed-test summaries.
func (s *PostgresTelemetrySink) queryTelemetryOverviewAggregates(ctx context.Context, query string, args []any, now time.Time, window time.Duration) (trend []trendPoint, counts, relayFailureReasons map[string]map[string]int, relayClasses map[string]string, speedTests []speedTestSummary, err error) {
	// Pre-fill every bucket like the in-memory path so the trend spans the
	// whole window even where no events landed; the hourly rows add into their
	// bucket by index (one each, or a UTC day's worth on the long windows).
	width := telemetryTrendBucket(window)
	first := now.Add(-window).Truncate(width)
	for bucket := first; !bucket.After(now); bucket = bucket.Add(width) {
		trend = append(trend, trendPoint{Time: bucket})
	}
	counts = make(map[string]map[string]int)
	relayFailureReasons = make(map[string]map[string]int)
	relayClasses = make(map[string]string)

	rows, err := s.pool.Query(ctx, query, args...)
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
			// v1 is the hour as epoch seconds; reconstruct the instant and add it
			// by index into its pre-filled bucket.
			hour := time.Unix(int64Value(v1), 0).UTC()
			index := int(hour.Truncate(width).Sub(first) / width)
			if index < 0 || index >= len(trend) {
				continue
			}
			trend[index].Attempts += int(int64Value(v2))
			trend[index].Successes += int(int64Value(v3))
			trend[index].Failures += int(int64Value(v4))
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
// and the window's total session count. The total rides on each row, so it is
// only meaningful when the page is non-empty; the caller supplies the count
// for an empty page.
func (s *PostgresTelemetrySink) queryTelemetrySessionPage(ctx context.Context, query string, args []any, now time.Time, limit, offset int) ([]sessionSummary, int, error) {
	rows, err := s.pool.Query(ctx, query, append(append([]any{}, args...), limit, offset)...)
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
