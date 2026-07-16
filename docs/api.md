# Broker API

All API paths are currently versioned under `/api/v1`.

## Abuse limits

The unauthenticated endpoints (`GET /api/v1/relays`, `POST
/api/v1/telemetry/events`, `GET /api/v1/speed-test`) are rate limited per
client IP. Requests over the budget receive `429 Too Many Requests` with a
`Retry-After` header (seconds); clients should back off and retry. The budgets
are far above normal client behavior — relay-list polling, telemetry batching,
and an occasional speed test never hit them — and they apply to the real client
IP resolved through trusted proxies, so one abusive source behind Cloudflare
does not exhaust anyone else's budget.

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
record the real client IP. Trusted proxies default to Cloudflare's published ranges;
set `OPENRUNG_TRUSTED_PROXY_CIDRS` (comma-separated CIDRs) to add more. Forwarded
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
      "destination_ip": "142.250.191.142",
      "destination_port": 443,
      "protocol": "tcp"
    }
  ]
}
```

The endpoint accepts at most 200 events and 512 KiB per request. Events dated
more than one hour into the future are rejected (retention and dashboards key
off the server's receipt time, so client clocks cannot extend either), and
free-form fields are length-capped: attribute/measurement keys at 64
characters, attribute values and `application_package` at 256. Package
attribution is collected on Android 10 and newer. This first implementation
records connection starts and destinations, not per-flow byte counters or flow
completion times.

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

## Telemetry dashboard

Set `OPENRUNG_DASHBOARD_TOKEN` on the broker to enable the administrator
dashboard at:

```http
GET /admin/telemetry
```

The dashboard requires the configured token, creates an HttpOnly administrator
session lasting 12 hours, and refreshes its operational overview every 30
seconds. It supports one-hour, 24-hour, and seven-day windows. The JSONL file
remains the durable telemetry store; the broker retains the most recent seven
days in memory for dashboard queries, capped at a 64 MiB budget that discards
the oldest records first if a burst of telemetry would exceed it.

The authenticated dashboard reads its data from:

```http
GET /admin/api/telemetry/overview?window=24h
```

Valid windows are `1h`, `24h`, and `7d`. When the dashboard token is unset, all
dashboard routes return 404. Production deployments should use HTTPS directly
or through a reverse proxy.

The overview embeds only the 25 most recent sessions; the full session list for
a window is paged through:

```http
GET /admin/api/telemetry/sessions?window=24h&offset=0&limit=25
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

## Health

```http
GET /healthz
```

Response:

```json
{
  "ok": true,
  "signing_key_id": "3097e2dee2cb4a34"
}
```

`signing_key_id` identifies the active relay-list signing key (see List Relays)
so a monitor can assert the expected key is live without parsing a relay body;
it is public data that already ships in every relay-list response.

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
  "max_sessions": 8,
  "max_mbps": 20,
  "relay_version": "dev",
  "transport": "direct",
  "node_class": "volunteer"
}
```

`relay_version` reports the runtime version for both `volunteer`-class and
`foundation`-class relays. During the v1 migration, registration also accepts
the old `volunteer_version` name, and descriptor responses include it as a
deprecated alias for released clients that still require it. New integrations
must use `relay_version`; the compatibility alias is omitted from the examples.

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

Response:

```json
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
  "max_sessions": 8,
  "max_mbps": 20,
  "relay_version": "dev",
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
```

Response:

```json
{
  "ok": true,
  "expires_at": "2026-06-09T07:03:30Z"
}
```

Any registration credential (volunteer token, anonymous on an open broker, or
the foundation token) may heartbeat a volunteer-class relay. Extending a
**foundation** relay's lease additionally requires the foundation token;
anything weaker gets `403` and the lease is not extended. Relay IDs are public
in the relay list, so this guard is what stops a weaker credential from either
keeping an orphaned foundation label alive (e.g. after an endpoint takeover
through a pre-`node_class` broker binary) or interfering with a foundation
relay: a refused heartbeat changes nothing, and an unattended foundation row
expires from the origin store within one lease TTL.

The one-TTL bound applies to the live origin row, not to copies already signed.
An ordinary API snapshot has a 30-minute `not_after` window. On an origin
failure, the edge Worker can serve its last healthy API response for up to 15
minutes, but that response retains its original `not_after`; a signed
static-mirror response has a 24-hour window. Clients and local fallback caches
must stop using snapshots once `not_after` plus the protocol's bounded clock-skew
allowance has elapsed. Until then a snapshot can still show the earlier
`foundation` provenance, so removing the origin row is not an instant
client-visible revocation mechanism.

## List Relays

```http
GET /api/v1/relays?limit=5
```

The order of `relays` is the broker's candidate ranking. In global ranking mode,
the broker uses recent active sessions, connection successes/failures, observed
client latency, and speed-test telemetry. The latency it scores is aggregated
across all clients, so it cannot describe any one client's path. IPv6 is only a
final tie-breaker after score and heartbeat recency. `limit` truncates *after*
ranking, so membership of the returned set — unlike the order within it — is
decided entirely by the broker.

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
fallback, static mirrors). Signing covers channel integrity only — a censor can
still block or inject errors, which clients treat as a failed candidate. Error
responses are never signed. The signed body carries its own freshness and
shape: `not_after` (`server_time` + 30 minutes on this channel), `key_id`
(lowercase hex of the first 8 bytes of SHA-256 over the raw 32-byte public key,
advisory routing between pinned keys), `channel` (`"api"` here), and `limit`
(the effective request limit echoed back so a signed body cannot be replayed
for a differently-shaped request). Responses are sent with
`Cache-Control: no-store, no-transform` and an explicit `Content-Length`.

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
      "max_sessions": 8,
      "max_mbps": 20,
      "relay_version": "dev",
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

## Mirror Relay List

```http
GET /api/v1/relays.mirror
```

The mirror-channel relay list: the full directory page (the API's maximum page
size) signed exactly like `GET /api/v1/relays`, but with `channel` set to
`"mirror"`, `not_after` set to `server_time` + 24 hours, and no `limit` field —
the mirror body is not request-shaped, so there is nothing to echo. An hourly
cron on the broker host fetches this endpoint and publishes the exact body
bytes (`relays.json`) plus the signature header value (`relays.json.sig`) to
static mirrors; clients try mirrors only after every API candidate fails and
check `channel` so a long-lived mirror artifact can never be replayed into an
API slot.
