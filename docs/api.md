# Broker API

All API paths are currently versioned under `/api/v1`.

## Abuse limits

The unauthenticated endpoints (`GET /api/v1/relays`, `POST
/api/v1/wss/tickets`, `POST /api/v1/users/challenge`, `POST
/api/v1/users/auth`, `POST /api/v1/telemetry/events`, and `GET
/api/v1/speed-test`) and `POST /api/v1/access` are rate limited. Requests over the budget receive `429
Too Many Requests` with a `Retry-After` header (seconds); clients should back
off and retry only within their bounded operation deadline. Most endpoints use
the real client IP resolved through trusted proxies. WSS ticket requests also
use a bounded, hashed `X-OpenRung-Client-ID` component when it is well formed,
so legitimate clients behind one carrier-grade NAT address do not all consume
one bucket. That identifier is not authentication, so relay-side admission
limits remain mandatory.

Each of these per-client budgets is broker-host configuration, set as
`<per-second rate>:<burst>` in `OPENRUNG_RATE_LIMIT_RELAY_LIST`,
`OPENRUNG_RATE_LIMIT_WSS_TICKET`, `OPENRUNG_RATE_LIMIT_USER_AUTH`,
`OPENRUNG_RATE_LIMIT_ACCESS`, `OPENRUNG_RATE_LIMIT_TELEMETRY`,
`OPENRUNG_RATE_LIMIT_SPEED_TEST` and `OPENRUNG_RATE_LIMIT_RELAY_REGISTRATION`
(registration, heartbeat and lease polling share one budget). Clients must not
assume any particular budget; they react only to `429` and `Retry-After`.

The credentialed operational endpoint `GET /admin/api/relays/inventory` is rate
limited the same way, on a tighter budget: authentication bounds who may call
it, not how often, and each call costs a full unbounded read of the relay store.

## Client telemetry

Android clients attach a persistent installation identifier and a per-connection
session identifier when fetching relays:

```http
X-OpenRung-Client-ID: 7e29747c-8b52-4d50-a79f-6d82db653bdb
X-OpenRung-Session-ID: d9943d4f-fd59-4fb8-a170-ed8af949ccee
X-OpenRung-App-Version: 0.1.0
X-OpenRung-Android-API: 35
```

The broker writes a `client_seen` record containing the request source IP before
returning the relay list, at most once per client session every four minutes —
repeat polling inside that window does not produce more records. The default
broker command stores telemetry as JSON Lines in `openrung-telemetry.jsonl`;
change the path with `-telemetry-file`. The file is compacted in place once it
outgrows its size budget (256 MiB), keeping the retained recent records, so disk
usage stays bounded.

When the request arrives through a trusted proxy, the source IP is taken from the
`CF-Connecting-IP` header (falling back to the left-most `X-Forwarded-For` entry)
rather than the immediate peer, so requests fronted by `broker.openrung.org` still
record the real client IP. Trusted proxies are exactly the CIDRs in
`OPENRUNG_TRUSTED_PROXY_CIDRS` (comma-separated; none by default). Forwarded
headers from any other peer are ignored, so a direct connection to the origin cannot
spoof the source IP.

```http
POST /api/v1/telemetry/events
Content-Type: application/json
```

```json
{
  "events": [
    {
      "schema_version": 1,
      "event_id": "f82e6bf5-947b-4b6b-87d5-e1546a90ee22",
      "event": "application_connection",
      "occurred_at": "2026-06-20T18:30:00Z",
      "client_id": "7e29747c-8b52-4d50-a79f-6d82db653bdb",
      "session_id": "d9943d4f-fd59-4fb8-a170-ed8af949ccee",
      "application_package": "com.android.chrome",
      "application_uid": 10241,
      "measurements": {
        "connection_count": 237
      }
    }
  ]
}
```

