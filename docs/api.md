# Broker API

All API paths are currently versioned under `/api/v1`.

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
returning the relay list. The default broker command stores telemetry as
append-only JSON Lines in `openrung-telemetry.jsonl`; change the path with
`-telemetry-file`.

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

The endpoint accepts at most 200 events and 512 KiB per request. Package
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

The JSONL sink updates the dashboard's in-memory records immediately, flushes
buffered JSONL output every five seconds, syncs it to durable storage every 30
seconds, and performs a final flush and sync during graceful broker shutdown.

The Settings screen offers a manual volunteer speed test. It downloads from the
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
rejects requests larger than 25 MB.

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
days in memory for dashboard queries.

The authenticated dashboard reads its data from:

```http
GET /admin/api/telemetry/overview?window=24h
```

Valid windows are `1h`, `24h`, and `7d`. When the dashboard token is unset, all
dashboard routes return 404. Production deployments should use HTTPS directly
or through a reverse proxy.

Recent session entries include the Android API level, city, ISP, organization,
and ASN when those attributes are present in client telemetry. The overview also
includes top-city and top-ISP rankings counted by unique session.

The session `source_ip` prefers the broker-observed pre-tunnel `client_seen`
address, then the client's pre-tunnel `client_ip` attribute. The source address
of later telemetry uploads is used only as a fallback because connected uploads
can reach the broker through a volunteer relay.

Dashboard totals include active clients and active sessions. Active-session
breakdowns are available by relay, country, city, ISP, and operating system.

## Health

```http
GET /healthz
```

Response:

```json
{
  "ok": true
}
```

When the broker uses PostgreSQL relay state, `/healthz` also verifies database
connectivity. If relay state is unavailable, it returns `503` with an error
payload so a load balancer can stop routing to that broker instance.

## Register Volunteer

```http
POST /api/v1/volunteers/register
Authorization: Bearer <registration-token>
Content-Type: application/json
```

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
  "volunteer_version": "dev",
  "transport": "direct"
}
```

`transport` is optional and defaults to `direct`. The relay hub registers CGNAT
volunteers with `transport: "tunnel"` and a `public_host`/`public_port` pointing
at the hub; clients treat both the same.

Response:

```json
{
  "id": "relay_...",
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
  "volunteer_version": "dev",
  "registered_at": "2026-06-09T07:00:00Z",
  "last_heartbeat_at": "2026-06-09T07:00:00Z",
  "expires_at": "2026-06-09T07:03:00Z"
}
```

## Heartbeat

```http
POST /api/v1/volunteers/{id}/heartbeat
Authorization: Bearer <registration-token>
```

Response:

```json
{
  "ok": true,
  "expires_at": "2026-06-09T07:03:30Z"
}
```

## List Relays

```http
GET /api/v1/relays?limit=5
```

The order of `relays` is the broker's candidate ranking. In global ranking mode,
the broker uses recent active sessions, connection successes/failures, observed
client latency, and speed-test telemetry. IPv6 is only a final tie-breaker after
score and heartbeat recency. Clients should filter unusable descriptors without
reordering the broker-ranked list.

The PostgreSQL relay-state schema also keeps JSONB escape hatches for fields
that are still experimental: `relay_descriptors.attributes`,
`relay_sessions.attributes`, and `relay_metrics.measurements`. Public API fields
remain explicit columns/JSON properties until an experimental field graduates.

Response:

```json
{
  "count": 1,
  "server_time": "2026-06-09T07:00:00Z",
  "relays": [
    {
      "id": "relay_...",
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
      "volunteer_version": "dev",
      "registered_at": "2026-06-09T07:00:00Z",
      "last_heartbeat_at": "2026-06-09T07:00:00Z",
      "expires_at": "2026-06-09T07:03:00Z"
    }
  ]
}
```