The endpoint accepts at most 200 events and 512 KiB per request. Events dated
more than one hour into the future are rejected (retention and dashboards key
off the server's receipt time, so client clocks cannot extend either), and
free-form fields are length-capped: attribute/measurement keys at 64
characters, attribute values and `application_package` at 256. Every string
field must be valid UTF-8 without NUL characters — values a Postgres store
could not hold — or the batch is rejected with `400`.

Events that carry a `relay_id` are checked against the broker's own record of
minted relay identities. An event referencing a relay ID that is neither
currently registered nor was registered at any point in the telemetry
retention window (7 days) is silently discarded — such an ID cannot have
appeared in any signed relay list, so the event is fabricated, and storing it
would hand dashboards and relay ranking an unbounded attacker-controlled key
space. The response's `accepted` count reflects only stored events. Failure
reports about a relay that recently went offline are unaffected: its ID stays
known for the full retention window after its last registration or heartbeat,
and such events are stored with the node class the broker attested at that
last lease, so the relay keeps its dashboard attribution after its descriptor
expires.

Package attribution is collected on Android 10 and newer. Aggregating Android clients
skip DNS, collapse repeated flows per application, and omit destination details.
Legacy schema-version-1 clients may still send one event per flow and include
destination fields; the broker accepts those fields but never retains them.

For an `application_connection`, the optional integer
`measurements.connection_count` says how many flows the event represents.
Missing values count as `1` for legacy compatibility. Present integer values
outside `1..100,000` also fall back to `1`, avoiding both a permanently retrying
bad outbox item and maximum weighting for malformed input; non-integer values
are rejected with the rest of an invalid JSON batch. Across one HTTP batch, all
events for the same application can contribute at most `100,000`. The whole
accepted count is assigned to the server receipt hour; it is a dashboard-grade
approximation, not an exact per-flow timeline, and at-least-once client retries
can duplicate counts.

`application_connection` events are never stored as individual records — in
either telemetry store. At ingestion the broker adds the bounded represented
flow count to an hourly per-application total (Postgres: the
`telemetry_app_counts` table; JSONL mode: an in-memory rollup that restarts
empty) and discards the rest of the event, so destination and per-event client
metadata never reach disk. Only the aggregate feeds the dashboard's
top-applications panel, whose window edge is consequently hour-granular. These
were historically by far the highest-volume events, so client aggregation and
the broker rollup keep dashboard queries bounded by session-grade volume.

Before starting the tunnel, Android also resolves its public IP metadata and
adds `client_ip`, `country`, `country_code`, `city`, `asn`, `isp`, and
`organization` to subsequent session events. A failed metadata lookup does not
prevent a VPN connection.

While connected, Android sends a best-effort `session_heartbeat` immediately
and then every 50 to 70 seconds. Heartbeats include the active relay, device and
network attributes, geo/ISP metadata when available, and session/connection
durations. They bypass the durable client outbox so a delayed retry cannot make
an ended session appear active. Each heartbeat request also carries queued
ordinary telemetry when available. The broker considers a session active for
150 seconds after its latest received heartbeat unless a terminal event has
arrived.

Clients that can measure tunneled traffic add cumulative `bytes_sent`
(client-to-relay upload) and `bytes_received` (download) measurements to
`session_heartbeat` and `connection_ended` events. The dashboard takes the
largest value reported for a session, so out-of-order delivery cannot shrink
the total; sessions from clients that do not report traffic simply omit the
counters.

The JSONL sink updates the dashboard's in-memory records immediately, flushes
buffered JSONL output every five seconds, syncs it to durable storage every 30
seconds, and performs a final flush and sync during graceful broker shutdown.

The Settings screen offers a manual relay speed test. It downloads from the
configured broker, avoiding a dependency on external DNS while the VPN is
active. It performs a 1 MB warm-up followed by a measured 10 MB download through
the active tunnel and emits `speed_test_completed` with the active `relay_id`
and these measurements:

```json
{
  "bytes_downloaded": 10000000,
  "download_duration_ms": 2140,
  "time_to_first_byte_ms": 182,
  "download_mbps_milli": 37383
}
```

`download_mbps_milli` stores Mbps multiplied by 1,000, so `37383` represents
37.383 Mbps.

```http
GET /api/v1/speed-test?bytes=10000000
```

The broker streams the requested number of bytes with caching disabled and
rejects requests larger than 25 MB. Because each stream is expensive for broker
egress, only a few speed tests may run concurrently in addition to the per-IP
rate limit; a busy broker answers `429` with `Retry-After`, and the app should
retry the measurement later rather than treat it as a failure.

### WSS handshake failure taxonomy

A `transport_failed` event with `transport=wss` and `failure_stage=wss_handshake`
carries a `failure_reason` from wsscore's closed classification set. wsscore
classifies the dial at the one point where the typed error and the CDN response
status both still exist, then destroys everything else, so the token is the only
failure information any client can report. The full registry, with the meaning
and the quasi-identifier notes for each token, is
[`wsscore/README.md`](../wsscore/README.md).

| Group | Values |
| --- | --- |
| HTTP answer from the edge or sidecar | `ws_upgrade`, `http_401`, `http_403`, `http_421`, `rate_limited`, `http_502`, `http_503`, `http_other` |
| WebSocket negotiation | `ws_subprotocol` |
| Name resolution | `dns_bogon`, `dns_failure` |
| Local intent | `cancelled` |
| Socket outcome | `connection_refused`, `network_unreachable`, `connection_reset`, `tls_reset`, `response_reset` |
| TLS outcome | `tls_not_tls`, `cert_expired`, `cert_verify`, `tls_alert`, `tls_handshake` |
| Timeout, by handshake phase | `tcp_timeout`, `tls_timeout`, `response_timeout`, `handshake_timeout` |
| Nothing matched | `unclassified` |

The set is closed and frozen. Clients project the native reason through an
explicit literal allowlist and degrade any unrecognized value to
`wss_transport_failed`, so an unexpected string can never reach telemetry.
`unknown` on a wss handshake row identifies a pre-taxonomy client build, and the
`unclassified` share is the taxonomy's own coverage meter — the two are
deliberately different tokens.

`failure_detail` is intentionally absent from these rows. The token is the
detail, and free text would re-leak what the classifier just destroyed:
underlying error strings embed the CloudFront distribution hostname, resolved
edge addresses, and certificate subjects.

`connect_trigger` accompanies `transport_failed` and `connection_succeeded` **on
the mobile clients only** with a closed three-value set naming which path
started the connect cycle. The desktop client does not emit it today: desktop
rows (those carrying an `os` attribute rather than mobile's `os_name`) have no
`connect_trigger`, so a query must not assume its presence outside the mobile
cohort.

| Value | Meaning |
| --- | --- |
| `initial` | A user- or service-initiated connect. |
| `recovery` | The WSS monitor re-running the ladder after a session ended. |
| `network_epoch` | A connectivity-change reconnect. |

#### Per-user access attributes

Clients that carry the access flow (connectcore 0.7.1 and later) stamp two
attributes on their terminal connect events, so the broker can see how the
per-user access path went without a dedicated event. Both are closed token
sets, pinned by `connectcore/contract/vectors/access_outcome.json`; the
dashboard's "Per-user access" panel groups by them.

`access_outcome` rides on `connection_succeeded` and `connection_failed` and
names what the connect did about per-user access. It is absent when the
connect ended before the relay fetch decided (a local setup failure, a cancel
during discovery) and on clients that predate it.

| Value | Meaning |
| --- | --- |
| `granted` | The access answer was the candidate list the connect used. |
| `no_key` | The installation has no key; the public list was used without asking. |
| `not_requested` | A key exists but the target cannot be expressed to the access endpoint (a label-only target); the public list was used. |
| `unsupported` | The broker has no user/access endpoints; the public list was used. |
| `front_unbound` | The reachable front cannot carry an authenticated request (the no-SNI mode); the public list was used. |
| `denied` | The broker refused the installation (`403`); the connect failed with `failure_stage` `access_denied`. |
| `unapplied` | Leases were issued but no relay installed them within the bounded retries (`503`); the public list was used. |
| `rate_limited` | The access request was rate limited (`429`); the public list was used. |
| `empty` | The answer named no relay the ladder could use for the target; the public list was used. |
| `error` | Any other failure of the access request; the public list was used. |
| `fallback_public` | Access was granted, every leased candidate failed remotely, and a public-list relay carried the connect. |
| `enrollment_refused` | The broker refused to enroll this not-yet-enrolled installation from the current network (`403` with code `datacenter_source`); the public list was used. Clients from connectcore 0.8.0. |

`relay_source` rides on `connection_succeeded` only: `access` when the
connected relay's descriptor and credential came from the access answer (a
per-user lease), `public` when they came from the public list (the shared
credential). The share of `access` among successes is the adoption measure
enforcement waits on; every value other than `granted`/`fallback_public` in
`access_outcome` is a connect the public list carried and enforcement would
have failed.

#### Reading these rows

Aggregate before reading: per country, ASN, front, and app-version **shares** —
never per-`client_id` sequences. `client_id` is a permanent installation
identifier, and every one of these rows already pairs it with a client IP, geo,
and a censorship interaction, so a per-client trace is a surveillance artifact
rather than an analysis. Any future dashboard panel over these tokens must
enforce a minimum cell count (k-anonymity floor) before it ships.

Two rules keep the numbers honest:

- **Never compare failure rates across `app_version`.** Older builds re-run the
  entire connect ladder every time an idle WSS session expires, so their
  denominator is inflated by reconnects that cluster exactly where a benign
  timeout is most likely. Compare failure-mode shares *within* an app-version
  cohort, and use `connect_trigger` to separate first connects from recovery
  reconnects — that attribute is the durable control for this confound (mobile
  rows only; desktop rows do not carry it).
- Expect a one-time distribution shift when the taxonomy ships: failures that
  raced a concurrent close used to be recorded as `cancelled` (or as `unknown`
  on Android). Attribution is now error-first, so those rows re-materialize with
  their causal token. This is a recording-semantics change, not a network
  change.

There is no `sni_mode` attribute, by design. Whether a dial omitted SNI is
derivable operator-side by joining the already-transmitted `front_id` against
the signed relay list — which therefore has to be archived daily for any
retrospective analysis to survive front rotation.

## Telemetry dashboard

Set `OPENRUNG_DASHBOARD_TOKEN` on the broker to enable the administrator
dashboard at:

```http
GET /admin/dashboard
```

The dashboard requires the configured token (sign-in at
`/admin/dashboard/login`) and creates an HttpOnly administrator session lasting
12 hours. A left sidebar switches between the Overview and Relays pages and
names the running broker version; the top bar picks the one-hour, 24-hour,
seven-day, 14-day, or 31-day window (carried in the URL as `?window=`) and an
optional auto-refresh. The JSONL file remains the durable telemetry store; the
broker retains the most recent seven days in memory for dashboard queries,
capped at a 64 MiB budget that discards the oldest records first if a burst of
telemetry would exceed it.

The authenticated dashboard reads its data from:

```http
GET /admin/api/dashboard/overview?window=24h
```

Valid windows are `1h`, `24h`, `7d`, `14d`, and `31d`. The trend is bucketed by
hour up to `7d` and by UTC day beyond. Raw events are kept for seven days, so
only the Postgres telemetry store serves the two longer windows in full: its
dashboard rollup (one row per session plus hourly event counts) is kept for 31
days, and sessions older than seven days keep their counts but lose their
client IPs, city, ISP, organization, and ASN, which then render blank. Top
applications cover at most the last seven days. The JSONL store answers every
window with the seven days it retains. When the dashboard token is unset, all
dashboard routes return 404. Production deployments should use HTTPS directly
or through a reverse proxy.

The overview embeds only the 25 most recent sessions; the full session list for
a window is paged through:

```http
GET /admin/api/dashboard/sessions?window=24h&offset=0&limit=25
```

`offset` is the zero-based position into the window's sessions ordered newest
first, and `limit` accepts 1 to 100 (default 25). The response reports `total`
alongside the page so clients can render pagination controls; an offset past
the end returns an empty page rather than an error.

Recent session entries include the Android API level, city, ISP, organization,
ASN, and — when the client reports traffic counters — `bytes_sent` and
`bytes_received` for the session. The overview also includes top-city and
top-ISP rankings counted by unique session.

Every view that names a relay colours the name by the relay's broker-attested
class — bright green for `foundation`, orange for `volunteer` — across the
top-relay, speed-test, and active-by-relay rankings and the relay column of
recent sessions. A visible `FND` or `VOL` marker and accessible label repeat the
class on every coloured name; the full class also appears in its hover tooltip.
When an accepted event names an active relay, the broker records that relay's
attested class with the event, so it remains available after the live lease
expires. The class rides alongside the label as `node_class` (and
`relay_node_class` on session entries); a relay with no operator label falls
back to its ID but is still coloured. Rankings that are not relay-keyed (city,
country, ISP, OS, application) carry no class and render uncoloured.

The session `source_ip` prefers the broker-observed pre-tunnel `client_seen`
address, then the client's pre-tunnel `client_ip` attribute. The source address
of later telemetry uploads is used only as a fallback because connected uploads
can reach the broker through a relay.

Dashboard totals include active clients and active sessions. Active-session
breakdowns are available by relay, country, city, ISP, and operating system.

### Relays page

The sidebar links to a dedicated relay-fleet page at `/admin/dashboard/relays`,
backed by:

```http
GET /admin/api/dashboard/relays?window=24h
```

The response merges the live relay registry with the window's telemetry. Fleet
totals count the currently registered relays overall and by class
(`foundation_relays` / `volunteer_relays`), their advertised session capacity,
transports (direct vs tunnel), and punch/WSS capabilities, alongside
telemetry-derived numbers: relays seen in the window that are no longer
registered (`offline_relays`), live sessions across the fleet, and the distinct
count of clients currently connected through any relay.

Each relay row carries the registry half for online relays — label, class,
public endpoint, access address and operator contact (when the relay registered them), transport, exit location (city/country as resolved by the
broker), relay software version, capacity, punch/WSS capability, registration
and last-heartbeat times — merged with per-relay telemetry for the window:
currently connected clients and active sessions, unique clients and sessions,
successful and failed attempts with the modal failure reason, and average
speed-test results. Relays that appear only in telemetry are listed as offline
rows so a crashed or expired relay stays visible for the retention window; they
keep the node class retained with their telemetry. Rows order online first,
busiest first.

Rows require broker attestation, which comes in two equally trusted forms: a
currently registered relay ID (its telemetry always counts, even when every
window event was received during a lease gap), or — for telemetry-only
offline IDs — at least one window event stamped with the relay's node class
at ingestion, which happens solely while the named relay holds a live
registration. The anonymous telemetry API accepts arbitrary `relay_id`
strings, so unattested IDs are aggregated nowhere on this page — they can
neither mint offline rows nor inflate the connected-clients count. As a
second bound, the response lists at most 200 offline rows (the busiest); the
totals still count every attested offline relay, and the page notes when rows
were hidden.

### Users page

The sidebar's Users page (`/admin/dashboard/users`) is the operator's view of
[client users](#client-users-spec-openrung-user-auth-v1) and their
[access leases](#relay-lease-sync), backed by:

```http
GET /admin/api/dashboard/users?status=&sort=last_auth&offset=0&limit=50
GET /admin/api/dashboard/users/{ref}
PUT /admin/api/dashboard/users/{id}/status
```

The list takes the same parameters and returns the same rows as the
[operational list](#operational-user-endpoints). `{ref}` on the detail route is
a user ID, a lease label as relays log it (`lease-<id>`), or a bare lease ID,
which resolves to the lease's owner (`matched_lease` names it), so an operator
can go from a relay log line to the user. The detail carries the user's row
(status timeline and lease counters), its 50 most recently created retained
leases with each lease's status (`live`, `expired`, `revoked`) and the relay's
label while the relay is registered, `leases_total`, and
`lease_retention_days` (leases are deleted seven days after they expire).
Unknown, malformed and retention-expired references are `404`.

The status route is the same implementation as the operational
`PUT /admin/api/users/{id}/status` — same body, validation and response —
behind the dashboard session instead of the token, with the
[dashboard mutation guards](#dashboard-weight-endpoints) (`415` without a JSON
content type, `403` cross-origin). Each change is logged on the one
`user status changed` line, whose `via` field names the door (`dashboard` or
`operational api`). The page requires a reason and a confirmation before it
disables a user, and states that disabling stops new connections within
seconds (live leases are revoked and relays drop them on their next sync) but
does not terminate an established session. Responses never include public
keys, lease credentials, client source addresses or telemetry identifiers.
Without client users configured (`OPENRUNG_USER_TOKEN_KEY`) these routes
answer `503`.

## Operational relay inventory

```http
GET /admin/api/relays/inventory
Authorization: Bearer <OPENRUNG_API_TOKEN>
```

The broker's machine-to-machine view of its own fleet: **every** currently
active relay, for the operator's own tooling (fleet audits, deployment checks,
capacity reports). Unlike `GET /api/v1/relays` this is not a client candidate
page — it is never ranked and never truncated, so "absent from the inventory"
means the relay's lease has expired, not that it ranked below a page cut.

`OPENRUNG_API_TOKEN` enables the endpoint. When it is unset the route is not
registered at all and every request returns 404, so an unconfigured broker
cannot even be probed for it. The token is a dedicated credential: the broker
refuses to start when it equals the volunteer, foundation, or dashboard token,
or either signing seed. Requests are rate limited per client IP like the public
endpoints — a leaked token must not become an amplification lever against the
broker's database — and answer `429 Too Many Requests` with `Retry-After` over
budget. A missing or invalid bearer gets `401`; a method other than `GET` or
`HEAD` gets `405` with `Allow: GET, HEAD`, but only once the bearer has been
accepted — an anonymous caller sees the same `401` whatever method it uses.
A storage failure gets `503`. Every response the endpoint produces — `200`,
`401`, `405`, `429`, `503` — carries `no-store` in `Cache-Control`; the signed
`200` sends `no-store, no-transform`, like the other signed channels.

Authentication is the bearer token and nothing else. The path sits inside the
dashboard cookie's `/admin` scope, but no dashboard session authorizes it and
this token opens no dashboard page — the two credentials are separate on
purpose, and the broker refuses to start if they are equal.

Call the broker origin directly, not the public CDN front. The front is a
Cloudflare Worker that terminates TLS for every client (`deploy/broker-proxy`),
and it passes unknown paths through with no timeout; there is no reason to hand
a privileged operator credential to an edge, or to hold an edge connection open
for a full-fleet read, when the tooling that holds this token already reaches
the origin.

Relays are ordered by **relay ID**, ascending. Ranking order moves with live
telemetry, so two snapshots taken a minute apart would otherwise differ
everywhere and diff to noise even when the fleet is unchanged. Note the limit
of the join: a relay registered with a stable identity keeps its ID across
reconnects, but a legacy identity-less registration mints a fresh random ID
each time, so those rows do move between snapshots.

Successful responses are signed exactly like the relay list — the same
`X-OpenRung-Relays-Signature` header and the same Ed25519 key — but carry
`channel` `"inventory"` and no `limit` field: an
inventory body is not request-shaped and is never truncated, so there is
nothing to echo. The distinct channel is what stops an operator snapshot — an
untruncated superset of any client page, in a different order — from being
replayed into a client's API slot. `not_after` is `server_time` + 5
minutes, far tighter than the API channel's 30: a relay lease lives about three
minutes, so a stale inventory describes a fleet that no longer exists.

The relay objects are the public directory descriptors plus operator-only
keys: `ranking_weight` — the effective ranking weight the broker is applying to
that relay right now (`1` unless an override is stored; see
[Relay ranking weights](#relay-ranking-weights)) — and the latest [inbound
reachability probe](#inbound-reachability-probe): `reachable` is `null` until
the broker has probed the relay (tunnel transport, probing off, or not yet),
otherwise `true` or `false`, with `reachability_checked_at` giving the probe
time when there is one. A `false` relay is registered but withheld from
clients. `operator_contact` is the contact the relay registered with and
`access_host` its access address (see [Register Relay](#register-relay));
each is omitted when the relay sent none, and both appear only in this
trailing operator block, never in the descriptor fields the inventory shares
with the public directory. Lease
tokens, stable-identity keys and proofs, hub-observed exit addresses,
telemetry, and signing seeds never appear here; the endpoint exposes fleet
membership, not credentials.

Response:

```json
{
  "count": 23,
  "server_time": "2026-06-09T07:00:00Z",
  "not_after": "2026-06-09T07:05:00Z",
  "key_id": "3097e2dee2cb4a34",
  "channel": "inventory",
  "relays": [
    { "id": "relay_0a1b...", "public_host": "203.0.113.7", "public_port": 443, "node_class": "foundation", "...": "...", "ranking_weight": 1, "reachable": true, "reachability_checked_at": "2026-06-09T06:58:00Z", "operator_contact": "ops@example.org", "access_host": "2001:db8:1::10" }
  ]
}
```

## Relay ranking weights

```http
GET    /admin/api/relays/weights
PUT    /admin/api/relays/{id}/weight
DELETE /admin/api/relays/{id}/weight
Authorization: Bearer <OPENRUNG_API_TOKEN>
```

An operator-set per-relay multiplier on the candidate ranking behind
`GET /api/v1/relays`: a number in `[0, 1]`, default `1`. The broker orders each
response by weighted random sampling, so relays with similar telemetry-derived
scores share the head of the page instead of one taking every client; the
weight multiplies a relay's sampling weight. `1` leaves the ranking untouched,
values below `1` reduce the relay's share of page heads in proportion (`0.5`
halves its odds against an otherwise equal relay), and `0` pins it to the
bottom of every page without delisting it — and excludes the relay from the WSS-candidate
reservation that can otherwise promote a WSS-capable Foundation relay into a
short page's last slot (weights above `0` keep that eligibility). Weights key on
the relay ID, so they survive lease expiry and re-registration, and are never
pruned with descriptors.

These routes share the inventory's credential, per-IP rate limit, and
`no-store` posture, and answer like it: `401` for a missing or invalid bearer
before anything else (so an anonymous caller cannot tell the routes exist),
`405` with `Allow` for a wrong method once the bearer is accepted, `429` with
`Retry-After` over budget, `503` on a storage failure. The `{id}` must be a
well-formed relay ID (`relay_` plus 32 lowercase hex characters); anything else
is `400`. The relay need not be currently registered — an override may be set
on a relay that is between leases and applies when it returns.

`GET /admin/api/relays/weights` lists every stored override in relay-ID order:

```json
{
  "generated_at": "2026-09-16T07:00:00Z",
  "default_weight": 1,
  "count": 1,
  "weights": [ { "relay_id": "relay_0a1b...", "weight": 0.5 } ]
}
```

`PUT /admin/api/relays/{id}/weight` stores one. The body must be exactly one
JSON object with a numeric `weight` and no other keys; a missing body, a
non-JSON body, extra keys, trailing data, or a value outside `[0, 1]` is `400`.
`DELETE` restores the default and is idempotent. Both answer with the new
effective weight and the effective weight it replaced:

```json
{ "relay_id": "relay_0a1b...", "weight": 0.5, "previous_weight": 1 }
```

Every change through this API or the dashboard is logged at info level with
the relay ID, old and new weight, and the caller's IP.

### Dashboard weight endpoints

The relays page (`/admin/dashboard/relays`) edits the same weights through the
dashboard session cookie rather than the operational token:

```http
PUT    /admin/api/dashboard/relays/{id}/weight
DELETE /admin/api/dashboard/relays/{id}/weight
```

Validation and responses match the operational routes. Because the session is
a cookie, both mutations additionally require `Content-Type: application/json`
(`415` otherwise — an HTML form cannot send it) and must be same-origin
(`403` otherwise): the browser's `Sec-Fetch-Site` header decides when present
(`same-origin` passes; `cross-site`, `same-site`, and `none` are refused), and
only in its absence is the `Origin` header's host compared with `Host`. Fetch
metadata is used first because a CDN front rewrites `Host` to the origin
hostname, which would make the `Origin` comparison refuse every legitimate
request through the front. Together with the cookie's `SameSite=Strict` this
keeps a visited page from moving the dial. Each row of `GET /admin/api/dashboard/relays` carries the
relay's effective `ranking_weight`, offline rows included.

## Health

```http
GET /healthz
```

Response:

```json
{
  "ok": true,
  "signing_key_id": "3097e2dee2cb4a34",
  "wss_ticket_key_id": "7dfe918ebc0d45b4"
}
```

`signing_key_id` identifies the active relay-list signing key (see List Relays)
so a monitor can assert the expected key is live without parsing a relay body;
it is public data that already ships in every relay-list response.
`wss_ticket_key_id` is present only when WSS ticket issuance is configured and
identifies the broker's active ticket signer, allowing operators to confirm the
matching public key is installed on relay-local sidecars.
`source_classification_available` is present only when datacenter source
classification is configured (see Client users): `false` while its data is
missing or older than `OPENRUNG_SOURCE_ASN_MAX_AGE`, when new enrollments are
answered with `503`. It does not affect `ok` or the status code.

When the broker uses PostgreSQL relay state, `/healthz` also verifies database
connectivity. If relay state is unavailable, it returns `503` with an error
payload so a load balancer can stop routing to that broker instance.

## Register Relay

```http
POST /api/v1/relays/register
Authorization: Bearer <registration-token>
Content-Type: application/json
```

This is the only supported relay registration route. It registers both
`volunteer`-class and `foundation`-class relays.

Endpoint and free-form fields are validated: `public_host`, `server_name`, and
(for tunnel transport) `exit_host` must be syntactically valid DNS hostnames
or IP literals, and the remaining string fields are length-capped and must not
contain control characters. These values are served verbatim to every client
in the signed relay list, so free-form strings are not storable as relay
endpoints.

Beyond the short-window per-IP rate limit, the broker caps how many **new**
relay identities one source IP (IPv6: one /64) may successfully register per
rolling 24 hours — by default 64, tunable with
`OPENRUNG_MAX_NEW_RELAY_IDS_PER_IP_PER_DAY` (a positive integer, or `off` to
disable). Re-registrations that re-prove an already-known identity never
count, so stable-identity relays and tunnel relays reconnecting through the
hub are unaffected, and the budget is only consumed by registrations the
broker accepts, so a relay crash-looping on a rejected request cannot exhaust
it. Requests over the cap receive `429` with a `Retry-After` header, which the
relay runtime honors with backoff. A direct relay whose advertised endpoint
the broker cannot connect to is refused with `422` before anything is stored
(see [Inbound reachability probe](#inbound-reachability-probe)); that
refusal does not consume the cap either. Sources listed in
`OPENRUNG_REGISTRATION_CAP_EXEMPT_CIDRS` (comma-separated CIDRs) are exempt —
set this to the relay hub's egress addresses so a wave of first-time tunnel
volunteers is never throttled — and requests presenting the foundation token
bypass the cap. Without this cap, an anonymous-registration broker would let a
single address mint thousands of attested relay identities per hour, each of
which becomes a public directory row and a telemetry subject the dashboards
must aggregate.

Request:

```json
{
  "public_host": "2001:db8::1",
  "public_port": 443,
  "protocol": "vless-reality-vision",
  "client_id": "2c08df10-4ef4-4ab9-95c6-cb1e94cdb2ff",
  "reality_public_key": "xray-public-key",
  "short_id": "5f7a8d9c01ab23cd",
  "server_name": "www.cloudflare.com",
  "flow": "xtls-rprx-vision",
  "exit_mode": "direct",
  "max_sessions": 75,
  "max_mbps": 100,
  "relay_version": "relay/0.1.0",
  "transport": "direct",
  "node_class": "volunteer",
  "access_leases": true,
  "operator_contact": "ops@example.org",
  "access_host": "2001:db8:1::10"
}
```

`operator_contact` (optional) is the email address the relay's operator asked
to be reached at about the relay. It must be at most 128 printable ASCII
characters without whitespace, with exactly one `@`, a non-empty local part and
a domain containing a dot that neither starts nor ends it; anything else is
refused with `400`.
Registrations without it are accepted unchanged. The broker stores it with the
registration and shows it only in the [operational relay
inventory](#operational-relay-inventory) and the admin relays page: it is not
bound by the identity statement and never appears in the registration response,
relay lists, or any signed client-facing body.

`access_host` (optional) is an IPv6 address, distinct from `public_host`, on
which the relay serves the same listener. It must be a bare global unicast
IPv6 literal (no brackets, zone or IPv4-mapped form, not unique-local), may not
name the same address as `public_host`, and is accepted for direct transport
only; anything else is refused with `400`. It is honoured only for
identity-bearing registrations (a legacy identityless registration's is
dropped) and is replaced, or cleared, by every re-registration; heartbeats
keep it. It is not bound by the identity statement. The broker uses it in one
place: the [client access](#client-access) answer to a request that arrived
over IPv6 names it as the relay's `public_host`. The registration response, relay lists, the [inbound
reachability probe](#inbound-reachability-probe) and every other surface keep
`public_host`; only the operator inventory and the admin relays page show the
access address. The reference relay sends it (`OPENRUNG_ACCESS_HOST`) only after
confirming the address is configured on a host interface; see
`deploy/relay/README.md`.

`access_leases` (optional, default `false`) declares that the relay installs
broker-issued per-client credentials through the [relay lease
sync](#relay-lease-sync) endpoints. It is a registration-time capability
only — heartbeats never carry it and the identity statement does not bind it
— and it is honoured only for identity-bearing registrations, because the sync
endpoints authenticate with the registration's `lease_token`. The response
echoes the recorded value as `access_leases` (absent when `false`); a relay
starts its lease syncer only when the echo is present, so a broker that
predates leases leaves the relay serving the shared credential alone. The
capability is private registration state and never appears in relay lists.

### Inbound reachability probe

A direct-transport relay registers and heartbeats over connections it opens
itself, which proves nothing about whether its advertised
`public_host:public_port` accepts connections. The broker therefore dials that
endpoint itself (a bounded plain TCP connect, no Reality handshake) in the
registration request, before anything is stored. The relay listens before it
registers, so a failed connect means the endpoint is not accepting connections
from the broker's network, and the registration is **refused**: `422` with
the body
`{"error": "public endpoint unreachable: the broker could not connect to <host>:<port>; ..."}`.
Nothing is stored and no `Retry-After` is sent: the relay's own registration
retries (seconds apart at startup, then rounds backed off to a minute) re-run
the probe, so the relay admits itself as soon as the port opens and logs the
refusal within seconds of starting; and `deploy/relay/volunteer-up.sh` reads that log line to tell the
operator which firewall to open. The leading phrase `public endpoint
unreachable` is therefore a contract. A successful probe registers the relay
and reports `"reachable": true` in the response (absent when the broker did
not probe).

Heartbeats re-probe once the stored result is older than the probe interval,
so a relay that registered reachable and then went dark is **withheld from
[`GET /api/v1/relays`](#list-relays) and [`POST
/api/v1/access`](#client-access)** until a later probe succeeds; it stays
registered, heartbeats and syncs leases normally, and the operator views show
the result (see [inventory](#operational-relay-inventory)). A repaired relay
is relisted without re-registering. Only direct transport is probed; a tunnel relay's advertised
endpoint is the relay hub. A probe the broker could not attempt — no route or
local address for the endpoint's address family, a transient resolver
failure — is inconclusive and changes nothing.

The broker dials only endpoints a client on the public internet could reach
itself: the host is resolved first, and an address that is loopback, private,
link-local, carrier-grade NAT, multicast, documentation or otherwise
non-routable space — or one of the broker host's own — is never dialled. Such
a registration is inconclusive (unprobed and listed, as before), so
registering an endpoint can never reveal whether a port on or near the broker
accepts connections.

`OPENRUNG_RELAY_PROBE` switches the probe `off` (default `on`);
`OPENRUNG_RELAY_PROBE_TIMEOUT` bounds one dial (default `5s`) and
`OPENRUNG_RELAY_PROBE_INTERVAL` is how old a result may be before a heartbeat
re-probes (default `5m`). With the probe off every relay is listed on its
lease alone: results a previous run stored are ignored (they still show in
the inventory), so switching it off relists every withheld relay at once.

`relay_version` identifies the relay runtime artifact, independently of the
broker, relay hub, and client app versions. Standalone images report values such
as `relay/0.1.0`; the packaged volunteer app reports values such as
`desktop-volunteer/0.1.0`. Wire compatibility is negotiated separately through
the tunnel and punch protocol versions and capability flags. During the v1
migration, registration also accepts the old `volunteer_version` name, and
descriptor responses include it as a deprecated alias for released clients that
still require it. New integrations must use `relay_version`; the compatibility
alias is omitted from the examples.

`transport` is optional and defaults to `direct`. The relay hub registers CGNAT
volunteer-run relays with `transport: "tunnel"` and a
`public_host`/`public_port` pointing at the hub; clients treat both the same.

`node_class` records who operates the relay: `volunteer` (the default when
omitted) for community-run hardware, or `foundation` for relays the OpenRung
Foundation runs itself. It is provenance, not a quality score. A `foundation`
claim is only accepted when the request presents the broker's
`OPENRUNG_FOUNDATION_TOKEN` as its bearer token; any other credential claiming
`foundation` is rejected with `403` (fail loudly, never silently downgrade).
The foundation token bounds the claimable class without forcing it — its holder
may still register `volunteer` relays, but routine volunteer-class relay and hub
traffic should use the volunteer token so the privileged bearer stays out of
the hub path. On the shipped relay executable (`cmd/relay`), presenting
`OPENRUNG_FOUNDATION_TOKEN` is self-contained: it **forces** direct mode,
overriding any `auto`/`tunnel` setting, so operators do not configure the mode
themselves and the bearer never reaches a hub probe. (The hub path would receive
the registration bearer, and the shipped hub always registers the tunneled exit
operator as `volunteer`, so a Foundation-operated hub does not elevate the
volunteer-run relays behind it.) The class is served back inside the signed
relay-list body, so clients receive it with the same Ed25519 authenticity as
every other descriptor field.

Because `public_host`/`public_port` is client-supplied and foundation
endpoints are public in the signed list, the broker also protects a live
foundation relay's directory entry: a registration at a
`public_host:public_port` currently held by a `foundation` relay is rejected
with `403` unless it is itself a foundation-class registration (which requires
the token). Without this, an anonymous registrant could otherwise seize a
foundation relay's row — new id, its own keys, downgraded class — and force
the real relay into a re-registration race. A foundation operator re-registers
(refreshes) its own endpoint normally with the token. The same rule guards
heartbeats: extending a `foundation` relay's lease requires the foundation
token, so an orphaned foundation row expires from the origin store within one
lease TTL.

Foundation registrations must be sent over TLS: the foundation token is
high-value, and the relay client refuses to transmit it over a cleartext
`http://` broker URL (loopback excepted for local testing). It also refuses all
broker redirects in Foundation mode, preventing an HTTPS request from following
a downgrade. The plaintext broker origin used by volunteer-run relays —
reachable because Cloudflare challenges datacenter IPs on the TLS front — is
therefore volunteer-only.

For tunnel registrations the hub also sends `exit_host`: the volunteer-run
relay's public source IP as observed on its control connection, i.e. where
tunneled traffic actually exits. The broker uses it only to geolocate the relay
and never exposes it through any public endpoint, so the relay host's observed
exit IP stays private. `exit_host` is rejected for direct transport, where
`public_host` already is the exit.

### Stable relay identity (spec `openrung-relay-identity-v1`)

By default the broker mints a fresh random `relay_id` on every registration,
so a relay's ID churns whenever it restarts or the broker forgets its lease —
fragmenting dashboard history and ranking state across orphaned IDs. A relay
may instead prove possession of a long-lived Ed25519 identity key by adding
three fields to the registration:

```json
{
  "identity_public_key": "<base64 raw 32-byte Ed25519 public key>",
  "identity_proof": "<base64 Ed25519 signature>",
  "identity_expires_at": "2026-07-21T12:00:00Z"
}
```

The broker then derives the relay ID from the key — `relay_` + lowercase hex
of the first 16 bytes of SHA-256 over `"openrung-relay-identity-v1:id:"` plus
the raw key — so the same key always yields the same ID, with no broker-side
state. The proof signs a canonical newline-joined statement binding: the spec
tag, `identity_expires_at`, `transport`, the VLESS/Reality parameters
(`client_id`, `reality_public_key`, `short_id`, `server_name`, `flow`,
`exit_mode`), `max_sessions`, `max_mbps`, `label`, `node_class`, and — for
direct transport only — `public_host`/`public_port`. It deliberately does not
bind fields the relay cannot know or that the hub sets: the tunnel endpoint,
`punch_capable`/`punch_endpoint`, `exit_host`, and `relay_version`.

What this buys, and its limits: a captured proof cannot be re-signed with a
different identity key (the ID is the key), used after `identity_expires_at`,
dated more than 48 hours out, or — for a direct relay — pointed at a different
endpoint. A captured **tunnel** proof, however, can within its lifetime
re-register the same relay ID at an attacker-chosen endpoint, because the
tunnel statement binds no endpoint (the relay signs before the hub assigns
one). It cannot serve traffic, since the bound `reality_public_key`'s private
half never leaves the real relay, so the result is denial rather than
interception.

That denial cannot be perpetuated by the real relay's heartbeats. Every
successful identity-bearing registration mints a fresh, unlisted
`lease_token`, and a heartbeat for that stable ID must present the token for
the current registration. A replay (or stale tunnel session) rotates the
token; the live relay or hub's next heartbeat is rejected as not found, which
drives it to re-register its genuine request and restore the endpoint. While
the genuine relay or hub remains connected, one replay therefore buys at most
a transient overwrite; sustaining the denial requires repeated registration
replays until the proof expires. Passive capture requires observable
registration traffic (the plaintext volunteer broker origin); the TLS front
and origin-TLS close that path.

An already-authorized stale hub session is a separate case: TLS does not remove
credentials the hub legitimately holds. The hub checks local registry
ownership before re-registering and retires a stale session once a newer one
owns the ID. A narrow overlap with publication of the new session can still
briefly overwrite its endpoint, but the new session's next token-scoped
heartbeat restores it. The hub also recycles a tunnel session for a fresh
HELLO when the broker answers `401` `relay identity proof expired`.

All three fields travel together; a malformed, tampered, or expired proof
fails the registration with `401` — never a silent fall back to a random ID.
Requests without the fields keep the legacy random-ID behavior and do not
require a lease token on heartbeat. The identity public key and lease token
are never served through any public relay-list endpoint.

The identity grants nothing beyond ID continuity: node class is still attested
per registration by the token, the foundation endpoint guard applies
unchanged, and a valid proof for a different key confers no authority over an
endpoint. When an identity re-registers from a new endpoint, its previous row
is atomically abandoned (one row per identity). The shipped relay pins the key
with `-identity-seed` / `OPENRUNG_IDENTITY_SEED` (base64 32-byte seed, minted
per instance by the deploy scripts), the desktop volunteer app persists it in
`identity.json`, and an unpinned relay generates one per process — which still
keeps the ID stable across broker outages within the process lifetime.

### Relay-owned WSS capability (spec `openrung-relay-wss-capability-v1`)

An eligible relay advertises its own CDN fronts by adding all three capability
fields to its Foundation registration:

```json
{
  "node_class": "foundation",
  "transport": "direct",
  "public_port": 443,
  "wss_fronts": [
    {
      "id": "iran-a",
      "url": "wss://d111111abcdef8.cloudfront.net/api/v1/wss-bridge",
      "protocol_version": 1
    },
    {
      "id": "iran-b",
      "url": "wss://d222222abcdef8.cloudfront.net/api/v1/wss-bridge",
      "protocol_version": 1
    }
  ],
  "wss_capability_proof": "<base64 Ed25519 signature>",
  "wss_capability_expires_at": "2026-07-21T12:00:00Z"
}
```

The fields are all-or-nothing. WSS is accepted only for a direct-transport,
direct-exit Foundation relay on public port 443 that supplies a valid stable
identity proof. `wss_capability_expires_at` must equal `identity_expires_at`, be
in the future, and be no more than 48 hours ahead. The capability proof uses
the same Ed25519 identity key as the stable relay identity but a separate
domain-separated statement, so adding the capability does not change the
deployed `openrung-relay-identity-v1` bytes:

```text
openrung-relay-wss-capability-v1
<wss_capability_expires_at>
<unpadded standard-base64 canonical capability JSON>
```

The canonical JSON has exactly `identity_statement_sha256` (unpadded
standard-base64 SHA-256 of the identity statement) and `fronts` (the complete
normalized list). `wss_capability_proof` is standard-base64 of the 64-byte
Ed25519 signature over the three newline-separated lines above, with no
trailing newline. The proof therefore binds the exact relay identity and exact
ordered front set; changing a front ID, URL, protocol version, or expiry
invalidates it.

There may be at most four fronts. Front IDs are normalized lowercase strings of
1–64 letters, digits, `.`, `_`, or `-`, beginning and ending alphanumeric, and
the list is sorted by ID with no duplicate ID or URL. Each URL must use `wss`
on its default port, contain a multi-label DNS name rather than an IP literal,
use the exact path `/api/v1/wss-bridge`, and contain no userinfo, query, escaped
path, or fragment. Each CDN origin must terminate at the relay that advertised
it; neither the broker nor the relay-local sidecar accepts a client-selected
destination.

The broker verifies the proof before storing the descriptor. Invalid,
incomplete, expired, or non-canonical capabilities fail registration rather
than silently dropping WSS. If the broker has no WSS ticket signer configured,
a registration advertising fronts returns `503`. The private capability proof
and expiry are not published. Only `wss_fronts` is copied into registration and
directory responses, where the ordinary detached relay-list signature covers
the relay ID, Foundation class, endpoint, Reality parameters, and advertised
fronts together.

Response:

```json
{
  "id": "relay_...",
  "lease_token": "lease_<opaque random token>",
  "access_leases": true,
  "public_host": "2001:db8::1",
  "public_port": 443,
  "city": "Tokyo",
  "country": "Japan",
  "country_code": "JP",
  "latitude": 35.6895,
  "longitude": 139.6917,
  "node_class": "volunteer",
  "protocol": "vless-reality-vision",
  "client_id": "2c08df10-4ef4-4ab9-95c6-cb1e94cdb2ff",
  "reality_public_key": "xray-public-key",
  "short_id": "5f7a8d9c01ab23cd",
  "server_name": "www.cloudflare.com",
  "flow": "xtls-rprx-vision",
  "exit_mode": "direct",
  "max_sessions": 75,
  "max_mbps": 100,
  "relay_version": "relay/0.1.0",
  "registered_at": "2026-06-09T07:00:00Z",
  "last_heartbeat_at": "2026-06-09T07:00:00Z",
  "expires_at": "2026-06-09T07:03:00Z"
}
```

`city`, `country`, and `country_code` are resolved by the broker (never taken
from the relay's registration request) so clients can show where a relay's
traffic
physically exits: from `exit_host` for tunnel relays and from `public_host`
for direct relays. The lookup is best-effort: when it has not succeeded yet
the fields are omitted, and the broker retries on heartbeats until it
resolves.

## Heartbeat

```http
POST /api/v1/relays/{id}/heartbeat
Authorization: Bearer <registration-token>

{
  "ok": true,
  "lease_token": "lease_<token from registration>",
  "client_id": "<optional replacement VLESS credential>"
}
```

Response:

```json
{
  "ok": true,
  "expires_at": "2026-06-09T07:03:30Z",
  "client_id": "<the credential the directory now serves>"
}
```

`client_id` is optional on the request. When present it replaces the VLESS
credential the relay list serves for this relay, in the same authorized write
that renews the lease (at most 128 characters, opaque to the broker exactly as
at registration). Only an identity-bearing registration presenting its valid
`lease_token` can change it: a legacy identityless relay renews without a
token, and relay IDs are public, so a credential on such a heartbeat is
ignored (the response reports the unchanged one). A relay that rotates its credential announces each new value
this way and keeps the previous one accepted until the response echoes the
successor, so a directory snapshot fetched just before a rotation stays
usable for its whole `not_after` window. A heartbeat without `client_id`
leaves the served credential unchanged, which is what relays and hubs that
predate rotation send.

Any registration credential (volunteer token, anonymous on an open broker, or
the foundation token) may heartbeat a volunteer-class relay. Extending a
**foundation** relay's lease additionally requires the foundation token;
anything weaker gets `403` and the lease is not extended. Relay IDs are public
in the relay list, so this guard is what stops a weaker credential from either
keeping an orphaned foundation label alive (e.g. after an endpoint takeover
through a pre-`node_class` broker binary) or interfering with a foundation
relay: a refused heartbeat changes nothing, and an unattended foundation row
expires from the origin store within one lease TTL.

A heartbeat also re-runs the [inbound reachability
probe](#inbound-reachability-probe) of a direct relay's endpoint once the
stored result is older than `OPENRUNG_RELAY_PROBE_INTERVAL`; the heartbeat
response is unaffected, but the outcome decides whether the relay is listed.

An identity-bearing registration also requires its current `lease_token` on
heartbeat. The broker rotates this opaque token on every successful
registration and returns `404 relay not found` for a missing or superseded
token, causing the relay or hub to re-register. Legacy random-ID registrations
may omit it for rolling compatibility. The token is registration-private and
must not be logged or copied into relay-list data.

The one-TTL bound applies to the live origin row, not to copies already signed.
An ordinary API snapshot has a 30-minute `not_after` window. On an origin
failure, the edge Worker can serve its last healthy API response for up to 15
minutes, but that response retains its original `not_after`. Clients and local
fallback caches must stop using snapshots once `not_after` plus the protocol's
bounded clock-skew
allowance has elapsed. Until then a snapshot can still show the earlier
`foundation` provenance, so removing the origin row is not an instant
client-visible revocation mechanism.

## Relay lease sync

```http
GET /api/v1/relays/{id}/leases
GET /api/v1/relays/{id}/leases?after=<revision>
POST /api/v1/relays/{id}/leases/ack
Authorization: Bearer <lease_token from registration>
```

Access leases are broker-issued, short-lived VLESS credentials for one client
installation on one relay (ADR-006). A relay that registered with
`access_leases` keeps its xray in step with the broker's desired set through
these two endpoints. Both authenticate with the registration's `lease_token`
— not the registration token — and both answer `404 relay not found` for a
missing or superseded token, an unknown relay, or a legacy identityless
registration, so the relay's ordinary heartbeat re-registration path takes
over and the next poll uses the new token.

The poll returns the relay's **full** desired set and its revision in one
consistent snapshot:

```json
{
  "server_time": "2026-10-16T12:00:00Z",
  "revision": 42,
  "leases": [
    {
      "id": "0123456789abcdef0123456789abcdef",
      "credential": "2c08df10-4ef4-4ab9-95c6-cb1e94cdb2ff",
      "expires_at": "2026-10-16T13:00:00Z"
    }
  ]
}
```

`revision` is a per-relay counter that advances by one on every change to the
set — a lease issued, renewed, revoked or evicted — and every lease carries
the revision of its last change, so a set served at revision *N* contains
exactly the live leases changed at or before *N*. Leases never carry user
identity; relays only ever see the lease ID, which they use as the xray user
label `lease-<id>`. Expiry is relative: a relay removes a lease at
`now + (expires_at − server_time)` on its own clock, so a clock offset between
broker and relay never shifts the window, and a lease the broker never revoked
still ends on time through a broker outage.

Without `after` the broker answers immediately, even with an empty set at
revision `0` — the first poll of a session, which the relay acknowledges
before it goes online so nothing is ever released against a set it has not
installed. With `after=N` (the revision the relay last applied) the broker
holds the request for up to 20 s until its revision exceeds *N*, then answers;
when nothing changes within the hold it answers with the unchanged set and the
relay simply polls again, so an idle relay never busy-loops. `after` must be a
non-negative integer (`400` otherwise).

The ack body is `{"revision": 42}` and the response is `204`. A relay
acknowledges a revision only after every xray add and remove for that set
succeeded; a partial result is retried with a fresh full poll instead. The
broker records the highest acknowledged revision per relay and only ever
raises it. A registration that mints a new `lease_token` (first registration,
endpoint move, re-registration after the lease expired) resets the
acknowledged revision to `0` while keeping the revision counter, so the
relay's next full poll re-installs every live lease — leases outlive relay
descriptors, and so does the per-relay revision state.

Issuance is bounded: a user holds at most `OPENRUNG_MAX_LEASES_PER_USER`
(default 10) live leases — at the cap the least recently renewed leases
outside the current request are revoked to make room — and a relay holds at
most `OPENRUNG_MAX_LEASES_PER_RELAY` (default 2000); a relay at its cap is
skipped. Leases live for `OPENRUNG_ACCESS_LEASE_TTL` (default `1h`, between
`1m` and `24h`); renewal extends the same credential rather than minting a
new one. Expired and revoked leases stay resolvable through the operational
API for seven days.

### Operational lease lookup

```http
GET /admin/api/leases/{id}
Authorization: Bearer <OPENRUNG_API_TOKEN>
```

Resolves a relay-log label to its lease. `{id}` is the bare lease ID or the
label as logged (`lease-<id>`). The view never includes the credential:

```json
{
  "lease_id": "0123456789abcdef0123456789abcdef",
  "label": "lease-0123456789abcdef0123456789abcdef",
  "user_id": "user_0123456789abcdef0123456789abcdef",
  "relay_id": "relay_0123456789abcdef0123456789abcdef",
  "status": "live",
  "created_at": "2026-10-16T12:00:00Z",
  "renewed_at": "2026-10-16T12:30:00Z",
  "expires_at": "2026-10-16T13:30:00Z",
  "revision": 42
}
```

`status` is `live`, `expired` or `revoked` (with `revoked_at`). Disabling the
user through `PUT /admin/api/users/{id}/status` revokes every live lease the
user holds in the same transaction and wakes the affected relays' polls, so
the credentials are removed within one relay sync. Unknown, malformed and
retention-expired IDs are `404`; the route shares the operational API's
limiter and exists when `OPENRUNG_API_TOKEN` is configured.

## List Relays

```http
GET /api/v1/relays?limit=5
```

The order of `relays` is the broker's candidate ranking. A direct relay whose
latest [inbound reachability probe](#inbound-reachability-probe) failed is
left out before ranking and page shaping. The broker uses recent
connection successes/failures, active sessions, observed client latency, and
speed-test telemetry. The latency it scores is aggregated across all clients,
so it cannot describe any one client's path. The order is drawn per response by
weighted random sampling without replacement, with each relay's weight rising
steeply with its score: a clearly better relay leads almost every response,
while relays with similar scores take turns at the head, so concurrent clients
spread across them rather than all landing on the marginal leader. IPv6 is only
a final tie-breaker after the sampled order and heartbeat recency. `limit`
defaults to `OPENRUNG_RELAY_LIST_DEFAULT_LIMIT` and may not exceed
`OPENRUNG_RELAY_LIST_MAX_LIMIT` (larger values are `400`); the operator keeps
the maximum at or above every shipped client's request. `limit`
truncates *after* ranking, so
membership of the returned set — unlike the order within it — is decided
entirely by the broker.

Clients should filter unusable descriptors without reordering the broker-ranked
list. A client may reorder on a signal the broker cannot observe — notably the
connecting client's own network path — provided broker order is preserved among
candidates that measure comparably (for example, within one latency bucket), so
the broker's load-balancing term still decides between them. Ranking must
reorder, never exclude: a client must not drop a relay on a local measurement.

The PostgreSQL relay-state schema also keeps JSONB escape hatches for fields
that are still experimental: `relay_descriptors.attributes`,
`relay_sessions.attributes`, and `relay_metrics.measurements`. Public API fields
remain explicit columns/JSON properties until an experimental field graduates.

Every `2xx` relay-list response is signed: the header

```http
X-OpenRung-Relays-Signature: ed25519;<key_id>;<base64 signature>
```

carries a detached Ed25519 signature over the exact raw body bytes, so clients
can authenticate the directory even on non-TLS channels (the direct-IP
fallback). Signing covers channel integrity only — a censor can
still block or inject errors, which clients treat as a failed candidate. Error
responses are never signed. The signed body carries its own freshness and
shape: `not_after` (`server_time` + 30 minutes on this channel), `key_id`
(lowercase hex of the first 8 bytes of SHA-256 over the raw 32-byte public key,
advisory routing between pinned keys), `channel` (`"api"` here), and `limit`
(the effective request limit echoed back so a signed body cannot be replayed
for a differently-shaped request). Responses are sent with
`Cache-Control: no-store, no-transform` and an explicit `Content-Length`.
All timestamp strings in the signed directory body use UTC RFC 3339 at
whole-second precision (for example, `2026-06-09T07:00:00Z`); the broker keeps
subsecond precision internally but does not expose fractional seconds here.

Response:

```json
{
  "count": 1,
  "server_time": "2026-06-09T07:00:00Z",
  "not_after": "2026-06-09T07:30:00Z",
  "key_id": "3097e2dee2cb4a34",
  "channel": "api",
  "limit": 5,
  "relays": [
    {
      "id": "relay_...",
      "public_host": "2001:db8::1",
      "public_port": 443,
      "city": "Tokyo",
      "country": "Japan",
      "country_code": "JP",
      "latitude": 35.6895,
      "longitude": 139.6917,
      "node_class": "volunteer",
      "protocol": "vless-reality-vision",
      "client_id": "2c08df10-4ef4-4ab9-95c6-cb1e94cdb2ff",
      "reality_public_key": "xray-public-key",
      "short_id": "5f7a8d9c01ab23cd",
      "server_name": "www.cloudflare.com",
      "flow": "xtls-rprx-vision",
      "exit_mode": "direct",
      "max_sessions": 75,
      "max_mbps": 100,
      "relay_version": "relay/0.1.0",
      "registered_at": "2026-06-09T07:00:00Z",
      "last_heartbeat_at": "2026-06-09T07:00:00Z",
      "expires_at": "2026-06-09T07:03:00Z"
    }
  ]
}
```

`node_class` (`volunteer` or `foundation`, see Register Relay) is
broker-attested and covered by the list signature. Clients written before the
field existed ignore it; clients that read it must treat a missing value as
`volunteer` and must not relax any signature or transport verification for
`foundation` relays — the class is operator provenance, not a trust bypass.

An eligible Foundation descriptor also contains its relay-owned capability:

```json
"wss_fronts": [
  {
    "id": "iran-a",
    "url": "wss://d111111abcdef8.cloudfront.net/api/v1/wss-bridge",
    "protocol_version": 1
  }
]
```

Clients must use fronts only from a fresh, verified directory response. A front
belongs to the descriptor containing it; clients must never copy a URL to
another relay. Unknown protocol versions are ignored, preserving direct
Reality compatibility for older clients and newer front protocols.

## Client users (spec `openrung-user-auth-v1`)

Every client installation can hold a P-256 ECDSA keypair and authenticate it
to the broker. The broker mints a random, stable user ID for a public key the
first time it sees it and returns the same ID on every later authentication,
so there is no separate enrollment step. Operators can disable a user; a
disabled user still authenticates (so the client learns its state) but
receives no access token, and every later authorization refuses it. These
endpoints exist only when `OPENRUNG_USER_TOKEN_KEY` is configured.

User IDs are deliberately independent of telemetry identifiers: clients must
not send `X-OpenRung-Client-ID` / `X-OpenRung-Session-ID` on these requests,
and the broker never logs public keys, challenges, signatures or tokens.

### Challenge

```http
POST /api/v1/users/challenge
```

Returns `201 Created` with a single-use challenge valid for one minute:

```json
{
  "challenge": "Zm9yLWV4YW1wbGUtb25seS1ub3QtcmVhbC1kYXRh",
  "expires_at": "2026-10-16T12:01:00Z"
}
```

Challenges live in broker memory. A broker restart forgets outstanding
challenges; the client then simply requests another. Under extreme load the
endpoint answers `503` with `Retry-After` rather than growing without bound.

### Authenticate

```http
POST /api/v1/users/auth
Content-Type: application/json

{
  "public_key": "<standard base64 of the SubjectPublicKeyInfo DER of a P-256 key>",
  "challenge": "<the challenge exactly as issued>",
  "signature": "<standard base64 of the raw 64-byte r‖s ECDSA signature>"
}
```

The signature covers the SHA-256 digest of the newline-joined statement

```
openrung-user-auth-v1
<challenge>
<canonical public key>
```

where the canonical public key is the standard base64 of the 65-byte
uncompressed point. Every SPKI encoding of one key therefore names one user.
The body must be exactly one JSON object with no unknown fields (8 KiB cap).
The challenge is consumed before the signature is checked: each challenge
buys exactly one verification attempt.

Success is `200 OK`:

```json
{
  "user_id": "user_0123456789abcdef0123456789abcdef",
  "status": "enabled",
  "access_token": "<opaque bearer token>",
  "token_expires_at": "2026-10-17T12:00:00Z"
}
```

The access token is opaque to clients and authenticates later control-plane
requests for 24 hours by default (`OPENRUNG_ACCESS_TOKEN_TTL`); it must be
sent only as an `Authorization: Bearer` header over HTTPS and never logged.
Rotating `OPENRUNG_USER_TOKEN_KEY` invalidates every outstanding token at
once, and clients re-authenticate.

| Status | Meaning |
| --- | --- |
| `400` | Malformed body, unknown field, unparseable or non-P-256 key, invalid challenge text |
| `401` | `challenge unknown or expired`, or `invalid signature` |
| `403` | The user is disabled; the body carries `user_id` and `status` but no token |
| `403` + `"code": "datacenter_source"` | A new identity from a datacenter or hosting network (see below); nothing was stored or charged |
| `429` | Enrollment budget exhausted for this source; `Retry-After` says when the window resets |
| `503` + `"code": "source_unclassified"` + `Retry-After` | A new identity could not be classified because the broker's classification data is missing or stale; retry later |
| `503` | Store unavailable |

**Enrollment budget.** Only a *new* identity is charged, after its signature
verifies, against two fixed windows per source (the IPv4 address, or the IPv6
/64): `OPENRUNG_ENROLLMENT_BURST_LIMIT` per hour (default 3) and
`OPENRUNG_ENROLLMENT_DAILY_LIMIT` per 24 hours (default 10). A refused
attempt is never charged, and existing keys authenticate freely. A negative
limit disables that window. Clients behind one shared address share a budget,
so a client should surface the `Retry-After` rather than retry immediately.

**Datacenter sources.** When the operator configures source classification
(`OPENRUNG_SOURCE_ASN_DB` and `OPENRUNG_DATACENTER_ASN_LIST`, see
`deploy/broker/README.md`), a *new* identity whose source address lies in a
range announced by a listed datacenter or hosting ASN is refused before the
enrollment budget is consulted:

```json
{"error": "enrollment is not available from this network", "code": "datacenter_source"}
```

The classification is a lookup in local files the operator refreshes; no
request leaves the broker. Addresses the data does not cover are not
datacenter sources, and `OPENRUNG_ENROLLMENT_EXEMPT_CIDRS` are never refused.
An existing key authenticates from any network, so an installation enrolled
once keeps working through another VPN or a hosting provider. While the data
is missing or older than `OPENRUNG_SOURCE_ASN_MAX_AGE` (default 7 days), a new
identity receives `503` with `Retry-After: 300` and the code
`source_unclassified` instead of being admitted or refused on stale
information. Without the two files configured nothing changes. The `code`
field is the machine-readable part of these bodies: clients should map it to
their own explanation (connectcore raises the `enrollment_refused` notice and
records the `enrollment_refused` access outcome) rather than show the
`error` text. The broker counts both refusals in its ten-minute access
summary line and logs nothing about the individual request.

Later authentication of an existing key only updates its `last_auth_at`;
nothing else about the record changes, and a disabled identity stays disabled.

### Operational user endpoints

```http
GET /admin/api/users/{id}
PUT /admin/api/users/{id}/status
Authorization: Bearer <OPENRUNG_API_TOKEN>
```

`GET` returns the operator view — never the public key:

```json
{
  "user_id": "user_0123456789abcdef0123456789abcdef",
  "status": "disabled",
  "created_at": "2026-10-16T12:00:00Z",
  "last_auth_at": "2026-10-16T14:00:00Z",
  "disabled_at": "2026-10-16T15:00:00Z",
  "disabled_reason": "abuse report"
}
```

`PUT` takes exactly one JSON object `{"status": "disabled", "reason": "…"}`
(or `"enabled"`, which clears the reason) and answers with the updated view.
Disabling also revokes every live [access lease](#relay-lease-sync) the user
holds; enabling revokes nothing.
The reason is at most 256 characters of printable text. Every change is
logged with the caller's IP. Unknown or malformed IDs are `404`; the routes
share the operational API's limiter and exist only when both
`OPENRUNG_API_TOKEN` and `OPENRUNG_USER_TOKEN_KEY` are configured.

```http
GET /admin/api/users?status=disabled&sort=leases_24h&offset=0&limit=50
Authorization: Bearer <OPENRUNG_API_TOKEN>
```

Lists users one page at a time. `status` (`enabled` or `disabled`) filters;
`sort` is `last_auth` (default: most recently authenticated first),
`leases_24h` (most leases issued in the last 24 hours first) or `enrolled`
(newest first), with the user ID as the final tiebreak; `limit` is 1–100
(default 50) and `offset` is zero or more. Invalid parameters are `400`.
Each row is the operator view above plus three counters derived from access
leases — the broker keeps no per-user traffic volume, so lease activity is the
only volume signal:

```json
{
  "generated_at": "2026-10-16T16:00:00Z",
  "sort": "leases_24h",
  "total": 1,
  "offset": 0,
  "limit": 50,
  "users": [
    {
      "user_id": "user_0123456789abcdef0123456789abcdef",
      "status": "enabled",
      "created_at": "2026-10-16T12:00:00Z",
      "last_auth_at": "2026-10-16T15:55:00Z",
      "live_leases": 2,
      "leases_issued_24h": 6,
      "relays_24h": 3
    }
  ]
}
```

`live_leases` counts leases neither revoked nor expired, `leases_issued_24h`
counts leases created in the last 24 hours (a renewal extends a lease rather
than issuing one), and `relays_24h` counts distinct relays among leases
issued or renewed in the last 24 hours. `total` counts every user matching the
filter. The route shares the limiter and the configuration requirements above.

## Client access

```http
POST /api/v1/access
Authorization: Bearer <access_token>
Content-Type: application/json

{"limit": 5, "relay_id": "relay_0123456789abcdef0123456789abcdef"}
```

An authenticated user asks for relays to connect to and receives one
per-user VLESS credential (an access lease) on each. The body is exactly one
JSON object with no unknown fields: `limit` (1–5, default 5), and at most one
target — `relay_id` (one exact relay) or `country` (a two-letter code, matched
case-insensitively against `country_code`). No target asks for the
best-ranked relays.

The broker ranks relays exactly as for `GET /api/v1/relays`, keeps only relays
that can install per-user credentials (identity-bearing, lease-capable,
direct-transport registrations whose latest [inbound reachability
probe](#inbound-reachability-probe) did not fail), narrows them to the target, reserves the WSS
slot like the public list, and then issues or extends one lease per relay. It
waits up to five seconds for each relay to acknowledge the lease through its
[lease sync](#relay-lease-sync) and answers with the relays that did.

**Acknowledgement.** The answer is whatever subset acknowledged in time: a
relay that stays silent beside one that acknowledged is simply left out. When
no relay acknowledged, the broker re-reads the relays it is waiting for. One
whose last heartbeat is still within the relay heartbeat interval is alive and
merely slow, so the wait runs once more (one extension; a request therefore
takes at most ten seconds). A relay that has missed a heartbeat is treated as
gone and the answer is `503` at once, so the client's retry or fallback is not
delayed by a relay that will never install the lease. A relay that has left
the directory altogether is not a candidate at all: a request naming it as
`relay_id` gets `200` with an empty list (nothing is issued).

**Empty answers.** `200` with `"relays": []` means no lease-capable relay
matches the request right now. Clients treat it by context: at connect time
the shared public list is used instead (until enforcement), and at renewal
time it means the connected relay is gone, so the client reconnects through
its ladder immediately rather than retrying a dead relay until the lease
expires. The broker logs one `INFO` summary of access answers per ten-minute
window (counts by outcome: granted, empty, unapplied, extended, denied,
unauthorized, rate limited, malformed, cancelled, errors; plus the user
authentications that refused a new identity from a datacenter source or for
want of classification data), so access health
remains readable from the broker log after the origin access log has rolled
over; a request its client abandoned mid-flight is logged at `INFO`, never as
an error.

Success is `200 OK`, signed exactly like a relay list (same key, same
`X-OpenRung-Relays-Signature` header, same wire schema) but on the `access`
channel, without `limit`, and with the lease expiry:

```json
{
  "count": 1,
  "server_time": "2026-10-17T12:00:00Z",
  "not_after": "2026-10-17T12:30:00Z",
  "key_id": "<key id>",
  "channel": "access",
  "lease_expires_at": "2026-10-17T13:00:00Z",
  "relays": [{ "id": "relay_…", "client_id": "<this user's lease credential>", "…": "…" }]
}
```

Each descriptor's `client_id` is the caller's credential for that relay; it
stops being accepted at `lease_expires_at` (compare with `server_time`, not the
local clock). For a relay that registered an `access_host`, and only when the
access request itself arrived over IPv6 (as seen by the broker, through a
trusted front the viewer's address), the descriptor's `public_host` is that
access address instead of the address the public relay list carries. A request
over IPv4 gets the public host. Every other field is the relay's public
descriptor either way. Clients must verify the signature and require
`channel == "access"`; a public list never verifies here and an access body
never verifies as a public list.

**Renewal.** A connected client repeats the request with the connected relay
as `relay_id` well before expiry (about half-way). The broker extends the
existing lease and returns the same `client_id` with a later
`lease_expires_at`, so a running tunnel never needs a new credential.

| Status | Meaning |
| --- | --- |
| `200` with `"relays": []` | No relay matches the target — including a `relay_id` that has expired from the directory — or every match is at its lease capacity; nothing was issued |
| `400` | Malformed body, unknown field, `limit` out of range, both targets, invalid country |
| `401` | Missing, invalid or expired access token, or an unknown user: authenticate again |
| `403` | The user is disabled |
| `429` | Rate limit (per client address and user) |
| `503` + `Retry-After: 2` | Leases were issued but no relay acknowledged them in time (after one extended wait if a relay was still heartbeating); the retry returns the same credentials |

Access requests carry no telemetry identifiers, never follow redirects, and are
refused by clients on endpoint-unbound fronts (the Azure no-SNI mode); the
route exists only when `OPENRUNG_USER_TOKEN_KEY` is configured and the store
holds leases. Each user holds at most `OPENRUNG_MAX_LEASES_PER_USER` live
leases; past it the least recently renewed leases outside the request are
revoked first.

## Issue WSS Session Ticket

```http
POST /api/v1/wss/tickets
Content-Type: application/json
Cache-Control: no-store

{
  "relay_id": "relay_0123456789abcdef0123456789abcdef",
  "front_id": "iran-a"
}
```

This unauthenticated control-plane endpoint issues one short-lived, single-use
ticket for the exact live relay and exact front named in the request. The
broker looks up `front_id` only inside that relay's current signed capability;
the request cannot supply a URL or destination. The relay must still be a live,
direct-mode, direct-exit Foundation relay on public port 443 with a stable
identity. Unknown, expired, ineligible, or mismatched relay/front pairs return
the same `404` shape so the endpoint does not resolve arbitrary front IDs.

Success is `201 Created` with cache prevention headers:

```json
{
  "ticket": "v1.<key-id>.<claims>.<signature>",
  "expires_at": "2026-07-21T11:32:00Z",
  "url": "wss://d111111abcdef8.cloudfront.net/api/v1/wss-bridge"
}
```

The returned URL is copied from the same live descriptor; the client must
compare it with the selected front in its verified directory response. The
ticket's Ed25519-authenticated claims contain the version, fixed audience,
unique ticket ID, exact relay ID, exact front ID, issue/not-before/expiry times,
and maximum stream count. They contain no host, port, Reality target, or
browsing destination. The default broker ticket lifetime is two minutes,
capped by the relay's remaining lease and the five-minute protocol maximum;
the default stream limit is 64 and the protocol maximum is 1,024. The
relay-local sidecar atomically consumes the unique ticket ID, rejects replay,
requires the ticket relay ID to equal its configured local relay ID, and
requires the front ID to match the authenticated CDN origin token.

The client sends the opaque ticket only in the WebSocket upgrade:

```http
GET /api/v1/wss-bridge HTTP/1.1
Authorization: Bearer <ticket>
Sec-WebSocket-Protocol: openrung-wss-bridge-v1
```

Tickets must not appear in URLs, query parameters, cookies, subprotocol values,
logs, telemetry, or error text. Ticket acquisition must use HTTPS (except the
explicit loopback development allowance), must reject every redirect, and must
never forward the POST to a redirect target. Desktop clients try configured
broker fronts within one bounded operation deadline; network failures and
eligible transient responses advance to the next front. `Retry-After` is
honored only when valid and no greater than both the configured maximum wait
and the remaining overall deadline. A ticket, broker-front, CDN, or WSS failure
is fallback-local and must not reduce the selected relay's direct health rank.

The endpoint exists only when `OPENRUNG_WSS_TICKET_SIGNING_SEED` is configured.
`400` reports malformed or incomplete JSON, `409` asks the client to refresh a
relay whose lease is too close to expiry, `429` includes a bounded
`Retry-After`, and `503` reports temporary store failure. This fallback is
implemented by the desktop client in this repository and by the Android and
iOS clients in their separate mobile repository. Its opaque WebSocket and
multiplexing mechanics come from the shared, independently tagged
`github.com/openrung/openrung/wsscore` module. The hardened HTTPS ticket
exchange comes from the independently tagged
`github.com/openrung/openrung/brokerapi` module; broker-front scheduling and
relay-health policy remain application concerns. Mobile releases pin reviewed
module tags and ship independently; publishing a shared-module tag alone does
not update an installed app.
